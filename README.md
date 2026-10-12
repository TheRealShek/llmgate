# llmgate

`llmgate` is an OpenAI-compatible LLM gateway written in Go. It accepts HTTP
requests and forwards them to one inference server, such as llama.cpp. The
backend loads the model and generates tokens; the gateway handles HTTP routing,
request validation, cancellation, and deadlines.

Phase 1 is in progress. Health checks, model listing, JSON and streaming chat,
and request IDs with structured logs work today. Authentication, rate limits,
usage tracking, metrics, and Docker Compose are still planned. Benchmarks are
not available yet.

## Run locally

Use Go 1.27 or later and an already-running OpenAI-compatible backend. Set the
backend's base URL without the `/v1` suffix:

```sh
GATEWAY_UPSTREAM_URL=http://127.0.0.1:8081 go run ./cmd/gateway
```

The gateway listens on port 8080 by default. In another terminal:

```sh
curl http://localhost:8080/v1/healthz
curl http://localhost:8080/v1/models

# Replace MODEL_ID with an ID returned by /v1/models.
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}],"stream":false}'

# Stream tokens as the backend generates them.
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Without `GATEWAY_UPSTREAM_URL`, the process still starts. Health returns `200`,
while model and chat requests return `503`. Health checks only the gateway
process, not backend availability.

## How a request works

```mermaid
flowchart LR
    Client --> Routes[Go HTTP routes]
    Routes --> Health[Health response]
    Routes --> Chat[Chat validation]
    Routes --> Proxy[Reverse proxy]
    Chat --> Proxy
    Proxy --> Backend[Inference server]
```

1. Go's `http.ServeMux` matches the method and path. The gateway exposes
   `GET /v1/healthz`, `GET /v1/models`, and `POST /v1/chat/completions`.
2. For chat, the handler reads the body into a bounded buffer. It requires
   `application/json`, a JSON object, and a boolean `stream` field when present.
   `stream: true` selects SSE forwarding. Explicit `max_tokens` values must be
   positive integers within the configured limit.
3. The handler preserves backend-specific fields and inserts the configured
   `max_tokens` limit when omitted. The backend validates the model, messages,
   and other generation options.
4. The standard library's `httputil.ReverseProxy` sends the request upstream and
   copies the backend's status, response headers, and body back to the client.
   Model-list requests use this proxy directly.

The proxy derives its deadline from the incoming request's Go `context`, which
carries cancellation signals. A client disconnect cancels upstream HTTP work.
For JSON responses, the upstream timeout covers waiting for headers and reading
the response body. Streams have separate connect, header, and idle-read timeouts
and no total deadline. Each copied stream chunk flushes immediately. Before
headers arrive, a timeout returns `504` and a connection failure
returns `502`. After a response starts, a failure ends the incomplete response;
the gateway cannot replace a status it has already sent. Backend HTTP errors
pass through unchanged.

Every request gets a generated `X-Request-ID` response header. The gateway sends
the same ID to the backend and attaches it to request and proxy-failure logs.
Logs go to stderr as JSON and omit prompts, API keys, and raw URLs. The finish
log includes elapsed milliseconds and `completed`, which means the handler
returned normally, even for an HTTP error. An aborted stream can log
`completed: false`; its HTTP status may already have been sent.

On `SIGINT` or `SIGTERM`, the gateway stops accepting connections and gives
active requests time to finish. It closes remaining connections when the
shutdown deadline expires.

## Configuration

| Environment variable | Default | Controls |
| --- | --- | --- |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GATEWAY_UPSTREAM_URL` | Unset | Backend base URL |
| `GATEWAY_UPSTREAM_TIMEOUT` | `30s` | Total deadline for model lists and JSON chat |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | `5s` | TCP connect and TLS handshake deadlines |
| `GATEWAY_UPSTREAM_HEADER_TIMEOUT` | `30s` | Backend response-header wait |
| `GATEWAY_STREAM_IDLE_TIMEOUT` | `30s` | Each upstream stream-body read wait |
| `GATEWAY_MAX_BODY_BYTES` | `1048576` | Chat request body limit |
| `GATEWAY_MAX_TOKENS` | `1024` | Output-token limit and omitted-value default |
| `GATEWAY_READ_TIMEOUT` | `10s` | Incoming headers and body read deadline |
| `GATEWAY_READ_HEADER_TIMEOUT` | `5s` | Incoming header read deadline |
| `GATEWAY_IDLE_TIMEOUT` | `60s` | Wait for the next request on an idle client connection |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown deadline |

Oversized chat bodies return `413`, invalid JSON returns `400`, and unsupported
content types return `415`. Duration settings accept Go values such as `500ms`
or `30s` and must be positive.

## Read the code

Start with [startup](cmd/gateway/run.go), then follow the
[request logs](internal/requestlog/handler.go), [routes](internal/gateway/handler.go),
[chat validation](internal/gateway/chat.go), and [proxy](internal/proxy/handler.go).
See [streaming](internal/proxy/stream.go) for idle-read cancellation and
[shutdown](cmd/gateway/server.go) for
connection cleanup. The implementation uses only the Go standard library.

Run the tests with:

```sh
go test ./...
```

[Design decisions](docs/decisions.md) records the current tradeoffs.
[Project intent](docs/PROJECT_INTENT.md) describes the remaining Phase 1 work
and later plans for multiple backends and prefix-aware routing.
