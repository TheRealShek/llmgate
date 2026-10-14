# Decisions

## Primary inference model

Use GLM-Edge-4B-Chat Q6_K as the primary CPU model in llama.cpp, following
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
Caching and the HTTP endpoint policy are later decisions.

Build and discuss the key-storage slice before wiring authentication into the
gateway, as agreed in the session. The development Compose file starts only
Postgres and its migration tool at this checkpoint.

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

Use one `gateway.NewHandler(nonStreaming, streaming, readiness, limits)` constructor for
startup and tests. Remove variants from earlier project stages instead of
keeping them as compatibility wrappers inside this internal package.

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
