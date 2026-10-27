# Decisions

## Primary inference model

Use GLM-Edge-4B-Chat Q6_K as the primary model in llama.cpp, following
Abhishek's trial and selection. Serve it with the alias `glm-edge-4b-chat` so
requests and the benchmark use a stable model ID.

Use the official `zai-org/glm-edge-4b-chat-gguf` file `ggml-model-Q6_K.gguf`
at revision `e168d9e95cf9af8bfbe54e04df7123516a51415d`. Its SHA-256 is
`d8e83d4f9b6c6c89214d5c1dd17355d44be99173c15efa846d31ce01d1bda1f3`.
Keep the weights outside the repository and mount their directory read-only.
The file includes its chat template.

The previous Qwen model is retired. Abhishek confirmed running the scoped
Docker-volume cleanup command successfully. Historical Qwen benchmarks remain
unchanged and labeled with their original model. A separate
[GLM single-stream baseline](benchmarks.md) now records 100 measured requests
with the replacement model, its checksum, and the inference image digest.

## Local GPU inference

Use CUDA on the RTX 3060 Laptop GPU for normal inference. The earlier CPU-only
Docker command did not use this GPU. On October 16, 2026, the CUDA build loaded
all 41 model layers into GPU execution with a 2048-token context and one slot.
The process used about 3692 MiB of VRAM. Five measured streams per mode showed
median decode rates of 51.27 tokens/s on CUDA and 13.82 tokens/s on CPU with
the same llama.cpp release. See [results and limitations](benchmarks.md).

Use the pinned official `b11681` CUDA 12.8 binaries and runtime libraries for
the local setup. They run with the existing driver without a CUDA compiler,
system library changes, or Docker access. Select `CUDA0` and request all layers
explicitly. Check the startup offload message and the process in `nvidia-smi`;
successful HTTP responses alone do not prove GPU execution.

Keep CPU execution available for explicit comparisons and machines without a
usable GPU. Larger contexts, more slots, or other GPU applications may exhaust
VRAM even though this configuration fits. Recheck memory before increasing
those limits. The gateway still runs on the CPU and forwards HTTP requests;
the inference backend owns GPU execution. This remains the single-backend
Phase 1 setup, separate from the later vLLM comparison.

## Persistent API-key storage

Use PostgreSQL with versioned SQL migrations through `golang-migrate`, one of
the migration tools listed in the intent document. Run migrations explicitly
instead of changing the schema whenever the gateway starts.

Generate keys from 32 cryptographically random bytes, encode them as URL-safe
text with an `lg_` prefix, and store only SHA-256 hashes. A password hash is
useful for human-chosen passwords; these generated keys have 256 bits of entropy.
A unique hash index supports direct lookup. Return the raw key only after
successful insertion and never include it in stored identity structs.

