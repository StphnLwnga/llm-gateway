# Benchmarks

## Gateway overhead

The gateway overhead is the latency that the gateway adds to a request. The test sends the same requests from k6 straight to the fake provider. Then it sends them through the gateway to the same fake provider. The difference between the two is the overhead.

```bash
make loadtest        # build, start the loadtest stack, run all 24 cases, print two tables (about 18 minutes)
make loadtest-down   # remove the loadtest stack and its Postgres volume
```

### Setup

| Part | Detail |
|---|---|
| Script | `make loadtest` runs `loadtests/run.sh`. |
| Stack | A separate Compose project, `llm-gateway-loadtest`, from `deploy/compose.yaml` plus `deploy/compose.loadtest.yaml`. It has its own network and Postgres volume, and publishes no ports. Thus, it can run next to the normal stack. |
| Gateway | Reads no `.env`. `OPENAI_BASE_URL` points at the fake provider, with a placeholder `OPENAI_API_KEY`. The normal stack never points at the fake. |
| Routes | For each token delay, the script adds a route alias (`loadtest-fake-5ms`, `loadtest-fake-0ms`) with `gatewayctl routes add` if it is not there. A second run reuses it. |
| k6 | `grafana/k6:2.3.0`, pinned by digest. It runs as a container on the same Compose network. Both paths cross the same network, so the only difference is the gateway hop. |
| Cases | 2 token delays (5 ms, 0 ms) x 2 paths (direct, gateway) x 2 modes (stream, non-stream) x 3 load levels (1, 10 and 100 virtual users). Each case is a separate k6 run. |
| Order | The direct and gateway runs alternate at each level, so a change in the machine affects both paths. |

### Method

- **Fake settings.** Every case asks for 20 tokens, with no extra delay before the first token.
  - The 5 ms table uses `fake?tokens=20&tpot=5ms`, which is closer to a real model. The fake alone takes about 107 to 110 ms per answer (the direct p50 column). That is 19 gaps of 5 ms, plus the timer overshoot of each sleep. Thus, the overhead is small next to it.
  - The 0 ms table uses `fake?tokens=20&tpot=0ms`. The fake answers at once, so the gateway's own cost is most of the time.
  - `TPOTS` (default `"5ms 0ms"`), `TOKENS` and `VUS_LEVELS` change these settings.
- **Load model.** The test is a closed loop. Each virtual user sends its next request when the previous one ends, with no think time. At 100 users, the request rate depends on the latency.
- **Warm-up.** Each case runs 10 s of warm-up, which is not recorded. Then it runs 30 s of measurement at the same load. `WARMUP_S` and `DURATION_S` change them.
- **The number.** The latency is k6's `http_req_duration` for the full response, from the request sent to the last byte read. k6 reads the whole body. It does not see the SSE events one by one.
- **No time to first byte.** The fake sends stream headers at once. The gateway holds them until the first token (`STATUS_HOLD`). Thus, TTFB measures different events on the two paths, and the tables leave it out.
- **Non-stream through the gateway.** The gateway reads a stream from the fake and joins it. The direct path asks the fake for a non-streamed answer. Both take the same token delays.
- **Errors.** A response counts only if it is `200` and complete.
  - A stream must end with `data: [DONE]` and carry the 20 expected tokens.
  - A non-streamed answer must carry the same text and finish reason `stop`.
  - The error column is the share of responses that fail this check. A case with errors is not a valid result.
- **TW peak.** The most gateway sockets to the fake in `TIME_WAIT` during a case. A sidecar container shares the gateway's network namespace and counts them every second. The count stays near 0 while the connection pool works.
- **Resources.** The script samples `docker stats` for the whole run. It prints the peak memory and CPU of the gateway and the fake provider, and whether either container was killed or restarted. The gateway runs with `mem_limit: 128m` and the fake with `64m`. Neither has a CPU limit. k6, the gateway and the fake share the CPUs of the Docker VM.
- **What is not on the path.** A request does not touch Postgres, because routes come from the in-memory snapshot. Thus, `DB_MAX_CONNS` does not limit this test. The gateway writes one log line per request at `info`, as in normal use.

### Results

