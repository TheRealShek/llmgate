# Architecture

This page explains the implemented, single-backend gateway in Phase 1. Start
with the [system map](../README.md#architecture), then use this page to
trace a request and find the package responsible for each step. The
[project intent](PROJECT_INTENT.md) describes planned work. Queues, multi-backend
routing remain later-phase work.

## One chat request

What happens to `POST /v1/chat/completions`? The arrows below show handler
order, including deferred finalization. External services appear in the README
system map, not this handler diagram. The logging wrapper assigns a
request ID before metrics and routing, then logs completion when the inner handlers return.

```mermaid
flowchart TD
    subgraph Logged[Request logging surrounds the public handler]
        direction TD
        Metrics[Observe public request] --> Routes[Match method and path]
        Routes --> Auth[Authenticate key]
        Auth -->|identity accepted| Rate[Check request credit]
        Rate -->|admitted and backend configured| Chat[Validate bounded chat body]
        Chat -->|supported text chat| Quota[Count input and reserve token allowance]
        Quota -->|capped request| Usage[Usage recorder observes the response]
        Usage -->|stream false or omitted| JSON[JSON proxy]
        Usage -->|stream true| SSE[Streaming proxy]
        JSON -.->|after proxy returns or aborts| Finalize[Settle token allowance, then insert usage]
        SSE -.->|after proxy returns or aborts| Finalize
    end
```

Authentication requires one Bearer credential. A successful lookup registers
the request for local revocation and adds the key identity and limits to its
context. Invalid credentials return `401`; database lookup failures return `503`
on a cache miss. The lookup deadline ends before forwarding starts.

Request admission consumes one credit from the key's Redis token bucket.
Exhaustion returns `429` with `Retry-After`; Redis failures return `503`.
These rejections stop before validation or inference. Admission has its own
deadline, which does not limit the subsequent inference request. Once admitted,
a request keeps its charge even if validation, forwarding, or delivery fails.

Chat validation bounds the body, checks the JSON object and response mode, and
validates or supplies `max_tokens`. It preserves backend-specific fields. The
backend checks the model, messages, and generation options. Quota preparation
also checks text content, one-completion bounds, and generation-limit aliases. Validation buffers
the body, so the handler clones the request and supplies a new body reader to
the chosen proxy while preserving its context. Before that clone enters the
proxy, `quota` renders and tokenizes the text prompt, reserves its allowance in
Redis, and caps `max_tokens` to the output budget. It requests final usage for
streams. Counting and Redis have independent deadlines that do not bound generation.

The proxy removes the gateway Authorization header, sets the upstream target
and request ID, and forwards the request. The backend generates the response;
the proxy copies its status, headers, and body to the client. Responses travel
back through the same HTTP handler call, not through a second admission check.

Validated chat enters a usage recorder before the selected proxy. It observes
only bytes accepted by the client-facing writer and preserves flushes and proxy
abort panics. Token settlement runs first, then one bounded final insert after
proxy copying finishes, even
on disconnect or abort. It records the request ID, key, backend, requested model,
HTTP status, outcome, timings, and observed backend token counts. The row contains
no prompt or response text. Models and pre-inference rejections do not create rows.

Other routes take shorter paths. Models authenticate and consume a credit but
skip chat validation. Liveness checks only the process. Readiness makes a fresh
backend health probe, bypassing authentication and Redis; it neither reserves
capacity nor gates chat traffic. If no backend is configured, models and chat
return `503` after authentication and admission, before chat validation.

## Package responsibilities

Each package link opens a starting point. The [code reading order](#read-the-code)
below connects the handlers for one streaming request.

| Package | Owns | Boundary |
| --- | --- | --- |
| [`cmd/gateway`](../cmd/gateway/run.go) | Configuration, component assembly, shared resources, listeners, shutdown, and local key-creation command. | Connects packages into a running service; handlers own request behavior. |
| [`internal/gateway`](../internal/gateway/handler.go) | Public routes, handler order, chat validation, and response-mode selection. | Backend validates generation-specific fields. |
| [`internal/auth`](../internal/auth/handler.go) | Caller identity, bounded identity cache, active-request tracking, and revocation coordination. | `store` persists keys; cancellation tracking belongs to this process. |
| [`internal/ratelimit`](../internal/ratelimit/handler.go) | Redis-backed atomic request admission and its deadline. | Request credits count HTTP requests, not generated tokens. |
| [`internal/quota`](../internal/quota/limiter.go) | Exact backend prompt counting, atomic daily reservations, output caps, and settlement. | Backend enforces generation limits; Redis owns shared budget state. |
| [`internal/proxy`](../internal/proxy/forward.go) | Upstream HTTP forwarding, credential removal, JSON deadlines, and streaming flushes and idle cancellation. | `gateway` selects response mode; backend generates tokens. |
| [`internal/backend`](../internal/backend/readiness.go) | On-demand backend readiness probes. | No periodic health registry or backend selection yet. |
| [`internal/usage`](../internal/usage/recorder.go) | Bounded JSON/SSE observation and one final usage-write attempt. | Counts describe observed backend usage; the recorder settles the quota permit before writing Postgres. |
| [`internal/store`](../internal/store/usage.go) | Postgres pool setup, key lifecycle, and usage insertion. | Callers own pool lifetime and operation deadlines. |
| [`internal/admin`](../internal/admin/handler.go) | Operator-token authentication and private key-management routes. | Creation calls `store`; revocation calls the shared authenticator. |
| [`internal/metrics`](../internal/metrics/metrics.go) | Private Prometheus registry, bounded key labels, request observations, TTFT, active streams, and scraping. | Authentication tags identity; the usage observer supplies streaming events. Scraping reads no external dependencies. |
| [`internal/requestlog`](../internal/requestlog/handler.go) | Request IDs and start/finish logs around handlers. | Normal handler return does not establish successful inference or delivery. |
| [`internal/api`](../internal/api/error.go) | Shared JSON error envelope and writing it to HTTP responses. | Callers choose the status and error details. |

## Read the code

For `POST /v1/chat/completions` with `stream: true`, follow these entry points:

1. [`run`](../cmd/gateway/run.go) constructs dependencies and assembles handlers.
2. [`requestlog.NewHandler`](../internal/requestlog/handler.go) surrounds
   [`metrics.Metrics.Observe`](../internal/metrics/metrics.go) and
   [`gateway.NewHandler`](../internal/gateway/handler.go), which matches the route.
3. [`auth.handler.ServeHTTP`](../internal/auth/handler.go) authenticates and
   registers the request through [`lookupAndRegister`](../internal/auth/active.go).
4. [`ratelimit.handler.ServeHTTP`](../internal/ratelimit/handler.go) checks admission
   through [`RedisStore.Take`](../internal/ratelimit/store.go) and
   [`bucket.lua`](../internal/ratelimit/bucket.lua).
5. [`chatHandler.ServeHTTP`](../internal/gateway/chat.go) validates the body and
   selects the response mode. [`Limiter.Admit`](../internal/quota/limiter.go) counts
   input and reserves output, then the handler calls
   [`Recorder.Record`](../internal/usage/recorder.go) with the quota permit.
6. [`streamHandler.ServeHTTP`](../internal/proxy/stream.go) adds idle cancellation;
   [`newForwarder`](../internal/proxy/forward.go) rewrites the upstream request.
   Go's reverse proxy copies the response through the
   [observer](../internal/usage/observe.go). JSON delivery instead uses
   [`NewNonStreamingHandler`](../internal/proxy/nonstream.go) and a total deadline.
7. On normal return or panic unwinding, the recorder releases the active-stream
   gauge, classifies the outcome, and
   settles the permit through [`Permit.Finish`](../internal/quota/limiter.go), then
   calls [`UsageStore.RecordUsage`](../internal/store/usage.go). The original panic
   continues after the write attempt.

Use the ownership sections below for revocation and shutdown. Setup and test
commands live in the [manual testing guide](manual-testing.md).

## Where state lives

- **Postgres** stores key hashes, identities, configured limits, and revocation
  timestamps, plus final usage records. Raw keys are returned once on creation.
  `tokens_per_day` supplies the normal daily token budget.
- **Redis** stores request credits per key, plus daily spent and reserved tokens
  in `llmgate:tokens:v1:<key_id>`. Models and chat share the request bucket;
  only validated chat reserves token budget.
  Gateways serving the same key database must share the Redis database for a
  shared limit. Credits are admission state, not a durable billing ledger.
- **Process memory** holds the bounded identity cache and active-request registry
  in one shared authenticator. Cache hits do not renew expiry. During a database
  outage, unexpired cached identities can authenticate; misses and expired entries
  fail closed. Redis must still admit those requests. Cache expiry affects new
  authentication, not streams already admitted.
- **Each request context** carries logging metadata, authenticated identity, and
  cancellation through the handler chain. Authentication and admission use
  shorter-lived child contexts for their own deadlines. Their expiry does not
  become the stream's total deadline.

Admin revocation first updates Postgres, then invalidates local cached identities
and signals active requests for that key. The cache and registry share a mutex;
coordination with in-flight lookups prevents an overlapping lookup from restoring
a revoked identity. Other gateway processes receive no cancellation signal.
Direct SQL revocation also sends no local signal; cached identities can remain
usable until expiry. A successful admin response acknowledges the update and
local signals, not that backend decoding has already stopped. See the
[revocation decision](decisions.md#revocation-and-active-requests) for coordination details.

## Response lifetime and cleanup

JSON forwarding has a total upstream deadline, including response-body reads.
Streaming has connection, response-header, and idle-body-read limits, with no
total stream deadline. An idle limit bounds each wait for backend bytes, not
time between model tokens. The reverse proxy flushes copied bytes immediately;
the usage observer parses complete SSE data events while forwarding continues.
Its line/event and JSON buffers have explicit bounds. Oversized payloads continue
to the client; unavailable metadata stays unknown rather than failing inference.

Before response headers are sent, upstream timeouts return `504` and connection
failures return `502`. Backend HTTP error responses pass through. After the
response starts, a copy failure ends an incomplete response. The gateway cannot
replace its status or append a guaranteed structured error. It has no application
retry or backend failover policy today.

Client disconnects cancel upstream HTTP work through the inherited request
context. An idle-body timeout cancels that stream's child context; local
revocation cancels the authenticated request context. These are signals to stop
HTTP work, not proof that a particular model server stopped decoding. The
[manual cancellation check](manual-testing.md#6-test-client-disconnect-cancellation)
covers real-backend verification.

As handlers exit, the stream wrapper stops or joins its read timer, deferred
cancellation releases child contexts, and authentication removes the request
from its registry. The outer logger records elapsed time and whether its inner
handler returned normally. An HTTP error can still have `completed: true`;
an aborted proxy response can have `completed: false`.

## Token allowance ownership

`quota.Limiter` owns immutable dependencies. Each admitted chat gets one
`Permit` containing its Redis reservation, with no retained body or context.
The usage recorder finishes that permit during normal return or panic unwinding.
A completed response with matching usage refunds unused output. Uncertain
outcomes retain the full allowance while releasing the pending marker.

Redis serializes reservations and settlement with Lua. Pending allowances count
against admission, and duplicate settlements cannot refund twice. The Redis day
and expiry belong to admission, so a stream ending tomorrow cannot change
tomorrow's budget. Dependency failures stop new inference before forwarding.
Settlement failures retain the charge and preserve the response. A crashed
request's marker expires with its daily counter. See the
[quota decision](decisions.md#daily-input-and-output-quotas) for grace, supported
inputs, and durability limits.

## Usage records and missing information

`total_ms` measures gateway arrival through proxy completion or abort, excluding
token settlement and the final database write. Streaming `ttft_ms` measures arrival through the first
complete, nonterminal SSE data event accepted by the response writer. This can
be a role-only event; it is not a backend GPU timestamp or proof of client
receipt. Comments and `[DONE]` do not count. JSON requests have no observed TTFT.
The separate request-finish log also includes time spent finalizing usage.

`observed_sse_chunks` counts complete data events other than `[DONE]`, including
role-only, error, and usage-only events. It is not a model-token count. Token
counts come only from a valid backend `usage` object with both nonnegative
counts. Missing, partial, malformed, or oversized metadata leaves both counts
NULL. Quota preparation requests final SSE usage; the recorder observes those
events without rewriting the stream.
The requested model is NULL when absent or unsuitable for bounded text storage.

A normally returned 2xx JSON response or a 2xx SSE response ending in `[DONE]`
is `completed`. HTTP failures and observed SSE error payloads are `failed`.
Client or local-revocation cancellation is `canceled`. An abort, writer failure,
or SSE EOF without `[DONE]` is `interrupted`. HTTP status remains separate, so
an interrupted stream can have HTTP 200. These states describe observation at
the gateway, not a durable billing or delivery guarantee.

The final write uses `context.WithoutCancel` to retain logging metadata while
detaching client/revocation cancellation, then adds its own timeout. It is
synchronous and may delay handler or response completion up to that bound.
No connection is held for the duration of inference; the insert acquires one
from the existing pool. Failure logs a fixed error and does not rewrite the
response. There is no retry, background queue, or crash-recovery ledger. Process
exit or forced shutdown can lose a final write. The
[decision](decisions.md#inference-usage-recording) records this availability tradeoff.

## Prometheus measurement boundaries

Each gateway process owns a private registry with four metric families. Public
request counters and duration histograms finish after the handler returns or
unwinds, including quota settlement and usage writing. They include probes,
validation/authentication failures, models, and chat. Scrapes and the separate
admin listener are excluded. Status is the observed HTTP status, or `none` when
cancellation or a panic ends the request before headers. A 200 status does not
establish successful stream completion.

Authentication records the verified key ID on the request's shared observation
state. Model proxying and validated inference mark the fixed `primary` backend;
rejections and probes use `none`. Unknown paths, request IDs, model names, raw
keys, and payloads never become labels. The registry admits a bounded number of
key-ID labels for its lifetime and groups further identities under `other`.
The default capacity is zero, so all authenticated keys share `other`; per-key
labels require an explicit positive capacity.
Metrics reset when the process restarts. See the
[metrics decision](decisions.md#initial-prometheus-metrics) for the label policy.

Streaming inference increments its gauge before proxying, including prefill,
and decrements before accounting begins, even on cancellation or abort. The
existing SSE observer supplies one TTFT observation at its first complete data
event. Its timing and framing definition matches usage TTFT above. No event
means no TTFT sample, and JSON responses produce none. The metrics writer
preserves flushes and response control through `Unwrap`; its status is atomic
because reverse-proxy flushing may use another goroutine.

`GET /v1/metrics` and its HEAD form bypass authentication, Redis, quota checks,
and the backend. Scraping has a concurrency cap and a deadline. It exposes only
these application collectors, with no default process or Go-runtime collectors.

## Startup and shutdown ownership

[`run`](../cmd/gateway/run.go) opens and checks Postgres and Redis before binding
HTTP listeners, constructs one authenticator shared by public and admin handlers,
and owns the shared upstream transport. Both enabled listeners bind before
either serves. A partial bind failure closes listeners already opened. The
one-off key-creation command uses Postgres without starting either HTTP server.

On a termination signal, or an unexpected server exit,
[`runServers`](../cmd/gateway/server.go) stops both enabled listeners and drains
them concurrently under one deadline. Active requests can finish during that
window. Deadline expiry force-closes remaining connections, which cancels their
HTTP request contexts. Shutdown joins serving and shutdown goroutines before
returning to `run`, which closes idle upstream connections and releases Redis
and Postgres. The process signal starts draining; it does not immediately
cancel every active stream.
