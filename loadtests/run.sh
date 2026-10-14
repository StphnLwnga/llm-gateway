#!/usr/bin/env bash
# Gateway overhead test: k6 straight to the fake provider, then k6 through the
# gateway to the same fake, for each token delay, mode and VU level. Prints p50
# and p95 per token delay.
# Usage: loadtests/run.sh (or make loadtest). Tear down: make loadtest-down.
set -euo pipefail

cd "$(dirname "$0")/.."

TOKENS="${TOKENS:-20}"
TPOTS="${TPOTS:-5ms 0ms}"
WARMUP_S="${WARMUP_S:-10}"
DURATION_S="${DURATION_S:-30}"
VUS_LEVELS="${VUS_LEVELS:-1 10 100}"
RESULTS=loadtests/results

compose() {
	docker compose -p llm-gateway-loadtest -f deploy/compose.yaml -f deploy/compose.loadtest.yaml --profile loadtest "$@"
}

quiet() {
	compose --progress quiet "$@"
}

# ensure_route makes alias point at model. The route is kept across runs; a
# route left by a run with other fake settings would make the two paths do
# different work.
ensure_route() {
	local alias=$1 model=$2 existing id current enabled
	existing="$(quiet run --rm -T gatewayctl routes list | awk -v a="$alias" '$2 == a { print $1, $5, $6 }')"
	if [[ -z "$existing" ]]; then
		quiet run --rm -T gatewayctl routes add --alias "$alias" --provider openai --upstream-model "$model" --priority 10
		return
	fi
	read -r id current enabled <<<"$existing"
	if [[ "$current" != "$model" ]]; then
		echo "route $id for $alias uses $current, not $model; run make loadtest-down first" >&2
		exit 1
	fi
	if [[ "$enabled" != "true" ]]; then
		quiet run --rm -T gatewayctl routes enable "$id"
	fi
}

print_table() {
	local tpot=$1
	echo
	echo "== fake model fake?tokens=${TOKENS}&tpot=${tpot}, ${WARMUP_S}s warm-up, ${DURATION_S}s measured per case"
	cat "$RESULTS/$tpot"-*.tsv | awk -F'\t' -v dur="$DURATION_S" '
		{ key = $2 FS $3; n[key, $1] = $4; p50[key, $1] = $5; p95[key, $1] = $6; err[key, $1] = $8; keys[key] = 1
		  if ($1 == "gateway") tw[key] = $9 }
		END {
			printf "%-10s %5s | %9s %9s %8s %6s | %9s %9s %8s %6s %8s | %9s %9s\n",
				"mode", "VUs", "direct", "", "", "", "gateway", "", "", "", "", "added", ""
			printf "%-10s %5s | %9s %9s %8s %6s | %9s %9s %8s %6s %8s | %9s %9s\n",
				"", "", "p50 ms", "p95 ms", "req/s", "err %", "p50 ms", "p95 ms", "req/s", "err %", "TW peak", "p50 ms", "p95 ms"
			for (key in keys) {
				split(key, k, FS)
				printf "%-10s %5s | %9.2f %9.2f %8.1f %6s | %9.2f %9.2f %8.1f %6s %8s | %9.2f %9.2f\n",
					k[1], k[2],
					p50[key, "direct"], p95[key, "direct"], n[key, "direct"] / dur, err[key, "direct"],
					p50[key, "gateway"], p95[key, "gateway"], n[key, "gateway"] / dur, err[key, "gateway"], tw[key],
					p50[key, "gateway"] - p50[key, "direct"], p95[key, "gateway"] - p95[key, "direct"]
			}
		}' | { IFS= read -r h1; IFS= read -r h2; echo "$h1"; echo "$h2"; sort -k1,1r -k2,2n; }
}

mkdir -p "$RESULTS"
rm -f "$RESULTS"/*.tsv "$RESULTS"/stats.txt

echo "== starting the loadtest stack (project llm-gateway-loadtest)"
compose up --build -d --wait gateway fakeprovider

for tpot in $TPOTS; do
	ensure_route "loadtest-fake-$tpot" "fake?tokens=${TOKENS}&tpot=${tpot}"
done

# docker stats has no peak value, so sample it for the whole run.
containers="$(compose ps -q gateway fakeprovider)"
(
	while true; do
		docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' $containers >>"$RESULTS/stats.txt" 2>/dev/null || true
	done
) &
sampler=$!
trap 'kill "$sampler" 2>/dev/null || true' EXIT

for tpot in $TPOTS; do
	for mode in stream nonstream; do
		for vus in $VUS_LEVELS; do
			# Alternate the paths at each level so any drift in the machine hits both.
			for target in direct gateway; do
				echo "== tpot $tpot, $mode, $vus VUs, $target"
				sockets=""
				if [[ "$target" == "gateway" ]]; then
					sockets="$(quiet run -d --rm --no-deps upstream-sockets)"
				fi
				quiet run --rm -T --no-deps k6 run --quiet \
					-e TARGET="$target" -e MODE="$mode" -e VUS="$vus" -e TOKENS="$TOKENS" -e TPOT="$tpot" \
					-e ALIAS="loadtest-fake-$tpot" -e FAKE_MODEL="fake?tokens=${TOKENS}&tpot=${tpot}" \
					-e WARMUP="${WARMUP_S}s" -e DURATION="${DURATION_S}s" \
					overhead.js >/dev/null
				tw="-"
				if [[ -n "$sockets" ]]; then
					tw="$(docker logs "$sockets" 2>/dev/null | sort -n | tail -1)"
					docker rm -f "$sockets" >/dev/null
				fi
				f="$RESULTS/$tpot-$mode-$vus-$target.tsv"
				printf '%s\t%s\n' "$(cat "$f")" "${tw:--}" >"$f"
			done
		done
	done
done

kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true

echo
echo "== machine: $(uname -sm), $(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown CPU)"
echo "== docker: $(docker info --format '{{.OperatingSystem}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes for containers')"
echo "== TW peak: the most gateway sockets to the fake in TIME_WAIT during the case, sampled every second"
for tpot in $TPOTS; do
	print_table "$tpot"
done

echo
echo "== peak memory and CPU during the run (sampled about every 2s)"
awk -F'\t' '
	function mib(s,   v) {
		v = s + 0
		if (s ~ /GiB/) return v * 1024
		if (s ~ /KiB/) return v / 1024
		if (s ~ /B$/ && s !~ /iB/) return v / 1048576
		return v
	}
	{ split($2, m, " / "); used = mib(m[1]); cpu = $3 + 0
	  if (used > mem[$1]) mem[$1] = used
	  if (cpu > top[$1]) top[$1] = cpu
	  limit[$1] = m[2] }
	END { for (c in mem) printf "%-40s peak %7.1f MiB of %s, peak CPU %.0f%%\n", c, mem[c], limit[c], top[c] }
' "$RESULTS/stats.txt"
for c in gateway fakeprovider; do
	echo "$c: $(docker inspect -f 'OOMKilled={{.State.OOMKilled}} restarts={{.RestartCount}} status={{.State.Status}}' "$(compose ps -q "$c")")"
done
echo
echo "The stack is still up. Tear it down with: make loadtest-down"