Results from the run of 2026-10-12 on the machine below, as `make loadtest` printed them. Each case had a 10 s warm-up and 30 s measured.

Machine: Darwin arm64, Apple M4. Docker Desktop, with 10 CPUs and 8,319,504,384 bytes (7.7 GiB) for containers.

5 ms between tokens (`fake?tokens=20&tpot=5ms`):

| Mode | VUs | Direct p50 (ms) | Direct p95 (ms) | Direct req/s | Direct err % | Gateway p50 (ms) | Gateway p95 (ms) | Gateway req/s | Gateway err % | TW peak | Added p50 (ms) | Added p95 (ms) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| stream | 1 | 109.67 | 113.11 | 9.1 | 0.00 | 111.54 | 115.29 | 8.9 | 0.00 | 0 | 1.87 | 2.18 |
| stream | 10 | 109.53 | 113.87 | 91.0 | 0.00 | 112.65 | 122.01 | 87.5 | 0.00 | 0 | 3.12 | 8.14 |
| stream | 100 | 107.48 | 112.93 | 922.4 | 0.00 | 107.46 | 111.33 | 927.8 | 0.00 | 0 | -0.02 | -1.60 |
| non-stream | 1 | 110.21 | 114.23 | 9.1 | 0.00 | 111.46 | 115.78 | 8.8 | 0.00 | 0 | 1.25 | 1.55 |
| non-stream | 10 | 109.66 | 114.27 | 90.8 | 0.00 | 110.94 | 115.35 | 90.0 | 0.00 | 0 | 1.28 | 1.08 |
| non-stream | 100 | 108.05 | 114.24 | 919.9 | 0.00 | 106.88 | 111.02 | 934.0 | 0.00 | 0 | -1.17 | -3.22 |

0 ms between tokens (`fake?tokens=20&tpot=0ms`):

| Mode | VUs | Direct p50 (ms) | Direct p95 (ms) | Direct req/s | Direct err % | Gateway p50 (ms) | Gateway p95 (ms) | Gateway req/s | Gateway err % | TW peak | Added p50 (ms) | Added p95 (ms) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| stream | 1 | 0.13 | 0.18 | 4759.8 | 0.00 | 0.35 | 0.73 | 2020.9 | 0.00 | 0 | 0.22 | 0.55 |
| stream | 10 | 0.27 | 0.82 | 20192.6 | 0.00 | 1.74 | 3.49 | 4739.5 | 0.00 | 0 | 1.47 | 2.67 |
| stream | 100 | 2.01 | 13.10 | 22017.5 | 0.00 | 12.51 | 29.79 | 6939.7 | 0.00 | 3 | 10.50 | 16.69 |
| non-stream | 1 | 0.06 | 0.08 | 12191.9 | 0.00 | 0.35 | 0.61 | 2595.4 | 0.00 | 0 | 0.29 | 0.53 |
| non-stream | 10 | 0.13 | 0.30 | 53770.0 | 0.00 | 1.01 | 1.94 | 8845.9 | 0.00 | 0 | 0.88 | 1.64 |
| non-stream | 100 | 0.74 | 3.15 | 83428.7 | 0.00 | 6.68 | 15.62 | 13448.3 | 0.00 | 12 | 5.94 | 12.47 |

Peaks for the whole run:

| Container | Peak memory | Peak CPU | OOM kill | Restarts |
|---|---|---|---|---|
| fake provider | 33.3 MiB of 64 MiB | 434% | no | 0 |
| gateway | 69.4 MiB of 128 MiB | 414% | no | 0 |

### Reading the results

- **5 ms between tokens.** The gateway adds 1 to 3 ms p50 at 1 and 10 VUs. At 100 VUs it adds about 0, which is within the timer noise of the fake. A negative value is noise.
- **0 ms, 1 VU.** The gateway's own cost is about 0.2 to 0.3 ms p50 per request.
- **0 ms, 10 and 100 VUs.** Both paths are CPU-bound.
  - k6, the gateway and the fake share the 10 CPUs of the Docker VM. The gateway and the fake each peaked at about 4 cores.
  - Thus, the added latency at 100 VUs is mostly queueing for CPU. It is not the cost per request, and it is not a capacity figure.
  - Little's law agrees with the closed loop: 6,939.7 req/s x 12.51 ms is about 87 requests in flight, against 100 VUs.