Use `pgx` and its connection pool for PostgreSQL access. The standard library
does not include a PostgreSQL driver. Prefer explicit parameterized SQL over
an ORM for these three operations. The caller owns the pool and its limits.
See [pgx's guidance](https://github.com/jackc/pgx/wiki/Getting-started-with-pgx-through-database-sql)
on native PostgreSQL access.

Revoke by recording a timestamp, preserving key identity for later usage records.
Repeated revocation succeeds without changing the timestamp. Unknown and revoked
key lookups return `store.ErrKeyNotFound`; invalid creation options return
`store.ErrInvalidKeyOptions`. Define these reusable errors in
`internal/store/error.go` so their messages have one source. Callers use
`errors.Is` to recognize them. Storage failures preserve their original causes
through `%w` and stay distinct.
The HTTP authentication policy is described below. Caching remains later work.

Build and discuss the key-storage slice before wiring authentication into the
gateway, as agreed in the session. The development Compose file starts only
Postgres and its migration tool; the gateway and inference backend run separately.

## Code naming

Each name should describe its actual role or behavior in its package and at the
call site. Use matching names for related paths, such as `NewNonStreamingHandler`
and `NewStreamingHandler`, so the supported response mode is clear. Name timeout
parameters for what they bound, such as `totalTimeout` and `idleTimeout`.

Apply this to constructors, functions, types, fields, and variables. Keep names
concise when context already makes their meaning clear. Comments explain the
request flow and reasoning; names should make sense without reading the implementation.

## Documentation scope

Keep setup commands and configuration in the manual testing guide, design
choices here, and phase requirements in the project intent.
Keep measured results and their raw data in the existing benchmark files.
Keep the system map in the README and the implemented request flow, package
responsibilities, code reading order, and state and lifecycle ownership in
[architecture](architecture.md). Keep the README focused on the project overview,
a minimal start example, the system map, and links to these guides.
Update these documents instead of adding separate progress, planning, or
per-feature explanations that duplicate the code and drift as it changes.

## Model-list forwarding

Use the standard library's `httputil.ReverseProxy` for `GET /v1/models`.
It forwards backend status, headers, and body, and carries client cancellation
to the upstream request without adding a dependency.

Keep startup available when `GATEWAY_UPSTREAM_URL` is unset. `/v1/healthz` remains
available; model requests return `503` until a backend is configured.

`GATEWAY_UPSTREAM_TIMEOUT` bounds the entire model-list request, including the
response body. Its default is `30s`. A deadline before response headers produces
`504`; a deadline after the response starts terminates the incomplete response.
Transport failures before response headers produce `502`. Backend HTTP error
responses pass through unchanged.

This total deadline applies to model lists and non-streaming chat. Chat streams
use the separate timeouts described below.

## Endpoint versioning

All gateway endpoints use `/v1`, including health checks, metrics, and admin
routes. Future API versions use separate prefixes such as `/v2`. Unversioned
paths and unsupported versions return `404`.

## Non-streaming chat requests

`POST /v1/chat/completions` accepts a JSON object with `stream` omitted or false.
`stream: true` selects the SSE forwarding path. Model, message, and
backend-specific field validation remains the backend's responsibility.

Buffer the request once to validate JSON and the stream flag, then forward the
original bytes. This preserves fields the gateway does not understand. Bound
that buffer with `GATEWAY_MAX_BODY_BYTES`, which defaults to `1048576` bytes.
Oversized bodies receive `413`; invalid JSON receives `400`; unsupported content
types receive `415`.

`GATEWAY_READ_TIMEOUT`, default `10s`, bounds reading incoming request headers
and bodies. It does not limit response streaming. Backend requests retain the
client's cancellation and use `GATEWAY_UPSTREAM_TIMEOUT` for their total deadline.

## Chat output-token limits

`GATEWAY_MAX_TOKENS`, default `1024`, limits explicitly supplied `max_tokens`
values before forwarding. The value must be a positive integer within the
configured limit; invalid or excessive values receive `400` without starting
backend work.

Insert the configured limit when `max_tokens` is omitted, so the request does
not depend on the backend's default. This validates and supplies the standard
`max_tokens` field; daily quotas and backend-specific token options are separate
work.

Inspect top-level options as `json.RawMessage`. Valid explicit token counts keep
the original request bytes. For omitted values, insert just the new field before
the closing object brace, preserving all existing bytes and unknown fields.
Update Content-Length to match the resulting body. The gateway constructor
receives both body and token limits; startup supplies positive values for both.

## Streaming chat forwarding

Streaming requests pass through the same JSON, body-size, and output-token
validation as non-streaming requests. Startup supplies a separate streaming
proxy. A configured JSON backend without a streaming proxy returns `501` for
streaming requests.

Use `ReverseProxy` with immediate flushing to forward backend response bytes
without parsing or rebuilding SSE events. A copied chunk can contain part of an
event or multiple events; the client still receives the original byte sequence.

`GATEWAY_UPSTREAM_CONNECT_TIMEOUT`, default `5s`, bounds TCP connection attempts
and TLS handshakes separately. `GATEWAY_UPSTREAM_HEADER_TIMEOUT`, default `30s`,
bounds waiting for backend response headers. Both apply to the shared transport.
Header timeouts before a response begins return JSON `504`.

`GATEWAY_STREAM_IDLE_TIMEOUT`, default `30s`, bounds each blocked upstream body
read, including the first read after headers. Each read owns a timer that cancels
only that request on expiry. Stop or join its callback after reading, so no timer
callback remains when the read returns. Streaming has no total request deadline.

Client disconnects cancel the upstream request. A backend failure or idle timeout
after response headers ends the incomplete stream; the gateway cannot replace
its HTTP status. Do not retry or append a fabricated completion marker.

## Request identity and logs

Generate a fresh, random request ID for every HTTP request, including health
checks and rejected requests. Return it in `X-Request-ID` and forward it to the
backend in the same header. Replace caller IDs and discard backend response IDs
so clients and gateway logs share one identity. Trusting caller IDs would let
clients reuse IDs or put sensitive text into logs.

Attach the request logger to the incoming context. Request clones retain this
metadata along with client cancellation. Copy the proxy per request before
setting its internal error logger, so concurrent requests do not share IDs.
The transport remains shared.

Log request start and finish, the method, the registered route, and elapsed
time in milliseconds. A deferred finish log runs when a handler returns or
panics. `completed` means the handler returned normally; it does not mean the
backend returned success or the client received every byte. Proxy failure logs
use the same ID and omit raw error messages. Do not log bodies, authorization,
raw URLs, query strings, or caller-supplied IDs.

Logging still passes the response writer through unchanged. The later
[usage-recording decision](#inference-usage-recording) adds a narrow observer
around validated inference only, preserving flushing and HTTP abort behavior.

## Gateway construction

Use one `gateway.NewHandler(options)` constructor for startup and tests.
`gateway.HandlerOptions` names the non-streaming proxy, streaming proxy, readiness
handler, authenticator, and chat limits. Named fields distinguish the three
`http.Handler` dependencies at each call site and make omitted dependencies
visible. Remove variants from earlier project stages instead of keeping them
as compatibility wrappers inside this internal package.

Keep `main` focused on logging setup, process signals, and exit status. `run`
selects local key creation or HTTP startup and owns the selected execution path.
Keep command parsing errors wrapped with their causes while exposing only safe
messages. Flag errors can contain supplied values, so use the command's private
error wrapper before returning them to the logging boundary.

Keep logging, routing, chat validation, and proxying as separate components
because each owns distinct behavior. Startup names the route handler `routes`
and its logging wrapper `loggedRoutes` so their execution order is visible.
The architecture guide follows one streaming request through the functions that
run it.

Keep both response modes in the `proxy` package because both forward requests
to the backend. `nonstream.go` owns the total-deadline handler; `stream.go` owns
the streaming handler and idle-read wrappers. `forward.go` owns reverse-proxy
setup and error logging shared by both modes. Group code by its responsibility
instead of grouping all types that implement `http.Handler` into one package.

## Bounded inference admission

Use one in-memory FIFO queue per backend in each gateway process. Bound active
slots and waiting capacity separately. Compose derives the gateway’s slots from
`LLAMA_PARALLEL` so it does not intentionally send more concurrent inference
than the backend can execute. Other deployments must keep those values aligned.
Multiple gateway replicas would multiply these local limits; shared capacity
coordination is not implemented.

Acquire after authentication, request-rate limiting, and basic chat validation,
but before prompt counting, token reservation, or durable admission. This avoids
quota holds and backend counting work for overloaded requests. Holding the slot
during preparation is conservative: slow Redis or token counting can leave an
inference slot idle, but does not allow unbounded preparation behind the backend.
Model listing and health checks bypass inference admission.

Use a small configurable waiting capacity and wait deadline. Defaults are starting
bounds for the single-slot development backend, not measured optimal settings.
Allow zero waiting capacity for immediate load shedding. Full queues return 429
with a one-second `Retry-After` hint; expired waits return 503. The hint is not a
capacity reservation or guarantee. Canceled waiters consume no inference capacity.
[Manual testing](manual-testing.md#configuration) owns the numeric defaults.

Release the slot when proxying ends, including aborts, before final accounting.
Slow final writes must not retain backend execution capacity. Quota settlement
can still be pending when the next request starts preparation; its existing
reservation remains charged until settlement completes. Use idempotent release
and atomic waiter handoff so cancellation or expiry racing a grant cannot leak
a slot. Waiting is not durable: a process crash drops the waiting HTTP requests
without creating inference records or token reservations.

## Backend readiness and periodic health

The Phase 2 registry supersedes Phase 1’s probe-per-readiness-request design.
Probe each configured backend’s `/health` endpoint periodically, preserving its
base path. Use the model server’s health endpoint rather than model listing:
a reachable server may still be loading its model.

Start each backend unhealthy. Require consecutive successes to mark healthy and
consecutive failures to mark unhealthy, resetting the opposite streak on each
result. This avoids changing readiness after every transient failure, at the cost
of delayed detection and recovery. Durations and thresholds are configurable;
[manual testing](manual-testing.md#configuration) owns their defaults.

Only HTTP 200 is successful. Do not follow redirects, forward caller data, or
expose backend responses. Close bodies after headers without buffering. Each
probe has an independent deadline. One worker per backend bounds concurrency and
keeps a slow backend from delaying another. Startup owns cancellation and joins
workers before releasing the transport.

`GET /v1/readyz` reads shared health and returns plain `ok` with 200 when any
backend is healthy, otherwise a fixed OpenAI-compatible 503. It creates no
upstream work and does not inherit a caller’s cancellation into shared probes.
An unset backend remains unready; liveness stays independent. Current chat
forwarding does not yet use health to gate or select inference backends.

## Route authentication

Protect model listing and both chat response modes with one active API key in
`Authorization: Bearer <key>`. Health and readiness remain public. Authenticate
inside the matched route, after request logging and before chat validation or
proxying. Unknown routes and unsupported methods retain their routing errors.
All active keys have the same route permissions. Request and token limits are
carried with identity. The separate request limiter below enforces request rates;
the [daily quota policy](#daily-input-and-output-quotas) enforces token budgets.

Resolve keys through the cache policy below, with `KeyStore.LookupKey` on misses.
Missing, malformed, ambiguous, unknown, and revoked credentials return a generic
JSON `401` with `WWW-Authenticate: Bearer`. On cache misses, database failures
and lookup deadlines return a fixed JSON `503`. Fail closed and never forward
rejected requests. Client cancellation stops the lookup and does not attempt an
error response.

Attach the returned key identity and limits to the request context, never the
secret or hash. Preserve the incoming cancellation and logging metadata. Give
each lookup its own deadline and cancel it before entering the next handler;
that deadline must not limit the lifetime of an authenticated stream.
Strip Authorization from outbound model and chat requests in the shared proxy.
Never forward gateway credentials as backend credentials or include secrets in
logs, errors, or configuration diagnostics. Public deployments need HTTPS.

Startup requires a database URL, owns one bounded pgx pool, verifies connectivity
with a bounded deadline, and closes the pool after HTTP shutdown. Migrations
remain explicit. A local key-creation command uses the same configuration and
store operation and prints the raw secret once after successful insertion.
Default to at most 10 pool connections, a 5-second startup/connect timeout, and
a 2-second lookup timeout. Environment settings can override these positive
limits. Connections are acquired for short database operations and released
before inference; a long stream does not hold a database connection. These are
initial development limits, not measured capacity claims. The local creation
command uses the same operation timeout for its single insertion.

Admin key management has its own listener policy below. The cache policy below
bounds database-outage availability and freshness.

## Revocation and active requests

Use `DELETE /v1/admin/keys/{id}` on the private listener to persist revocation
and cancel the key's active requests in this process. Authentication and the
admin handler share one `Authenticator`; the admin route must call its
`RevokeKey`, not the store directly. Only a successful store update triggers
cancellation. A failed update leaves tracked requests alone. After successful
persistence, cancellation still runs if the operator disconnects before the reply.

The authenticator registers each admitted request's cancellation function under
its key ID. Its handler removes the entry and releases the context on every
exit, including panic and client disconnect. The registry retains active
requests only. It keeps no permanent revoked-ID set or background goroutines.

Snapshot a shared generation counter before each lookup. After a successful
lookup, check that generation and register the request under the same mutex.
A committed revocation increments the generation and copies cancellation
functions for the target key while holding that mutex. Invoke those functions
after unlocking. Completed requests remove themselves independently, and
concurrent revocations can signal requests that have not exited yet.

A lookup crossing a published revocation retries within its original lookup
deadline. This also rechecks unrelated in-flight lookups, a small extra database
cost when an operator revokes a key. Lookups registered before publication are
included in cancellation; stale lookups returning afterward cannot bypass it.
The mutex never covers database calls or inference work. This avoids blocking
all authentication behind one slow database operation and avoids retaining
tombstones indefinitely. The cache policy below extends this coordination to
cached identities.

Return `204` with no body after persistence and local cancellation signaling.
Repeated revocation preserves the first timestamp and returns `204`. Unknown
IDs return `404`; noncanonical or nonpositive decimal IDs return `400` after
operator authentication. Default `GATEWAY_ADMIN_REVOKE_TIMEOUT` to `2s` for
the storage operation. Failures or deadlines return safe `503` responses.
A timeout or lost response can leave the operator uncertain whether the database
committed; retrying this idempotent endpoint is safe.

Cancellation interrupts upstream work and an already-started stream. It cannot
retract delivered tokens or replace a response status already sent. An interrupted
stream has no normal `[DONE]` marker. The `204` confirms cancellation was signaled,
not that every handler or GPU task has already exited. Verify backend termination
separately. Another key's requests remain active. Direct database updates and
revocation in another process do not notify this registry automatically.

Multiple gateways need revocation notifications and recovery from missed
messages. Each process owns only its local active requests. A strict stop deadline
requires an explicit failure policy for partitions and notification outages.
Estimate registry size from admitted request rate times request duration, then
measure memory and cancellation latency. The earlier conversation's sizing
numbers were illustrative, not measured gateway claims. Admission bounds belong
to the planned overload-protection work.

## Authentication caching

Cache successful identities in the shared authenticator. Index entries with
SHA-256 of the presented secret and retain only the identity, limits, hash, and
expiry. Never retain the raw secret or cache lookup failures. Keep the existing
constructor as the sole construction path, with explicit lookup and cache limits.

Default to a 30-second fixed TTL and 1,024 entries. Both limits must be positive
and can be configured through `GATEWAY_AUTH_CACHE_TTL` and
`GATEWAY_AUTH_CACHE_CAPACITY`. Start expiry before database lookup, so a slow
lookup cannot extend freshness. Hits and concurrent fills do not renew an
unexpired entry. A lookup that finishes after its cache lifetime can authenticate
that request but does not populate the cache.

Evict the oldest inserted entry when full. FIFO keeps cache hits and eviction
simple without tracking access order. A map and linked list bound both indexing
and eviction state. Expired entries are removed on access; idle expired entries
remain within the capacity bound. There are no cache workers or per-key timers.
This capacity is an initial development limit, not a measured memory claim.

Unexpired hits work during a Postgres outage. Misses and expired entries must
query Postgres and return `503` on failure or deadline. Never extend expiry or
serve stale entries because storage is unavailable. Successful recovery refreshes
identity and limits. Startup still requires Postgres, and admin operations still
require a successful database write. This policy trades a bounded freshness
window for cached-key availability; it does not guarantee uninterrupted access
through a database outage.

Cache hits register active requests under the same mutex used by revocation.
Successful revocation removes every cached identity for that key and advances
the generation before taking its cancellation snapshot. Database fills must
pass the generation check before caching and registering. A failed revocation
changes neither cached identities nor active requests. No database operation or
cancellation callback runs under the mutex.

The guarantee remains local to one gateway process and its admin API. Direct SQL
changes become visible on the next successful refresh after expiry or eviction;
they do not cancel active work. Cache expiry also does not cancel admitted
requests. Multiple gateways need a separate invalidation and notification policy.

## Operator-local admin key creation

Expose `POST /v1/admin/keys` only on a separate loopback listener in the same
process. The public router never registers admin routes. A token on the public
listener would allow anyone holding a leaked token to create keys remotely.
Separating reachability and permission addresses that concern without relying
on an ingress configuration that this project does not yet provide. A Unix
socket would add filesystem permission and container-mount management; keep
TCP loopback for the current host deployment.

Admin HTTP is disabled by default. Explicit enablement requires a separate
operator token. Accept only a literal loopback IP and numeric port, rejecting
wildcard binds and hostnames so name resolution cannot change the boundary.
Default the address to `127.0.0.1:8082`. Generate a random operator token outside
the gateway and supply it through configuration. Require 32 to 256 URL-safe
ASCII characters; the length check does not establish randomness. Rotate by
changing configuration and restarting. Inference keys grant no admin permission.

Authenticate before reading or validating the request body. Compare fixed-size
SHA-256 digests with a constant-time comparison. Require JSON with a name and
positive request/token limits, reject unknown fields and trailing JSON values,
and bound the body and database insertion. Default the body limit to 4096 bytes
and the insertion deadline to 2 seconds, both configurable. The admin server
uses the existing request read, header, and idle deadlines. Its response write
deadline is the read timeout plus the larger creation or revocation timeout;
reject overflow rather than
silently disabling that deadline. Public SSE retains no write deadline.
Reuse `KeyStore.CreateKey`, return `201` with identity and the raw key once,
and set `Cache-Control: no-store`.
Log the created key ID with the request ID for auditing and lost-response cleanup.
Never log the admin token, issued key, body, or raw database diagnostics. A
committed insertion whose response is lost may leave a key the operator cannot
recover; do not retry automatically or store plaintext secrets to recover it.

Both enabled listeners must bind before either serves. A bind failure closes
previous listeners. An unexpected server exit stops its peer. Drain both servers
concurrently under one shutdown deadline, force-close overdue connections, and
join serving and shutdown goroutines before releasing the shared database pool.
The [accounting cleanup window](#controlled-shutdown-accounting) also applies to
both listeners. The number of lifecycle goroutines is bounded by enabled servers.

In host development, remote operators use SSH forwarding to host loopback;
local processes still need the token. Loopback does not protect against a
compromised host or unintended request forwarding. In Compose, admin HTTP stays
on gateway-container loopback and is disabled by default. Operators run HTTP
commands inside that container, or use its key-creation command. No admin port
is published and the loopback-only configuration rule remains unchanged.
Creation and revocation use the same operator-token boundary.

## Per-key request limits in Redis

Use one token bucket per API-key ID for authenticated models and chat, including
HEAD model requests. Authentication runs first; request admission runs before
chat validation and forwarding. Public probes, unknown routes, unsupported
methods, and private admin operations do not consume request credits. A credit
counts an admitted HTTP request, not a generated model token. No credit is
refunded after admission, including validation failures, backend errors, client
disconnects, or revocation.

Bucket capacity is `requests_per_min`; refill is continuous at
`requests_per_min / 60` credits per second. New or expired buckets start full.
This permits a burst up to the full capacity and limits the sustained rate; it
is not an exact sliding-window count. Both routes share the same Redis key
`llmgate:requests:v1:<key_id>`. Gateways serving the same database must share the
same Redis database. Independent installations need separate Redis databases.

Execute refill, admission, decrement, and expiry in one embedded Lua script.
Use Redis `TIME` to avoid differences between gateway clocks. Clamp backward
clock movement to the bucket's last timestamp and refill at most to capacity.
Expire each bucket after one idle minute of refill. If the clock has moved
backward, both expiry and retry guidance include the delay until its prior
timestamp. This prevents premature expiry from granting a fresh full bucket. Lower configured limits clamp stored credits instead of
resetting the bucket. Limits cached by authentication can remain stale until
refresh, as documented above.

Lua uses doubles. Store credits with 17 significant digits and reject limits
outside `1..2^53-1` with `503` rather than silently losing decrements. Normal
key creation still accepts positive int64 limits; operators must keep request
limits within that range for admission. This slice does not add key editing.

Return the shared JSON `429` format with `Retry-After` rounded up to whole
seconds and at least one second. The delay predicts the next credit, not a
reservation for the caller. Redis errors, script failures, malformed results,
and decision deadlines return a safe JSON `503` without forwarding. This
fail-closed policy protects GPU work from unlimited requests during Redis
outages. Existing admitted streams continue, and health/readiness retain their
existing process/backend meaning.

Use go-redis for pooled Redis connections, TLS, and script execution rather
than maintaining a Redis wire-protocol client. Default to 10 connections, a
2-second startup/connect deadline, and a 500ms decision deadline. Configure
both the base pool and the hard connection maximum, disable the unused pipeline
pool, and honor context deadlines. Disable automatic command retries because a
lost response may follow a committed decrement. Such a request returns `503`
and may already have spent one credit. A `NOSCRIPT` response can safely fall
back from `EVALSHA` to `EVAL`, since the first call did not run the script.
See [Redis scripting](https://redis.io/docs/latest/develop/programmability/eval-intro/)
and [go-redis production guidance](https://redis.io/docs/latest/develop/clients/go/produsage/).

Startup requires successful Redis connectivity before binding listeners. It
owns the client until HTTP draining ends, then closes it. Local key creation
connects only to Postgres. The Compose Redis service binds localhost only,
uses a persistent volume and AOF with one-second fsync, and caps data memory at
128 MiB with `noeviction`. Memory pressure fails admission instead of evicting
live buckets and granting fresh bursts. AOF recovery can lose recent credits
on an unclean failure; request limits are not a durable billing ledger. Full
Gateway/backend Compose assembly is now defined below. Daily token accounting uses
the same Redis connection pool, as described below.

## Inference usage recording

Persist one final record for each validated chat attempt that reaches the proxy,
including upstream failures, cancellation, and incomplete responses. Model lists
and requests rejected before inference remain outside this ledger. Correlate
records with gateway-generated request IDs, unique in Postgres; retain key
identity after revocation and do not store prompts or response bodies.

The original bounded final Postgres write and later per-request Postgres
admission transaction are superseded by the
[shared accounting queue](#durable-accounting-admission-policy). Observation and
outcome classification remain unchanged. Final handoff still has its own
deadline, independent of client cancellation, and never rewrites an already
started inference response.

Abhishek chose unknown token counts plus observed SSE chunk counts over charging
chunk-based estimates. Store both token counts as NULL unless the backend
provides a valid usage pair; preserve explicit zero counts. A transport read or
SSE event is not a model token. Keep observed complete SSE data-event counts
separately for diagnosis. The [daily quota policy](#daily-input-and-output-quotas)
now defines charging for missing and interrupted usage. Chunk-based charging in
the original Phase 1 outline is superseded by retained token reservations.

Observe accepted response bytes without rewriting them or buffering a complete
stream. Bound JSON capture and each SSE line/event. If metadata exceeds those
bounds, keep forwarding and leave it unknown. Parse SSE framing across writes,
including multiline data, CR/LF boundaries, comments, and terminal markers, as
specified by the [SSE standard](https://html.spec.whatwg.org/multipage/server-sent-events.html#parsing-an-event-stream).
Keep proxy abort panics intact after finalization, and forward response-control
operations through the underlying writer. See
[architecture](architecture.md#usage-records-and-missing-information) for the
record fields, timing, and outcome definitions.


## Daily input and output quotas

Count both logical input and generated output tokens, including cached input.
Use a UTC calendar day determined by Redis `TIME`. Charge a request to its
admission day even if it finishes after midnight. This is a serving limit,
not a durable billing ledger. Redis AOF can lose recent state after a crash;
flushing or losing the Redis database resets budgets. Share one Redis database
across gateway instances serving the same key database.

Abhishek chose a configurable grace percentage, defaulting to zero. New work
requires spent plus outstanding reservations to remain below `tokens_per_day`.
An admitted request can use the shared ceiling of that limit plus its grace.
Round fractional grace tokens down. Support integer percentages from 0 to 100
and daily ceilings up to `2^53-1` for exact Lua arithmetic. Outstanding
reservations can temporarily block admission before completed usage reaches
the limit. Grace is shared by the key, not granted separately to each request.

Before inference, render the complete text-chat request through the same
backend's `/apply-template`, then call `/tokenize` with `add_special` and
`parse_special` both true. These paths match llama.cpp b11681's chat parser and
inference tokenizer. The pinned GLM model was checked with ASCII and Hindi
input, a system message, and cache hits. This does not establish support for
other model templates or backend versions. Media and multiple completions are
rejected until their accounting paths are supported. The counting calls have a
shared deadline and bounded responses. They never receive the gateway API key.
See the [pinned backend parser](https://github.com/ggml-org/llama.cpp/blob/b11681/tools/server/server-context.cpp)
and [generation schema](https://github.com/ggml-org/llama.cpp/blob/b11681/tools/server/server-schema.cpp).

One Lua operation reserves the exact input count plus the smaller of requested
output and available output budget. A prompt leaving no room for one output
token is rejected before inference. Normalize generation-limit aliases to one
capped `max_tokens`, and request final stream usage. Let the backend stop at the
cap and return generated text with its normal length finish reason. The gateway
does not cut the connection to enforce a token boundary or treat SSE chunks as
tokens. A backend that violates its generation cap is outside this guarantee;
usage mismatches log an error and never grant a refund below the reserved amount.

After a completed response with valid usage, reconcile to input plus output.
Canceled, failed, interrupted, unknown, or unrepresentable usage keeps the full
allowance. Reconciliation removes the reservation marker exactly once. It has
its own deadline independent of client cancellation and the Postgres write.
Failures are logged and do not change the response or trigger retries. A lost
admission reply or process crash can leave a conservative charge until midnight.
Late settlement cannot refund a new day's account. State expires at the next UTC
midnight, with one counter and only pending markers per active key.

Exhaustion returns `429` with `Retry-After` until the next UTC day. Redis and
counting failures return `503` before inference. Unsupported accounting inputs
return `400`. Already admitted responses keep their allocated cap during a Redis
outage. Models and probes spend no token quota; the separate request-rate bucket
still applies to models and chat. No new Postgres migration is required.


## Initial Prometheus metrics

Use the official Prometheus Go client and one private registry per gateway
process. It supplies concurrency-safe collectors, histogram semantics, label
escaping, and exposition negotiation. A handwritten exporter would duplicate
those responsibilities. Do not register global or runtime collectors in this
slice. See [Prometheus instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/)
and the [official Go client](https://github.com/prometheus/client_golang).

Export the Phase 1 families from the intent document. Request counters retain
`key`, `backend`, and `status`. Histograms retain the backend label, and the
active-stream gauge describes proxy work including prefill. Request duration
includes final accounting, while active streams end before accounting. Reuse
the bounded SSE observer's TTFT definition instead of parsing the stream twice.
The chat handler owns metric lifecycle and quota settlement; usage observation
supplies the outcome and a first-data notification without depending on either
package. Usage persistence remains a separate bounded operation.
Record completed, rejected, canceled, and aborted public handler attempts.
A wire status of 200 can accompany an interrupted response; usage rows own the
richer outcome classification.

Raw keys, request paths, request IDs, and model names never become labels. Bound
per-key labels to a configured capacity, default zero (all keys share `other`),
with no eviction or counter reuse. Additional identities share `key="other"`; unauthenticated attempts use
`key="none"`. This preserves aggregate counts with bounded memory even when
operators create many keys. The initial single-backend labels were `primary`
and `none`; the [Phase 2 routing policy](#local-least-loaded-backend-routing)
replaces this with configured names and `none`. Metrics reset on process restart,
not at midnight.

Scrapes are public on `/v1/metrics`, excluded from request metrics, and independent
of authentication, Postgres, Redis, and backend health. Bound simultaneous
scrapes and scrape duration. The separate operator listener is not instrumented.
The Compose gateway publishes only on host loopback; deployments exposing it
more broadly must also consider who can read key-ID labels. Prometheus/Grafana
services and dashboards remain separate from this initial exporter.

## Complete single-backend Compose definition

Keep existing Postgres and Redis services, named volumes, and the Compose
project namespace. Add the gateway, the CUDA backend, and migration startup
ordering in the same file. Migrations must finish successfully before the
gateway binds; inference startup also waits for healthy backend/storage
services. Later dependency outages keep the existing request-time policies.
Containers restart on process exit, while health checks expose failures. Bound
Docker log storage with rotation so backend logs cannot grow without limit.

Build a static gateway binary in a Go stage and run it as a non-root user on a
read-only Alpine filesystem. Keep credentials out of build inputs with an
allowlist Docker context, and exclude local `.env` files from version control.
The current development credential model uses a private environment file, not
a secret-management service. Daemon access can inspect container environments.

Pin the CUDA backend to the verified b11681 image digest so quota tokenization
does not silently change. Mount the separately verified GGUF read-only and
refuse to create a missing host model path. Use NVIDIA GPU device reservation;
do not silently fall back to CPU. The backend gets no host port. Existing
Postgres/Redis development ports and the gateway publication remain loopback.
Admin HTTP remains loopback inside the gateway container. No wildcard admin
binding or privileged gateway is needed. See [Compose GPU support](https://docs.docker.com/compose/how-tos/gpu-support/)
and [dependency conditions](https://docs.docker.com/reference/compose-file/services/).

The [deployment guide](manual-testing.md#compose-deployment) owns host
prerequisites, setup commands, and the current verification limits. A GPU
visibility smoke check does not establish inference in the pinned backend image
or successful full-stack startup.


## Controlled shutdown accounting

A forced connection close cancels inference but does not wait for HTTP handlers
or their deferred accounting. Joining only serving/shutdown goroutines allowed
process exit to race Redis settlement and Postgres inserts.

Track complete handlers across the public and admin listeners. Close admission
atomically with registration, drain normally, force-close overdue connections,
then wait under a separate cleanup deadline before releasing dependencies. Use
a shared counter and completion channel so a timed-out wait leaves no waiting
goroutine behind. Preserve abort panics and the existing drain-deadline error.
Report cleanup timeout with the remaining handler count.

Keep the normal drain window intact. Extending that window alone would only
postpone the race; reserving cleanup inside it would shorten graceful request
completion. A separate cleanup budget permits a longer but bounded stop. Its
default covers the sequential Redis-settlement and usage-write deadlines with
some margin. The pinned pgx driver can separately wait up to fifteen seconds
for failed-connection cleanup, so Docker's stop grace also allows that teardown
instead of killing the process while the driver is closing sockets. Missing
storage or an exhausted cleanup budget still prevents a durability guarantee.

The later [durable accounting path](#durable-accounting-admission-policy) covers
process crashes and Postgres outages. Controlled shutdown still needs this cleanup
window to preserve an observed final outcome before abandoning request ownership.


## Durable accounting admission policy

Use shared Redis for durable admission and known final outcomes, then deliver
records to Postgres in batches. This supersedes a synchronous Postgres admission
transaction per inference request. It keeps batch timers and Postgres writes out
of the inference path without making accounting local to one gateway replica.
The design follows the separation described in
[LiteLLM's production guidance](https://docs.litellm.ai/docs/proxy/prod#batch-spend-writes),
while retaining our stronger disk-acknowledged handoff requirement.

Abhishek chose stronger durability over an acknowledged one-second loss window.
Require AOF `always` and `noeviction`; use `WAITAOF 1 0` on the same reserved Redis
connection after each admission/final write. This acknowledges local disk
persistence, not survival of disk loss or promotion of a stale replica. Redis
must honor filesystem syncs. See [WAITAOF](https://redis.io/docs/latest/commands/waitaof/).
Persistence adds latency and applies to quota/rate writes on this shared Redis
instance too. The [final baseline](benchmarks.md) measures the complete path; it
does not isolate disk-sync cost from prompt counting, proxying, or network paths.

Reserve two fixed-size 8 KiB slots per request before generation. One retains
admission metadata; the other is preallocated for the final outcome. A shared
pending-request cap includes active attempts and undelivered outcomes. Keeping
active slots after admission delivery prevents new work from consuming their
final-result capacity. Finalization only replaces reserved values, including
under Redis memory pressure. This bounds queue payload allocation.

A separate AOF budget stops protected
traffic before rate-limit writes when current AOF bytes reach the budget minus
Redis maxmemory and four slot sizes per pending request. At defaults, 256 MiB
minus 128 MiB minus 32 MiB leaves a 96 MiB admission threshold. The reserve leaves
room for a rewrite and final outcomes. Background checks and concurrent writes
mean this is an admission threshold, not a hard filesystem quota. Operators must
provision disk capacity for Redis, rewrite scratch space, and backups. Actual
ENOSPC fails closed: a handoff without a disk acknowledgment never authorizes
generation.

Postgres retention defaults to 30 days from request start, swept hourly in
bounded transactions. Each full batch is followed immediately by another, so
cleanup can catch up with incoming traffic. Only usage rows and their matching
admissions are removed; unfinished admissions and keys survive. Eligibility age
and cleanup lag are explicit, rather than promising a fixed physical database
size. Redis removes delivered final records only after settlement succeeds.

One leased writer commits at most the configured batch size per transaction.
It acknowledges only unchanged Redis records after Postgres commit. A final
outcome arriving during admission delivery remains queued. Lost acknowledgments
cause retry-safe replay; a conflict pauses admission rather than dropping data.
Full batches drain immediately; the background timer bounds idle polling.

Keep the agreed rejection policy: no new inference once Postgres delivery fails.
The writer removes its short-lived shared health marker on failure. If it dies,
that marker expires after twice the write timeout plus twice the flush interval.
This is failure-detection time, not an optional serving grace period. Already
admitted requests can still save final outcomes. Redis failure or full capacity
also returns `503` before generation. Cached model listing is unaffected.

Persist the final outcome before attempting quota refunds. Failed handoff keeps
the allowance pending and charged. The writer repeats settlement after Postgres
commit and before Redis acknowledgment, using the same idempotent operation and
original UTC day as request cleanup. A settlement failure retains the queue record
for retry; a lost acknowledgment cannot refund twice.

Each gateway maintains an expiring ownership identity and registers active
request cancellation under that identity. Lease loss cancels local generation;
expired identities cannot renew or submit a late final result. After expiry, a
worker reconstructs missing finals as interrupted with unknown counts and retains
the conservative quota charge. `recovered = true` distinguishes their admission-
to-recovery interval from an observed duration. Waiting for the ownership lease
avoids declaring a live request abandoned merely because its admission reached
Postgres. Known final outcomes are replayed without replacing them with recovery
estimates. A gateway that resumes after suspension must establish a fresh epoch.


## Retry-safe usage delivery

A final usage record is immutable under its request ID. Identical delivery is
acknowledged; different accounting data returns `store.ErrUsageConflict` rather
than being discarded or overwriting the existing values. Compare every stored
field, treating unknown counts as distinct from known zero.

Use one atomic conflict-checked statement so concurrent delivery cannot race a
separate existence lookup. An identical retry performs a no-op update, which
creates a PostgreSQL row version but changes no accounting values. This is a
small retry-path cost for an atomic comparison. See
[Postgres conflict handling](https://www.postgresql.org/docs/current/sql-insert.html)
and [NULL-safe comparison](https://www.postgresql.org/docs/current/functions-comparison.html).
The batch writer uses the same conflict checks for admission and final usage.
Queued records replay after restart, including fenced abandoned-attempt recovery
and retryable quota settlement. Retention keeps both the admission and final row
for the configured eligibility period.


## Local least-loaded backend routing

Choose among healthy backends using the fewest local assignments, including
waiting, preparation, and inference. Rotate equal-load choices through configuration
order. Select and increment under one mutex before entering the backend queue.
Using a queue snapshot alone would let simultaneous requests all select the same
idle server before any of them entered its queue. Bound assignments by that
backend's slots plus waiting capacity and release them before final accounting.

This is an absolute request count, not normalized capacity, prompt cost, or GPU
utilization. It keeps this phase understandable, but differently sized servers or
long prompts can have unequal work despite equal counts. Counts and queues are
per gateway process; several gateways pointing at one server do not share this
capacity limit.

Keep counting, inference, and usage identity in the selected backend bundle.
Shared Redis reservations still enforce one daily budget across all targets.
All configured servers should expose the same model aliases. Model listing
selects one healthy server without inference admission rather than merging
catalogs. Prefix/model-aware routing and heterogeneous model pools are outside
this slice.

Health gates new selections using the existing probe thresholds. Already assigned
requests, including waiters, stay on their original server. No healthy backend
returns `503`; full capacity across healthy backends returns `429`. Requests are
not retried on another backend yet. A retry will need to preserve the response
commit boundary and account for uncertain consumption separately.

Accept a bounded named JSON list through `GATEWAY_BACKENDS`, while translating the
existing single URL to one `primary` target. Reject both settings together rather
than silently choosing one. Backend names become fixed metric labels, with a
maximum of 16 targets and `none` reserved for work without a selected backend.
