# PROJECT_INTENT.md

## Intent

Build an OpenAI-compatible LLM gateway in Go. It sits in front of several inference servers (llama.cpp first, vLLM later) and handles auth, rate limiting, queueing, routing, and metrics.

The goal is a portfolio project that shows I can own the serving layer of an inference system, with measured numbers to back it up. It is also how I learn the backend side of inference infrastructure by building it.

## Non-goals

- No model training, fine tuning, or custom GPU kernels.
- No UI. Grafana is the only dashboard.
- No Kubernetes or multi-region. Docker Compose only.
- No Rust rewrite until this is finished and written up.

## Stack

| Part          | Choice                                                             | Why                                                 |
| ------------- | ------------------------------------------------------------------ | --------------------------------------------------- |
| Language      | Go, standard library `net/http`, optional `chi`                    | Goroutines and `context` fit long-lived streams     |
| Model backend | llama.cpp CUDA server with GLM-Edge-4B-Chat Q6_K GGUF            | Full GPU offload on the local RTX 3060; CPU for comparisons |
| Storage       | Postgres                                                           | API keys and usage records                          |
| Shared state  | Redis                                                              | Rate limit counters shared across gateway instances |
| Metrics       | Prometheus and Grafana                                             | Standard, and interviewers know it                  |
| Load testing  | k6                                                                 | Handles streaming responses                         |
| Runtime       | Docker Compose                                                     | One command to start everything                     |
| Comparison    | vLLM on a rented GPU for one day                                   | Phase 3 only                                        |

## Architecture

This is the target architecture across phases. See the
[architecture guide](architecture.md) for the implemented request flow and components.

```
client
  |
  v
gateway (Go)
  auth -> rate limit -> admission queue -> router -> proxy
  |            |                                     |
  v            v                                     v
Postgres     Redis                  llama.cpp A, B, C (later vLLM)

gateway /v1/metrics -> Prometheus -> Grafana
```

Request path, in order:

1. Authenticate the API key (hash lookup in Postgres, cached in memory).
2. Check the per-key rate limit and token quota (Redis).
3. Enter the bounded queue for the chosen backend. If it is full, return `429` with `Retry-After`.
4. Pick a backend (health, load, and prefix).
5. Proxy the request and stream tokens back as SSE.
6. Record usage and metrics when the stream ends.

## API surface

- `POST /v1/chat/completions` with `stream: true` and `stream: false`
- `GET /v1/models`
- `GET /v1/healthz` (process is up) and `GET /v1/readyz` (at least one backend is healthy)
- `GET /v1/metrics`
- `POST /v1/admin/keys` and `DELETE /v1/admin/keys/{id}` behind an admin token

All gateway endpoints use a version prefix. The current API is `/v1`;
future API versions use separate prefixes such as `/v2`.

## Data model

```
api_keys(id, key_hash, name, requests_per_min, tokens_per_day, created_at, revoked_at)
usage(id, key_id, backend, model, prompt_tokens, completion_tokens,
      ttft_ms, total_ms, status, created_at)
```

Store only the hash of each API key. Never log the key or the prompt body.

---

## Phase 1. Working gateway on one backend

Target: weeks 1 to 3.

**Goal.** A single gateway in front of a single llama.cpp instance that handles real streaming clients correctly and knows who is calling.

**Build**

- Docker Compose with gateway, llama.cpp, Postgres, Redis.
- `POST /v1/chat/completions` that passes the SSE stream through chunk by chunk. Flush after every chunk.
- Non-streaming mode that returns one JSON response.
- Client disconnect cancels the upstream request. Use `http.NewRequestWithContext(r.Context(), ...)` and confirm llama.cpp stops decoding.
- Timeouts on connect, on time to first byte, and on idle gaps between chunks.
- API keys in Postgres with a migration tool (`goose` or `golang-migrate`). Keys are stored as hashes.
- Token bucket rate limit per key in Redis. Write it as a Lua script so the check and the decrement are atomic.
- Daily token quota per key covering input and output, with exact backend prompt counting, shared reservations, and configurable grace defaulting to zero. Reconcile completed requests from backend usage; retain reservations when consumption is uncertain.
- Usage row written when each request finishes, including TTFT and total time.
- Structured logs with `log/slog` and a request ID on every line.
- Graceful shutdown. On SIGTERM, stop accepting new requests and let active streams finish, with a hard deadline.
- First Prometheus metrics. Request count, request duration, TTFT, active streams.

**Current checkpoint and next slices**