- **Stream against non-stream.** A stream costs the gateway about twice as much as a non-streamed answer: 6,940 against 13,448 req/s at 100 VUs.
  - Both read 20 upstream events.
  - A stream also writes and flushes 20 events to the client. A non-streamed answer writes one JSON body.
- **Stream at 10 VUs.** The added p95 was higher than at the other levels in two separate runs (10.73 ms and 8.14 ms). The cause is not known yet.
  - A rerun on 2026-10-14 kept every trace (5 ms between tokens, 10 VUs). The gateway added 2.63 ms p50 and 4.35 ms p95 to a stream, and 1.21 ms and 1.58 ms to a non-streamed answer.
  - The gateway's work outside the GenAI span was 0.77 ms p95 for a stream and 0.49 ms for a non-streamed answer. The rest of the tail is inside the HTTP client span.
  - In a stream, the HTTP client span also holds the write of each event to the client. Thus, a trace cannot split the provider's time from the write time.
- **TW peak 3 and 12.** A few upstream connections closed at 0 ms and 100 VUs. The cause is not yet identified. The count is far below the ephemeral port range.

## Upstream connection pool

The gateway's upstream HTTP client keeps up to 256 idle connections per provider host, and 1024 in total. The Go default is 2 per host.

- Groq and OpenAI negotiate HTTP/2, so their requests share one connection either way.
- An HTTP/1.1 upstream without TLS (Ollama, a self-hosted vLLM, the fake) carries one request per connection.
- With only 2 connections kept idle, most concurrent requests open a new connection. Each closed connection then waits in `TIME_WAIT` on a local port.

Measured 2026-10-12 against the fake provider: 100 virtual users, streamed, 5 ms between tokens, for 5 minutes. The gateway's ephemeral port range is 32768 to 60999, which is 28,232 ports.

| | Go default (2 idle per host) | Pool (256 idle per host) |
|---|---|---|
| Sockets in `TIME_WAIT` | About 28,000 within about 66 s, then between 25,600 and 28,100 | 0 at every sample |
| Requests | 263,200 | 277,234 |
| Errors | 1,028 (0.39%): `502`, from dials that failed with `cannot assign requested address` | 0 |

With the pool, about 100 connections to the fake stayed established for the whole run. In the run with the Go default, the first 10 s are left out. The route had not loaded yet, so those requests got `404`. The `502` responses all came before the gateway sent a status.

## Time to first token on a local model

This test measures how the time to first token (TTFT) of a small local model changes with load. It uses 1, 5 and 20 concurrent requests, with 1 and 4 parallel slots in Ollama. A slot is one request that the model works on at the same time as others. The gateway only passes the stream through, so the numbers describe the model server.

```bash
TRACE_SAMPLE_PERCENT=100 make up-obs      # keep every trace, for the Ollama timings
docker compose -f deploy/compose.yaml run --rm gatewayctl \
  routes add --alias local --provider ollama --upstream-model llama3.2:3b --priority 10
K6=grafana/k6:2.3.0@sha256:9c2dee7f8ed74d317e4027c06a10f169b625638189de8d4555d0b3486a5aeb34
docker run --rm --network llm-gateway_default -v "$PWD/loadtests/k6:/scripts:ro" \
  -e LEVEL=5 -e ROUNDS=4 "$K6" run --quiet /scripts/ollama-ttft.js
```

`LEVEL` is the number of virtual users, and `ROUNDS` the number of requests each one sends in sequence.

### Setup

| Part | Detail |
|---|---|
| Machine | Apple M4 Mac mini, 16 GB. k6, Docker and the observability stack ran on the same Mac as the model. |
| Model server | Ollama 0.40.3 (Ollama.app) on the host, with a context of 4096 tokens. |
| Model | `llama3.2:3b`, Q4_K_M, 2,019,393,189 bytes, on an external drive. |
| k6 | `grafana/k6:2.3.0`, the same image as the overhead test, with `loadtests/k6/ollama-ttft.js`. The `per-vu-iterations` executor gives each virtual user a fixed number of requests. |
| Request | Streamed, a prompt of 243 tokens, `max_tokens` 128, `temperature` 0. Every answer reached 128 tokens. |
| Slots | `OLLAMA_NUM_PARALLEL`. On macOS, quit Ollama, then start it with `open -a Ollama --env OLLAMA_NUM_PARALLEL=4`. `launchctl setenv` did not reach Ollama.app in this test. |

