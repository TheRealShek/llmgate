# Single-stream baseline

GLM-Edge-4B-Chat Q6_K is the primary model. Normal local inference now uses
CUDA. Earlier CPU gateway measurements remain historical evidence.

## Final authenticated baseline on October 25, 2026

This run includes authentication caching, per-key request limits, exact prompt
counting, daily quotas, durable Redis admission/final handoffs, batched Postgres
delivery, recovery workers, and Prometheus metrics. Redis uses AOF `always`,
`noeviction`, and a same-connection `WAITAOF` barrier for accounting writes.

| Measurement | Direct CUDA backend | Authenticated gateway |
| --- | ---: | ---: |
| Successful measured streams | 50/50 | 50/50 |
| Median first-content latency | 18.32 ms | 34.85 ms |
| p95 first-content latency | 18.63 ms | 49.40 ms |
| Median total response time | 1.730 s | 1.743 s |
| Median backend decode speed | 57.24 tokens/s | 57.44 tokens/s |
| Completion tokens per stream | 99 | 99 |

Each path excluded one warm-up, then ran 50 sequential streams with alternating,
reversed pair order. The repeated prompt asked for three sentences about gateway
timeouts, using temperature zero, `max_tokens: 128`, and final streaming usage.
All measured streams ended with `[DONE]` and `finish_reason: stop`. p95 uses
nearest rank. First-content latency measures the first nonempty content delta;
the gateway's TTFT metric can instead observe an earlier role-only event.

The pinned b11681 CUDA backend used GLM-Edge-4B-Chat Q6_K, a 2048-token context,
six CPU threads, one parallel slot, and all GPU layers on the 6 GiB RTX 3060
Laptop GPU. The CPU governor was `powersave`. The verification gateway used the
application defaults and its own Compose Postgres/Redis volumes, while sharing
the already-loaded learning backend. Its dedicated key allowed 6,000 requests
per minute and 1,000,000 tokens per day; quota grace was zero. See the
[raw samples](../bench/results/2026-10-25-durable-accounting-gpu.json) and
[metadata](../bench/results/2026-10-25-durable-accounting-gpu.metadata.json) for
source fingerprint, exact image IDs, model checksum, settings, and GPU telemetry.
The source was uncommitted; the metadata identifies that explicitly.

All 51 gateway requests, including warm-up, produced completed, non-recovered
usage rows. Their 5,916 input/output tokens exactly matched Redis charging.
TTFT samples increased by 51, active streams returned to zero, and all durable
queue slots were delivered and released. Public request counts include probes.
Client response duration ends at `[DONE]`; final durable handoff and database
delivery were checked separately afterward.

Median first-content latency was 16.53 ms above the direct path. This combines
prompt counting, Redis admission and disk persistence, proxying, and different
network paths. Direct calls used the container bridge IP; gateway calls used a
published loopback port. It does not isolate disk-sync or proxy CPU cost. Warm
prefix state, uncontrolled laptop thermals, and desktop activity limit the
comparison. This is a single-stream baseline, not a concurrent-capacity claim.

