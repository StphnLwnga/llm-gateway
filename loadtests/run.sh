#!/usr/bin/env bash
# Gateway overhead test: k6 straight to the fake provider, then k6 through the
# gateway to the same fake, for each mode and VU level. Prints p50 and p95.
# Usage: loadtests/run.sh (or make loadtest). Tear down: make loadtest-down.
set -euo pipefail

cd "$(dirname "$0")/.."

TOKENS="${TOKENS:-20}"
TPOT="${TPOT:-5ms}"
WARMUP_S="${WARMUP_S:-10}"
DURATION_S="${DURATION_S:-30}"
VUS_LEVELS="${VUS_LEVELS:-1 10 100}"
ALIAS=loadtest-fake
FAKE_MODEL="fake?tokens=${TOKENS}&tpot=${TPOT}"
RESULTS=loadtests/results

compose() {
	docker compose -p llm-gateway-loadtest -f deploy/compose.yaml -f deploy/compose.loadtest.yaml --profile loadtest "$@"
}

quiet() {
	compose --progress quiet "$@"
}

mkdir -p "$RESULTS"
rm -f "$RESULTS"/*.tsv "$RESULTS"/stats.txt

echo "== starting the loadtest stack (project llm-gateway-loadtest)"
compose up --build -d --wait gateway fakeprovider

# The route is kept across runs; a route left by a run with other fake
# settings would make the two paths do different work.
existing="$(quiet run --rm -T migrate routes list | awk -v a="$ALIAS" '$2 == a { print $1, $5, $6 }')"
if [[ -z "$existing" ]]; then
	quiet run --rm -T migrate routes add --alias "$ALIAS" --provider openai \
		--upstream-model "$FAKE_MODEL" --priority 10
else
	read -r id model enabled <<<"$existing"
	if [[ "$model" != "$FAKE_MODEL" ]]; then
		echo "route $id for $ALIAS uses $model, not $FAKE_MODEL; run make loadtest-down first" >&2
		exit 1
	fi
	if [[ "$enabled" != "true" ]]; then
		quiet run --rm -T migrate routes enable "$id"
	fi
fi

# docker stats has no peak value, so sample it for the whole run.
containers="$(compose ps -q gateway fakeprovider)"
(
	while true; do
		docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' $containers >>"$RESULTS/stats.txt" 2>/dev/null || true
	done
) &
sampler=$!
trap 'kill "$sampler" 2>/dev/null || true' EXIT

for mode in stream nonstream; do
	for vus in $VUS_LEVELS; do
		# Alternate the paths at each level so any drift in the machine hits both.
		for target in direct gateway; do
			echo "== $mode, $vus VUs, $target"
			quiet run --rm -T --no-deps k6 run --quiet \
				-e TARGET="$target" -e MODE="$mode" -e VUS="$vus" -e TOKENS="$TOKENS" \
				-e ALIAS="$ALIAS" -e FAKE_MODEL="$FAKE_MODEL" \
				-e WARMUP="${WARMUP_S}s" -e DURATION="${DURATION_S}s" \
				overhead.js >/dev/null
		done
	done
done

kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true

echo
echo "== results: fake model $FAKE_MODEL, ${WARMUP_S}s warm-up, ${DURATION_S}s measured per case"
echo "== machine: $(uname -sm), $(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown CPU)"
echo "== docker: $(docker info --format '{{.OperatingSystem}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes for containers')"
echo
cat "$RESULTS"/*.tsv | awk -F'\t' -v dur="$DURATION_S" '
	{ key = $2 FS $3; n[key, $1] = $4; p50[key, $1] = $5; p95[key, $1] = $6; err[key, $1] = $8; keys[key] = 1 }
	END {
		printf "%-10s %5s | %9s %9s %8s %6s | %9s %9s %8s %6s | %9s %9s\n",
			"mode", "VUs", "direct", "", "", "", "gateway", "", "", "", "added", ""
		printf "%-10s %5s | %9s %9s %8s %6s | %9s %9s %8s %6s | %9s %9s\n",
			"", "", "p50 ms", "p95 ms", "req/s", "err %", "p50 ms", "p95 ms", "req/s", "err %", "p50 ms", "p95 ms"
		for (key in keys) {
			split(key, k, FS)
			printf "%-10s %5s | %9.2f %9.2f %8.1f %6s | %9.2f %9.2f %8.1f %6s | %9.2f %9.2f\n",
				k[1], k[2],
				p50[key, "direct"], p95[key, "direct"], n[key, "direct"] / dur, err[key, "direct"],
				p50[key, "gateway"], p95[key, "gateway"], n[key, "gateway"] / dur, err[key, "gateway"],
				p50[key, "gateway"] - p50[key, "direct"], p95[key, "gateway"] - p95[key, "direct"]
		}
	}' | { IFS= read -r h1; IFS= read -r h2; echo "$h1"; echo "$h2"; sort -k1,1r -k2,2n; }

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