### Method

- **TTFT.** `ttft_ms` from the gateway's request log line: from the request's arrival to the first content written to the client. Every request writes the line, so it is not sampled.
- **Ollama timings.** The model load, prefill and decode times come from the GenAI span (`llm_gateway.provider.*_duration`). Thus, the runs keep every trace.
- **Prefix cache.** Each prompt starts with a unique ID. A prefix cache cannot reuse a prompt whose first token differs, so every request does the full prefill.
- **Load model.** A closed loop: each virtual user sends its next request when the previous one ends. It is not an arrival rate.
- **Model state.** Warm: the model is in memory. Memory-cold: the model is not loaded, and its file is in the macOS page cache. Disk-cold: `sudo purge` empties the page cache, so the load reads the file from the drive.
- **OK.** A request is OK when it is `200` and the stream ends with `data: [DONE]`.

### Results

Measured 2026-10-14 on the machine above.

| Slots | Model state | VUs | TTFT p50 (s) | TTFT max (s) | OK | Notes |
|---|---|---|---|---|---|---|
| 1 | memory-cold | 1 | 1.29 | 4.32 | 5/5 | Load 0.79 s once the file is in the page cache |
| 1 | disk-cold | 1 | 4.34 | 4.61 | 5/5 | Load 3.83 to 4.09 s |
| 1 | memory-cold | 5 | 7.89 | 14.55 | 5/5 | |
| 1 | memory-cold | 20 | 29.2 | 57.6 | 18/20 | 2 first-token watchdog timeouts at 60 s |
| 1 | warm | 1 | 0.45 | 0.47 | 10/10 | Prefill 0.43 s; decode 2.9 s for 128 tokens (44 tokens/s) |
| 1 | warm | 5 | 14.43 | 15.05 | 20/20 | TTFT rises in steps of 3.35 s, one service time |
| 1 | warm | 20 | 58.9 | 59.9 | 36/40 | 4 first-token watchdog timeouts |
| 4 | warm | 1 | 0.43 | 0.48 | 10/10 | |
| 4 | warm | 5 | 1.99 | 11.95 | 20/20 | Decode 8.7 to 9.8 s per stream |
| 4 | warm | 20 | 44.2 | 45.2 | 40/40 | A batch starts about every 10.8 s |

`ollama ps` showed the model at 2.5 GB with 1 slot, and 4.0 GB with 4 slots.

### Reading the results

- **Prefill and decode.** Prefill reads all prompt tokens in one pass, so compute limits it. At batch 1, decode reads all the model weights for each token, so memory bandwidth limits it. The ceiling is about 120 GB/s / 2.02 GB = 59 tokens/s. The measured rate was 42 to 44 tokens/s.
- **One slot.** Ollama works on one request at a time, and the others wait before prefill. Thus, the queue shows only in TTFT.
  - At 5 VUs, TTFT rose in steps of 3.35 s: the prefill and decode of the request ahead.
  - Little's law (N = X x R) agrees: 5 requests in flight at one request per 3.35 s gives about 16.8 s per request, which is the TTFT plus the decode.
- **Past saturation.** At 20 VUs with 1 slot, some requests waited longer than `PROVIDER_FIRST_TOKEN_TIMEOUT` (60 s). The status hold had already sent `200` and heartbeats. Thus, the stream ended with an error event and no `[DONE]`.
- **Four slots.** TTFT fell, for example from 14.43 s to 1.99 s p50 at 5 VUs. But each stream decoded about 3 times slower: about 14 tokens/s per stream, about 56 in total. One decode step for 4 streams took about 70 ms, against 22.5 ms for one stream. The cause is not verified.
- **Limits.** k6, Docker and the stack share the Mac with the model. The test is a closed loop, so it does not show the TTFT at a given arrival rate.