To reproduce, prepare the [Compose stack](manual-testing.md#compose-deployment),
create a private key with the limits above, and export `API_KEY`. With a Linux
Docker bridge backend:

```sh
DIRECT_URL="http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' llmgate-learning-backend-1):8080"
python3 bench/single_stream.py \
  --model glm-edge-4b-chat \
  --direct-url "$DIRECT_URL" \
  --gateway-url http://127.0.0.1:8080 \
  --requests-per-path 50 > /tmp/llmgate-final-baseline.json
```

Substitute your backend container and gateway URL. The recorded run used gateway
port 18091 in a disposable verification deployment. Never commit the key.

### Final fault and durability checks

The [verification results](../bench/results/2026-10-25-phase1-final-verification.json)
record 14 passing deployed checks: JSON/SSE completion, decoding and prefill
disconnects, different per-key rates, Redis/Postgres outages, a killed GPU backend
mid-stream, gateway SIGKILL recovery, graceful and forced shutdown with three
active requests, forced shutdown during a Postgres outage, never-responding and
idle backends, and active admin revocation.

One 867-token uncached prefill request released the backend about 0.394 seconds
after disconnect. A killed gateway's unfinished attempt became interrupted with
unknown counts and `recovered = true` after its remaining ownership lease expired,
about 23.73 seconds in that check. These are individual observations, not latency
distributions. Three requests drained normally in 1.907 seconds. Forced shutdown
took 3.275 seconds with healthy storage and 18.306 seconds with frozen Postgres;
the latter includes the driver's failed-connection cleanup. Both forced cases
saved all three canceled outcomes, including replay after Postgres recovery.

Separate disposable-storage checks passed unsafe-fsync rejection, final-slot
writes under memory pressure, and Redis SIGKILL/restart replay. A real ENOSPC
check used a 4 MiB tmpfs with a 3 MiB filler: 61 admissions received disk
acknowledgment before Redis stopped accepting writes. All 61 survived restart
on the same filesystem. This proves the tested intact-disk failure path, not
survival of disk loss or stale-replica promotion. Focused race tests also cover
fenced ownership, duplicate delivery, settlement retry, UTC rollover, capacity,
AOF admission limits, and bounded retention. Reproduction commands live in the
[accounting checks](manual-testing.md#persistent-inference-usage).

## Authenticated Compose baseline on October 25, 2026 (before durable admission)

This is the first recorded baseline with authentication caching, Redis request
limits, daily token reservations, Postgres usage recording, and Prometheus
metrics enabled. The gateway used its application defaults. Its dedicated key
allowed 6,000 requests/minute and 1,000,000 tokens/day, so this workload did not
hit admission limits. Grace was zero. This run predates synchronous Postgres
admission and the durable Redis accounting queue. It does not measure the current
AOF-always handoff or batch-delivery path. The final baseline above supersedes
this run for the current middleware.

| Measurement | Direct CUDA backend | Authenticated gateway |
| --- | ---: | ---: |
| Successful measured streams | 50/50 | 50/50 |
| Median first-content latency | 18.50 ms | 27.73 ms |
| p95 first-content latency | 21.45 ms | 29.56 ms |
| Median total response time | 1.745 s | 1.751 s |
| Median backend decode speed | 56.75 tokens/s | 56.84 tokens/s |
| Completion tokens per stream | 99 | 99 |

Both paths sent the same repeated prompt, `temperature: 0`, `max_tokens: 128`,
and `stream_options.include_usage: true`. Each excluded one warm-up and then
ran 50 sequential streams; paths alternated and reversed order after each pair.
All streams ended with `[DONE]` and `finish_reason: stop`. p95 uses nearest rank.
The client measures the first nonempty content delta, whereas gateway TTFT
metrics measure the first complete nonterminal SSE event, which may be role-only.
Client duration ends at `[DONE]`, before final persistence necessarily completes.

The backend used the pinned b11681 CUDA image, GLM-Edge-4B-Chat Q6_K, a 2048-token
context, six CPU threads, one parallel slot, and full GPU offload on the 6 GiB
RTX 3060 Laptop GPU. The CPU governor was `powersave`. The
local gateway revision, exact image IDs, model checksum, driver, kernel, GPU
samples, and limits are recorded in the [run metadata](../bench/results/2026-10-25-authenticated-gpu.metadata.json).
[Raw results](../bench/results/2026-10-25-authenticated-gpu.json) contain every sample.

The measured gateway median first-content latency was 9.22 ms above direct
inference. This includes authentication, Redis admission, two backend
prompt-counting calls, observation, and different network paths. Direct calls
used the container bridge IP; gateway calls used its published loopback port.
It does not isolate proxy CPU overhead. The repeated prompt keeps prefix state
warm, and laptop thermal state and background desktop load were not controlled.
This establishes neither cold-cache performance nor concurrent capacity.

All 51 gateway requests, including warm-up, produced completed usage rows.
Their 5,916 total tokens exactly matched Redis quota charging. The TTFT sample
count increased by 51 and active streams returned to zero. Public request counts
also included health/readiness probes during the run.

To reproduce on the prepared learning stack, use its private benchmark key file
(created locally, never committed). For another deployment, create a key with
the limits above using the [setup guide](manual-testing.md#compose-deployment),
set `API_KEY`, and substitute its URLs. The bridge-IP command below assumes
Linux and the learning container name:

```sh
source deploy/.env.benchmark
DIRECT_URL="http://$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' llmgate-learning-backend-1):8080"
python3 bench/single_stream.py \
  --model glm-edge-4b-chat \
  --direct-url "$DIRECT_URL" \
  --gateway-url http://127.0.0.1:18080 \
  --requests-per-path 50 > /tmp/llmgate-authenticated-baseline.json
```

The [separate fault-check evidence](../bench/results/2026-10-25-phase1-verification.json)
records the operational checks. One prefill cancellation used an 868-token
uncached prompt and released the backend slot about 0.44 seconds after client
disconnection. That is one observation, not a cancellation-latency distribution.

The baseline above predates the controlled-shutdown fix. The
[shutdown follow-up](../bench/results/2026-10-25-shutdown-accounting.json)
records the later lifecycle checks: all three canceled rows persisted with
healthy storage, while a frozen Postgres caused logged write failures and the
pinned driver's separate connection-cleanup delay. Reproduce the accounting
regression with the [manual test commands](manual-testing.md#compose-deployment).

## Local CPU and GPU check on October 16, 2026

The RTX 3060 Laptop GPU can run this model with full layer offload. We used
the official llama.cpp `b11681` CUDA 12.8 binary for both modes, six CPU
threads, a 2048-token context, and one parallel slot. CPU execution selected
`--device none --n-gpu-layers 0`. CUDA selected
`--device CUDA0 --n-gpu-layers 99`, which offloaded all 41 layers. A separate
functional check also verified the documented `--n-gpu-layers all` setting.

| Measurement | CPU | CUDA on RTX 3060 |
| --- | ---: | ---: |
| Successful measured streams | 5/5 | 5/5 |
| Median first-token latency | 74.60 ms | 23.79 ms |
| Median total response time | 7.019 s | 1.935 s |
| Median backend decode speed | 13.82 tokens/s | 51.27 tokens/s |
| Completion tokens per measured request | 97 | 99 |
| Finish reason | stop | stop |

The median backend decode rate was about 3.71 times the CPU rate. Both modes
used the same prompt, temperature zero, and maximum of 128 completion tokens.
Floating-point execution differs between CPU and CUDA, so deterministic
sampling did not produce identical token counts. Compare decode rates with
that limitation in mind; total response times also include different lengths.

Each mode excluded one warm-up and then ran five sequential requests directly
against llama.cpp. The prompt repeats and its prefix stays cached. CPU ran
first, followed by CUDA. This is a small local feasibility check, without
concurrent load, controlled thermal conditions, or a gateway in the request
path. It does not establish tail latency or authenticated gateway overhead.
The laptop was on AC power with the CPU governor set to `powersave`.

During the GPU run, `nvidia-smi` identified the inference process using
3692 MiB. Total sampled GPU memory, including other applications, peaked at
4337 MiB; sampled GPU utilization reached 99%. Startup reported a 3244.16 MiB
CUDA model buffer, 240 MiB KV cache, and 74.01 MiB CUDA compute buffer. Host
buffers and runtime allocations also exist. These figures apply to this
context and slot count, not an arbitrary workload.

JSON chat, streaming completion, model listing, and health checks passed in
both modes. An additional CUDA stream disconnected after its first content
token. The backend logged cancellation and its slot became idle within about
43 ms of closing the response in this one check. A subsequent JSON request
completed. This checks direct backend cancellation, not the full gateway path.

Reproduce with the [manual setup and comparison commands](manual-testing.md#8-record-a-glm-single-stream-baseline).
See [CPU rows](../bench/results/2026-10-16-glm-cpu-5.json),
[CUDA rows](../bench/results/2026-10-16-glm-gpu-5.json), and
[runtime metadata and GPU evidence](../bench/results/2026-10-16-glm-cpu-gpu.metadata.json).
The earlier gateway baseline below used another build and must not be treated
as the CPU half of this comparison.

## Historical CPU GLM run on October 14, 2026

We started llama.cpp and the Go gateway in separate external Foot windows, then
ran `python bench/single_stream.py --model glm-edge-4b-chat`. The run excluded
one warm-up per path and measured 100 sequential requests, 50 directly to
llama.cpp and 50 through the gateway. All 100 completed with `[DONE]`, generated
95 completion tokens, and reported `finish_reason: "stop"`.

| Measurement | Direct llama.cpp | Through gateway |
| --- | ---: | ---: |
| Successful requests | 50/50 | 50/50 |
| Median client-observed first-token latency | 77.55 ms | 77.89 ms |
| p95 client-observed first-token latency | 84.27 ms | 86.41 ms |
| Median total response time | 7.186 s | 7.195 s |
| p95 total response time | 7.587 s | 7.637 s |
| Median backend decode speed | 13.23 tokens/s | 13.20 tokens/s |
| Completion tokens per request | 95 | 95 |
| Failures | 0 | 0 |

[Raw GLM results](../bench/results/2026-10-14-glm-single-stream-100.json) contain
all 100 measured rows and the unrounded summaries.
[Run metadata](../bench/results/2026-10-14-glm-single-stream-100.metadata.json)
records the model checksum, image digest, server build, hardware, commands,
warm-ups, compiler versions, and system state. Both services remained ready
after the run.

The backend used six CPU threads, a 2048-token context, and one parallel slot
on an Intel Core i7-12650H with 16 GB RAM. The prompt repeats, so this is a
warm-cache baseline. It uses `temperature: 0` and `max_tokens: 128`. Paths
alternate and the first path reverses after every pair. p95 uses the nearest-rank
method on each path's 50 measurements.

The model file is the official Q6_K GGUF documented in
[manual testing](manual-testing.md), with SHA-256
`d8e83d4f9b6c6c89214d5c1dd17355d44be99173c15efa846d31ce01d1bda1f3`.
The measured llama.cpp build was `b11580-4413a1924`. Its CPU Docker image was:

```text
ghcr.io/ggml-org/llama.cpp@sha256:c789283572ba78c2699b8945a96e654c850f938a717883f2e6187a14adac698f
```

The saved metadata contains the original Docker startup command. The current
manual guide starts native CUDA inference and does not reproduce this CPU run.

Gateway and benchmark source matched
[the model-switch commit](https://github.com/TheRealShek/llmgate/commit/456fc4d1f2cb2a5d588641a8e25cd7357a881940).
Uncommitted key-storage work existed, but the running gateway did not use it.
Gateway startup cleared inherited `GATEWAY_` variables and set only the listen
address and upstream URL, leaving the other limits at their documented defaults.

The difference between median first-token latencies was about +0.34 ms for the
gateway. The median of individual paired gateway-minus-direct differences was
about -0.34 ms. These small, mixed differences do not isolate proxy processing
cost. Decode speed was similar across both paths in this run.

This local run does not establish concurrent capacity, reliable p99 latency,
cold-cache performance, or performance after authentication and rate limits.
Power and CPU policy were recorded at the end, but background load and thermal
state were not controlled throughout the test.

## Historical Qwen run on October 14, 2026

We sent 100 measured requests, 50 directly to llama.cpp and 50 through the
gateway. All completed with text and `[DONE]`, generated 70 completion tokens,
and reported `finish_reason: "stop"`. The measured run took about 175.5 seconds,
excluding warm-up requests.

| Measurement | Direct llama.cpp | Through gateway |
| --- | ---: | ---: |
| Successful requests | 50/50 | 50/50 |
| Median client-observed first-token latency | 27.84 ms | 28.48 ms |
| p95 client-observed first-token latency | 30.29 ms | 30.19 ms |
| Median total response time | 1.754 s | 1.757 s |
| Median backend decode speed | 39.99 tokens/s | 39.91 tokens/s |
| Completion tokens per request | 70 | 70 |
| Failures | 0 | 0 |

[Raw results](../bench/results/2026-10-14-single-stream-100.json) contain every
measured request and the unrounded summaries. p95 uses the nearest-rank method
on the 50 successful measurements for each path.

## Workload and setup

This section describes the historical Qwen run.

- One active request at a time, with one warm-up request per path excluded.
- Direct endpoint at `http://127.0.0.1:8081`; gateway at `http://127.0.0.1:8080`.
- Paths alternate, with the first path reversed after each pair.
- Qwen2.5 1.5B Instruct, GGUF Q4_K_M, on the CPU backend.
- Prompt: `Explain why a gateway needs timeouts in three short sentences.`
- `stream: true`, `max_tokens: 128`, and `temperature: 0`.
- Backend startup used a 2048-token context, one parallel slot, and six threads.
- The machine inspected during setup had an Intel Core i7-12650H and 16 GB RAM.

The repeated prompt and warm-ups make this a warm-cache workload. The backend
and gateway run on the same machine. The run did not capture an image digest,
model checksum, or background system load, so it is not a fully pinned benchmark.

## Run the benchmark with GLM

Follow [manual testing](manual-testing.md) to start the backend and gateway.
Leave both running and avoid other inference traffic. Export an active `API_KEY`
using the manual authentication setup. The script sends it only to the gateway
and does not include it in the report. From the repository root:

```sh
python bench/single_stream.py > /tmp/llmgate-glm-single-stream-100.json
```

Current gateway requests include a direct Postgres authentication lookup. The
saved baselines predate authentication and do not measure this additional work.
New reports label their authentication mode; compare results with that difference
in mind. Historical results and metadata remain unchanged.

The script uses Python's standard library. Progress and warm-up results go to
stderr; the JSON report goes to stdout. It exits with a nonzero status if a
warm-up fails or any measured request fails. Failures during measured requests
remain in the report rather than contributing latency values to the summaries.

The default model is `glm-edge-4b-chat`. The script preserves the prompt, timing,
warm-up, and ordering method from the saved run. The model has changed, so
new output tokens and timings must be recorded as GLM results.

For reference, the historical run used this model argument. Repeating it would
require restoring the retired backend; the current GLM setup cannot reproduce
Qwen measurements:

```sh
python bench/single_stream.py \
  --model 'Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M' \
  > /tmp/llmgate-qwen-single-stream-100.json
```

## What the measurements mean

The raw `ttft_ms` field measures client-observed first-token latency. Timing
starts immediately before Python opens the HTTP request and stops when it parses
the first SSE event with nonempty `delta.content`. It includes client processing
and local HTTP transport. It does not measure the gateway's internal TTFT,
which the project intent defines from request arrival to the first SSE data
write. That metric remains future work.

`total_ms` ends after the client receives `[DONE]` and closes the response.
`backend_decode_tps` comes from llama.cpp's `timings.predicted_per_second`.
It describes backend decoding speed, not aggregate gateway throughput.

In the historical Qwen run, the difference between the two median first-token
latencies is about 0.64 ms.
The median of the 50 paired gateway-minus-direct differences is about 0.22 ms.
These are different summaries; neither isolates gateway processing cost from
run-to-run variation.

No obvious slowdown appeared through the gateway in this workload. This run
does not establish concurrent performance, reliable p99 latency, cold-cache
latency, or performance after authentication and rate limiting are added.
