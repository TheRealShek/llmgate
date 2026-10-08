# Decisions

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
need separate connect, first-response, and idle-gap timeouts in a later Phase 1 task.

## Endpoint versioning

All gateway endpoints use `/v1`, including health checks, metrics, and admin
routes. Future API versions use separate prefixes such as `/v2`. Unversioned
paths and unsupported versions return `404`.

## Non-streaming chat requests

`POST /v1/chat/completions` accepts a JSON object with `stream` omitted or false.
Until the SSE path is implemented, `stream: true` receives `501`. Model, message,
and backend-specific field validation remains the backend's responsibility.

Buffer the request once to validate JSON and the stream flag, then forward the
original bytes. This preserves fields the gateway does not understand. Bound
that buffer with `GATEWAY_MAX_BODY_BYTES`, which defaults to `1048576` bytes.
Oversized bodies receive `413`; invalid JSON receives `400`; unsupported content
types receive `415`.

`GATEWAY_READ_TIMEOUT`, default `10s`, bounds reading incoming request headers
and bodies. It does not limit response streaming. Backend requests retain the
client's cancellation and use `GATEWAY_UPSTREAM_TIMEOUT` for their total deadline.
