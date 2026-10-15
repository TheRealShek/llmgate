# Single-stream baseline

GLM-Edge-4B-Chat Q6_K is the primary model. Its measured baseline is below.
The older Qwen results remain separately labeled as historical evidence.

## GLM run on October 14, 2026

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
The measured llama.cpp build was `b11580-4413a1924`. To use the exact image again,
replace the `:server` image in the manual startup command with:

```text
ghcr.io/ggml-org/llama.cpp@sha256:c789283572ba78c2699b8945a96e654c850f938a717883f2e6187a14adac698f
```

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
