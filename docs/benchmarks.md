# Single-stream baseline

## October 14, 2026 run

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

## Reproduce

Follow [manual testing](manual-testing.md) to start the backend and gateway.
Leave both running and avoid other inference traffic. From the repository root:

```sh
python bench/single_stream.py > /tmp/llmgate-single-stream-100.json
```

The script uses Python's standard library. Progress and warm-up results go to
stderr; the JSON report goes to stdout. It exits with a nonzero status if a
warm-up fails or any measured request fails. Failures during measured requests
remain in the report rather than contributing latency values to the summaries.

The script reproduces the request, timing, warm-up, and ordering method used
for the saved run. It does not promise identical output tokens or timings.

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

The difference between the two median first-token latencies is about 0.64 ms.
The median of the 50 paired gateway-minus-direct differences is about 0.22 ms.
These are different summaries; neither isolates gateway processing cost from
run-to-run variation.

No obvious slowdown appeared through the gateway in this workload. This run
does not establish concurrent performance, reliable p99 latency, cold-cache
latency, or performance after authentication and rate limiting are added.