Streaming and JSON forwarding, request validation, cancellation, deadlines,
request logs, readiness, and graceful shutdown are implemented. API-key storage
now feeds route authentication through a bounded in-memory cache with Postgres
lookups on misses. Models and chat require an active key; health and readiness
remain public. A local command creates keys. An opt-in loopback admin listener creates and revokes keys over HTTP using a
separate operator token. Revocation persists in the same store and signals
active requests for that key through the shared authenticator. Public routes never expose admin
operations. Both servers share bounded startup and graceful shutdown ownership.
The authenticated CUDA baseline is recorded in [benchmarks](benchmarks.md).

Authentication caches successful identities for a fixed lifetime. Admin revocation
invalidates local entries; unexpired entries work during database outages, while
misses and expired entries fail closed. Redis now enforces per-key request rates
using an atomic token bucket. Models and chat share each key's bucket; exhaustion
returns `429` with retry guidance, and Redis errors fail closed with `503`.
Compose now defines the gateway, pinned CUDA backend, Postgres, Redis, and
startup migrations with health checks and persistent storage. Container inference,
CUDA model loading, persistence, dependency outages, cancellation, backend failures,
and concurrent shutdown have been exercised. Controlled shutdown now waits for
bounded request accounting before dependency teardown. Storage outages, crashes,
or an exhausted cleanup window can still lose best-effort usage rows. Validated
inference attempts now produce bounded, best-effort Postgres usage records with
timings, outcomes, backend token counts when present, and separate observed SSE
chunk counts. Daily input/output quotas now use exact prompt counting on the
pinned text backend, atomic Redis reservations, and a capped backend response.
Grace defaults to zero. Missing or interrupted usage retains the allowance
without inventing token counts. Initial request-count, duration, TTFT, and active-stream
metrics are exposed at `/v1/metrics`. The authenticated GPU baseline and operational
evidence are recorded. The forced-shutdown accounting regression is fixed and
covered by real-storage and GPU checks. Durable recovery remains the deferred
follow-up below; later-phase routing and overload protection have not started.
See [decisions](decisions.md) for authentication and revocation boundaries.

**Learn**

- `context` propagation and cancellation
- `http.Flusher` and how SSE works over HTTP/1.1
- Why a rate limit check must be atomic
- What happens to a goroutine when the client disappears

**Done when**

- A `curl -N` call streams tokens as they are generated.
- Killing the client mid-stream stops the backend within about a second. Show this in the llama.cpp logs.
- Two keys with different limits get different behavior under the same load.
- Baseline TTFT and tokens per second for one stream are written down in `docs/benchmarks.md`.

---

## Phase 2. Multiple backends and overload protection

Target: weeks 3 to 5.

**Goal.** The gateway stays predictable when load rises or a backend dies.

**Build**

- Run 2 to 3 llama.cpp instances in Compose.
- Backend registry with health checks every few seconds. Mark a backend unhealthy after N failures and healthy again after M successes.
- Bounded queue per backend with a fixed number of concurrent slots (match llama.cpp `--parallel`). When the queue is full, return `429` with `Retry-After`. Do not let latency grow without a limit.
- Queue wait timeout. A request that waits too long returns `503` instead of starting a stream nobody wants.
- Least-loaded routing based on in-flight requests per backend.
- Retry on a different backend only if no bytes have been written to the client. After the first token, a failure ends the stream with an error event. Never retry mid-stream.
- Circuit breaker per backend.
- Chaos tests. Kill a backend during a load test and check what clients see.
- Full metrics set (see below) and one Grafana dashboard checked into `deploy/grafana/`.

**Learn**

- Backpressure and load shedding
- Why an unbounded queue turns an overload into a latency disaster
- Tail latency, and why p99 matters more than the mean
- Failure handling when you cannot safely retry

**Done when**

- Under 3x overload, the gateway returns fast `429`s and p99 for admitted requests stays flat.
- Killing one backend causes a short blip, not an outage.
- No goroutine leaks after a test run. Check with `runtime.NumGoroutine()` exposed as a metric, or with `pprof`.
- The Grafana dashboard shows queue depth, active streams, TTFT, and error rate per backend.

---

## Phase 3. Prefix-aware routing, benchmarks, and write-up

Target: weeks 5 to 8.

**Goal.** Add the feature that sets the project apart, measure it honestly, and package the result.

**Build**

