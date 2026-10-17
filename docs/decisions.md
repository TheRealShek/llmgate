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

Keep setup commands in the manual testing guide, implementation reading order
in the README, design choices here, and phase requirements in the project intent.
Keep measured results and their raw data in the existing benchmark files.
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

Pass the response writer through unchanged. This preserves flushing and HTTP
abort behavior without implementing a response-writer wrapper. Status and byte
count collection are separate work.

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
The README follows one streaming request through the functions that run it.

Keep both response modes in the `proxy` package because both forward requests
to the backend. `nonstream.go` owns the total-deadline handler; `stream.go` owns
the streaming handler and idle-read wrappers. `forward.go` owns reverse-proxy
setup and error logging shared by both modes. Group code by its responsibility
instead of grouping all types that implement `http.Handler` into one package.

## Single-backend readiness

`GET /v1/readyz` probes the configured backend's `/health` endpoint on demand,
preserving the backend base path. Use llama.cpp's documented health endpoint
instead of model listing because a server can be reachable while its model is
still loading. See the [backend documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md#api-endpoints).

Only backend HTTP `200` means ready. Return plain `ok` with gateway status `200`
on success, and a fixed OpenAI-compatible `503` on any other backend status or
transport failure. Do not follow redirects, pass through backend responses, or
forward caller credentials, bodies, or query parameters. Forward the generated
request ID for tracing. An unset backend returns `503`; liveness remains `200`.

`GATEWAY_READINESS_TIMEOUT`, default `2s`, bounds each probe independently of
chat timeouts. The probe inherits client cancellation and owns its deadline's
cancel function. Close the backend body immediately after receiving headers.
This avoids waiting on a stalled body or buffering arbitrary content, at the
cost of potentially giving up connection reuse when response bytes remain unread.

Probe each time instead of keeping cached health or a background worker. This
keeps the first readiness behavior small, but probe traffic reaches the backend
directly. Readiness does not reserve capacity or block chat forwarding. Phase 2
will add periodic checks and use backend health in routing.

## Route authentication

Protect model listing and both chat response modes with one active API key in
`Authorization: Bearer <key>`. Health and readiness remain public. Authenticate
inside the matched route, after request logging and before chat validation or
proxying. Unknown routes and unsupported methods retain their routing errors.
All active keys have the same route permissions. Request and token limits are
carried with identity but are not enforced by authentication.

Use direct `KeyStore.LookupKey` calls for this slice. Missing, malformed,
ambiguous, unknown, and revoked credentials return a generic JSON `401` with
`WWW-Authenticate: Bearer`. Database failures and lookup deadlines return a
fixed JSON `503`. Fail closed and never forward rejected requests. Client
cancellation stops the lookup and does not attempt an error response.

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

Admin creation now has its own listener policy below. Admin revocation and
authentication caching remain separate tasks.

## Revocation and active requests

For the first authentication slice, revocation blocks subsequent lookups after
the database update commits. Requests already authenticated may finish. A
lookup concurrent with revocation may also admit a request. Authentication is
checked once per request, not once per stream chunk.

Add active cancellation alongside the admin revocation endpoint in a later
slice. For a single gateway, register each admitted request's cancellation
function under its key ID, remove it on completion, and cancel matching requests
only after successful persistent revocation. Coordinate authentication and
registration with revocation so an admission cannot miss cancellation. Do not
hold the registry lock while invoking cancellation functions. Cancellation
must preserve other keys' requests and release entries on every exit path.

Cancellation interrupts upstream work and an already-started stream; it cannot
retract delivered tokens or replace a response status already sent. Verify the
backend actually stops inference. Direct database changes do not automatically
notify an in-memory registry.

Multiple gateways need revocation notifications and recovery from missed
messages. Each process owns only its local active requests. A strict stop deadline
requires an explicit failure policy for partitions and notification outages.
Estimate registry size from admitted request rate times request duration, then
measure memory and cancellation latency. The earlier conversation's sizing
numbers were illustrative, not measured gateway claims. Admission bounds belong
to the planned overload-protection work.

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
deadline is the read timeout plus insertion timeout; reject overflow rather than
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
The number of lifecycle goroutines is bounded by the number of enabled servers.

For now the gateway runs on the host. Remote operators use SSH forwarding to
host loopback; local processes still need the token. Loopback does not protect
against a compromised host or unintended request forwarding. Container loopback
is distinct from host loopback. Resolve admin reachability explicitly when
adding the gateway to Compose; do not silently change to a wildcard bind or
publish the admin listener publicly. Revocation and active-request cancellation
remain the next slice.
