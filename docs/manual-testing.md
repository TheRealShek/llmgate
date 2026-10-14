# Manual testing with llama.cpp

These commands reproduce the Phase 1 checks from our October 14, 2026 session.
Use three terminals so backend logs, gateway logs, and client output stay visible.

The backend listens on host port 8081. The gateway listens on port 8080.
Both host ports are bound to localhost.

## 1. Start the backend in terminal one

Docker is installed on the test machine, but the session could not access its
socket without sudo. The first run downloads the image and model. The named
volume keeps the model after the container exits.

```sh
sudo docker run --rm --init \
  --name llmgate-backend \
  -p 127.0.0.1:8081:8080 \
  -v llmgate-model-cache:/models \
  -e LLAMA_CACHE=/models \
  ghcr.io/ggml-org/llama.cpp:server \
  -hf Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M \
  --host 0.0.0.0 \
  --port 8080 \
  --ctx-size 2048 \
  --parallel 1 \
  --threads 6
```

Keep this terminal open. The container listens on port 8080 internally; Docker
maps it to host port 8081. The image's `server` tag can change between runs.
This guide is a functional check, not a version-pinned benchmark.

References: [llama.cpp Docker instructions](https://github.com/ggml-org/llama.cpp/blob/master/docs/docker.md)
and [the Qwen model](https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF).

## 2. Check the backend and start the gateway in terminal two

```sh
curl -i http://127.0.0.1:8081/health
```

Wait until this returns HTTP 200 before starting the gateway.

```sh
cd /Drive2/Coding_Skills/Projects/llmgate

GATEWAY_ADDR=127.0.0.1:8080 \
GATEWAY_UPSTREAM_URL=http://127.0.0.1:8081 \
go run ./cmd/gateway
```

Expected startup message:

```text
Server listening on 127.0.0.1:8080
```

Keep this terminal open to watch request logs. Request logs remain JSON.

## 3. Check gateway health and model listing in terminal three

```sh
curl -i http://127.0.0.1:8080/v1/healthz
curl -i http://127.0.0.1:8080/v1/readyz
curl -sS http://127.0.0.1:8080/v1/models
```

Health and readiness should return HTTP 200. Health checks the gateway process;
readiness checks the backend's `/health` endpoint.

The model-list response should contain this ID under `data`:

```text
Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M
```

Use the ID actually returned if it differs from the examples below.

## 4. Test streaming chat

```sh
curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M",
    "messages": [
      {"role": "user", "content": "Explain why a gateway needs timeouts in three short sentences."}
    ],
    "stream": true,
    "max_tokens": 128
  }'
```

`-N` disables curl's output buffering. Expect `data:` events to arrive
progressively. Each JSON event contains an answer fragment in
`choices[0].delta.content`. Normal completion ends with a finish reason and
`data: [DONE]`.

Curl displays the raw Server-Sent Events rather than joining the answer fragments.
The repeated JSON metadata is expected.

The earlier version of this command used a shell variable for the model ID:

```sh
MODEL_ID='Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M'

curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL_ID\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain why a gateway needs timeouts in three short sentences.\"}],\"stream\":true,\"max_tokens\":128}"
```

Both versions send the same request.

## 5. Test non-streaming chat

```sh
curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M",
    "messages": [
      {"role": "user", "content": "Explain why a gateway needs timeouts in three short sentences."}
    ],
    "stream": false,
    "max_tokens": 128
  }'
```

Expect one JSON response after generation finishes. The complete answer appears
in `choices[0].message.content`, with token counts in `usage`. The `-N` option
is unnecessary for this mode but remains here to match the session command.

The observed response contained 41 prompt tokens and 116 completion tokens.
Generated text and token counts can differ on later runs.

## 6. Test client-disconnect cancellation

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M",
    "messages": [
      {"role": "user", "content": "Write a detailed story about a long expedition across the ocean."}
    ],
    "stream": true,
    "max_tokens": 1024
  }'
```

Press Ctrl+C in terminal three while tokens are arriving, before generation
finishes. Watch terminal one for backend cancellation and slot release.

The session produced these backend lines:

```text
49.49.307.529 W srv          stop: cancel task, id_task = 193
49.49.336.113 I slot      release: id  0 | task 193 | stop processing: n_tokens = 93, truncated = 0
```

The matching task ID shows that llama.cpp canceled the task and released its
slot. Release happened about 29 milliseconds after the backend logged
cancellation. This does not measure the delay from pressing Ctrl+C.

The gateway logged `completed: false` for the interrupted request. This means
its handler did not return normally. It is not a cancellation flag and does not
establish that model computation stopped without backend evidence.

This check covers disconnection during decode. Disconnection during prefill
still needs a separate check.

## 7. Test an unavailable backend

Press Ctrl+C in terminal one to stop llama.cpp. Leave the gateway running.
Then run these commands in terminal three:

```sh
curl -i http://127.0.0.1:8080/v1/healthz
curl -i http://127.0.0.1:8080/v1/readyz
curl -i http://127.0.0.1:8080/v1/models
```

Expected results:

- Health returns HTTP 200 because the gateway is still running.
- Readiness returns HTTP 503 because the backend is unavailable.
- Model listing returns HTTP 502 when the upstream connection fails.

These commands were proposed in the session, but their results have not yet
been supplied. Repeat the backend command in terminal one to restart it.

## Automated tests

For the full Go test suite:

```sh
go test ./...
```

For the startup and shutdown package checked during the startup-message change:

```sh
go test ./cmd/gateway
```

The gateway package tests passed during the session. These commands test the
code with test backends; they do not replace the real llama.cpp checks above.