- Prefix-aware routing. Hash the first N characters of the rendered prompt (system prompt plus the start of the messages) and send requests with the same prefix to the same backend. Use rendezvous hashing so adding or removing a backend moves as few prefixes as possible.
- Load fallback. If the preferred backend's queue is above a threshold, route to the least-loaded one instead. Count how often this happens.
- Turn on prompt caching in llama.cpp (`cache_prompt`) so a repeated prefix skips prefill.
- A k6 workload with a shared long system prompt and many short user messages, so prefix reuse is possible. Add a second workload with all-unique prompts as a control.
- Benchmarks at 1, 10, and 50 concurrent streams. Report p50 and p99 TTFT, inter-token latency, and tokens per second, with and without prefix routing.
- One-day vLLM run on a rented GPU. Same gateway, same k6 scripts, different backend. Report the difference.
- README and write-up (see below).

**Learn**

- Prefill vs decode, and why they stress the hardware differently
- The KV cache and why reusing it cuts TTFT
- Consistent and rendezvous hashing, and the cost of a skewed hash
- How to design a benchmark that does not flatter your own feature

**Done when**

- The with and without routing comparison is in the README, with the raw k6 output committed.
- The README states the result even if routing helps less than expected, and explains why.
- Someone can clone the repo and run `docker compose up` and `make bench` without asking me anything.

---

## Deferred durability follow-up

Revisit durable accounting after the current correctness fixes. It is not part
of the controlled-shutdown fix, and its implementation phase is not assigned.

- Durably record request identity and quota reservation before inference starts,
  then retain the final outcome until delivery is acknowledged.
- Recover unfinished requests after a crash. Mark interrupted attempts and
  unknown token counts honestly; do not reconstruct model tokens from chunks.
- Retry usage delivery and quota settlement idempotently using request and
  reservation identities, always settling against the original UTC day.
- Before implementation, choose storage and sync guarantees, bound disk/backlog
  and retries, and decide admission behavior when durable storage is unavailable
  or full. Guaranteed recording requires rejecting work that cannot be recorded.

For now, Postgres writes remain best effort and uncertain Redis consumption
remains conservatively charged. Fix and verify known lifecycle bugs first.

## Metrics to export

- `gateway_requests_total{key, backend, status}`
- `gateway_ttft_seconds{backend}` (histogram)
- `gateway_inter_token_seconds{backend}` (histogram)
- `gateway_request_duration_seconds{backend}` (histogram)
- `gateway_tokens_total{backend, kind}` where kind is prompt or completion
- `gateway_active_streams{backend}`
- `gateway_queue_depth{backend}`
- `gateway_queue_wait_seconds{backend}` (histogram)
- `gateway_rejected_total{reason}` (rate_limit, queue_full, queue_timeout, no_backend)
- `gateway_backend_healthy{backend}`
- `gateway_prefix_route_total{result}` where result is hit or fallback

Measure TTFT at the gateway, from the moment the request arrives to the moment the first SSE data chunk is written to the client.

## Failure cases to test

- Client disconnects during prefill, and during decode
- Backend closes the connection mid-stream
- Backend hangs and never sends a byte
- Redis is down (authenticated request admission fails closed with `503`)
- Postgres is down (cached keys keep working, new keys cannot be created)
- Request body is huge, malformed, or has `max_tokens` set absurdly high
- SIGTERM while 20 streams are active

## Repo layout

```
cmd/gateway/main.go
internal/
  auth/
  ratelimit/
  queue/
  router/
  proxy/
  backend/
  metrics/
  store/
deploy/
  docker-compose.yml
  prometheus.yml
  grafana/
bench/
  k6/
  results/
docs/
  decisions.md
  benchmarks.md
Makefile
README.md
```

Keep `docs/decisions.md` as a running log. For each non-obvious choice, write the options, what I picked, and why. Examples are fail open vs fail closed on Redis, queue size per backend, and the prefix length used for hashing.

## README target

The top of the README, in this order:

1. One paragraph on what the gateway does.
2. The benchmark table (p50 and p99 TTFT, tokens per second, at 1, 10, and 50 streams, with and without prefix routing).
3. The Grafana screenshot.
4. The architecture diagram.
5. `docker compose up` instructions.
6. Design decisions and what I would change next.

## Rules for myself

- Finish a phase before starting the next one. Do not add features from a later phase early.
- Write the test for a failure case before fixing the bug.
- Every performance claim needs a number and a command that reproduces it.
- Commit only when explicitly requested. Completing a working session does not
  authorize a commit or push.
- Apply for backend and platform roles while Phase 1 is in progress. Link the repo even when it is unfinished.

## Questions I should be able to answer when this is done

- What happens to the upstream request when the client disconnects, and how did I verify it?
- Why does the queue have a bound, and how did I choose the number?
- Why is it safe to retry before the first token and unsafe after?
- Why did I use a Lua script for the rate limiter?
- What is the KV cache, and why does prefix routing reduce TTFT?
- What did my benchmark show, and what would make it misleading?
