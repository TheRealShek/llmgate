# Manual testing with GLM and llama.cpp

GLM-Edge-4B-Chat Q6_K is the primary model. These commands run the Phase 1
checks through llama.cpp and the gateway. Use three terminals so backend logs,
gateway logs, and client output stay visible. Historical evidence below is
explicitly labeled with the model used at the time.

The backend listens on host port 8081. The gateway listens on port 8080.
Both host ports are bound to localhost.

## 1. Prepare the model and start the backend in terminal one

The model is already downloaded on this machine at
`~/.cache/llmgate/models/glm-edge-4b-chat/ggml-model-Q6_K.gguf`.
The download is 3,554,369,472 bytes. Its source revision and SHA-256 are also
recorded alongside it in `download.json`.

On a fresh machine, download the same official file:

```sh
mkdir -p "$HOME/.cache/llmgate/models/glm-edge-4b-chat"

curl --fail --location --retry 3 --continue-at - \
  --output "$HOME/.cache/llmgate/models/glm-edge-4b-chat/ggml-model-Q6_K.gguf" \
  'https://huggingface.co/zai-org/glm-edge-4b-chat-gguf/resolve/e168d9e95cf9af8bfbe54e04df7123516a51415d/ggml-model-Q6_K.gguf'
```

Before starting the backend, verify the file. This check also works for the
copy already downloaded on this machine:

```sh
printf '%s  %s\n' \
  'd8e83d4f9b6c6c89214d5c1dd17355d44be99173c15efa846d31ce01d1bda1f3' \
  "$HOME/.cache/llmgate/models/glm-edge-4b-chat/ggml-model-Q6_K.gguf" \
  | sha256sum --check
```

Continue only when the checksum reports `OK`. Start the backend with a read-only
mount of the model directory. Stop any existing backend on host port 8081 first.
Docker requires sudo in this session.

```sh
sudo docker run --rm --init \
  --name llmgate-glm-test \
  -p 127.0.0.1:8081:8080 \
  -v "$HOME/.cache/llmgate/models/glm-edge-4b-chat:/models:ro" \
  ghcr.io/ggml-org/llama.cpp:server \
  --model /models/ggml-model-Q6_K.gguf \
  --alias glm-edge-4b-chat \
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
and [the official GLM GGUF model](https://huggingface.co/zai-org/glm-edge-4b-chat-gguf).

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
glm-edge-4b-chat
```

Use the ID actually returned if it differs from the examples below.

## 4. Test streaming chat

```sh
curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-edge-4b-chat",
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

The same command can use a shell variable for the model ID:

```sh
MODEL_ID='glm-edge-4b-chat'

curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL_ID\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain why a gateway needs timeouts in three short sentences.\"}],\"stream\":true,\"max_tokens\":128}"
```

Both versions send the same request.

## 5. Test non-streaming chat

```sh
curl --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-edge-4b-chat",
    "messages": [
      {"role": "user", "content": "Explain why a gateway needs timeouts in three short sentences."}
    ],
    "stream": false,
    "max_tokens": 128
  }'
```

Expect one JSON response after generation finishes. The complete answer appears
in `choices[0].message.content`, with token counts in `usage`.

Generated text and token counts depend on the model and can differ on later runs.

## 6. Test client-disconnect cancellation

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-edge-4b-chat",
    "messages": [
      {"role": "user", "content": "Write a detailed story about a long expedition across the ocean."}
    ],
    "stream": true,
    "max_tokens": 1024
  }'
```

Press Ctrl+C in terminal three while tokens are arriving, before generation
finishes. Watch terminal one for backend cancellation and slot release.

The earlier Qwen run produced these backend lines. They show the expected log
pattern; repeat the command above to collect GLM cancellation evidence:

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

The earlier Qwen run passed these checks. Repeat them with GLM to verify the
current setup. Restart the backend with the command in step 1 and wait for
`/v1/readyz` to return 200. Send another chat request without restarting the
gateway to verify recovery. Recovery also passed in the earlier Qwen run.

## 8. Record a GLM single-stream baseline

With both services running and no other inference traffic:

```sh
python bench/single_stream.py > /tmp/llmgate-glm-single-stream-100.json
```

The default model is `glm-edge-4b-chat`. This sends one warm-up per path, then
100 sequential measured requests, split evenly between direct llama.cpp and the
gateway. See [benchmarks](benchmarks.md) for timing definitions and historical
results. Save GLM measurements separately from the retired model's data.

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
