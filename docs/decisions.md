# Decisions

## Code naming

Each name should describe its actual role or behavior in its package and at the
call site. Use matching names for related paths, such as `NewNonStreamingHandler`
and `NewStreamingHandler`, so the supported response mode is clear. Name timeout
parameters for what they bound, such as `totalTimeout` and `idleTimeout`.

Apply this to constructors, functions, types, fields, and variables. Keep names
concise when context already makes their meaning clear. Comments explain the
request flow and reasoning; names should make sense without reading the implementation.

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
Update Content-Length to match the resulting body. Existing constructor calls
preserve their behavior; startup uses the constructor that receives both body
and token limits.

## Streaming chat forwarding

Streaming requests pass through the same JSON, body-size, and output-token
validation as non-streaming requests. Startup supplies a separate streaming
proxy. Existing constructors without that proxy retain their `501` response
for streaming requests.

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
