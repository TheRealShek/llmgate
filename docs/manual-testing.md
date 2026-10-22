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

Continue only when the checksum reports `OK`. Normal local inference uses the
RTX 3060 through CUDA. The tested setup uses official native binaries, so it
does not need Docker permissions or a CUDA compiler. Keep the existing NVIDIA
driver. Check that `nvidia-smi` detects the GPU first.

The runtime is already installed on this machine. On a fresh machine with a
working NVIDIA driver, `gh`, and `tar`, download and verify the same release:

```sh
LLAMA_RELEASE_DIR="$HOME/.cache/llmgate/runtime/b11681-cuda12.8"
mkdir -p "$LLAMA_RELEASE_DIR"

gh release download b11681 --repo ggml-org/llama.cpp \
  --pattern '*ubuntu-cuda-12.8-x64.tar.gz' \
  --dir "$LLAMA_RELEASE_DIR" --skip-existing

(
  set -e
  cd "$LLAMA_RELEASE_DIR"
  printf '%s  %s\n' \
    'b920cfbdb0ec6bd46d7f20eeaa7c5d4eb760a73f7359a1e63146025674168863' \
    'llama-b11681-bin-ubuntu-cuda-12.8-x64.tar.gz' \
    '9445563f1ea8bae7acf09448f2da907cda223afd682a86d5fb5ba63b111303cc' \
    'cudart-llama-b11681-bin-ubuntu-cuda-12.8-x64.tar.gz' \
    | sha256sum --check
  tar -xzf llama-b11681-bin-ubuntu-cuda-12.8-x64.tar.gz
  tar -xzf cudart-llama-b11681-bin-ubuntu-cuda-12.8-x64.tar.gz
)
```

Both archive checks must report `OK`. The CUDA libraries stay inside this
cache directory. Stop any existing backend on host port 8081, then start:

```sh
LLAMA_RELEASE_DIR="$HOME/.cache/llmgate/runtime/b11681-cuda12.8"

LD_LIBRARY_PATH="$LLAMA_RELEASE_DIR/llama-b11681:$LLAMA_RELEASE_DIR/cudart-llama-b11681-bin-ubuntu-cuda-12.8-x64" \
"$LLAMA_RELEASE_DIR/llama-b11681/llama-server" \
  --model "$HOME/.cache/llmgate/models/glm-edge-4b-chat/ggml-model-Q6_K.gguf" \
  --alias glm-edge-4b-chat \
  --host 127.0.0.1 \
  --port 8081 \
  --ctx-size 2048 \
  --parallel 1 \
  --threads 6 \
  --device CUDA0 \
  --n-gpu-layers all \
  --log-verbosity 4
```

Keep this terminal open. Startup must show `offloaded 41/41 layers to GPU`.
While generating a response, run this in another terminal:

```sh
nvidia-smi --query-compute-apps=pid,process_name,used_gpu_memory --format=csv
```

Expect a `llama-server` process using about 3692 MiB with this configuration.
The CUDA model buffer is about 3244 MiB, the KV cache is 240 MiB, and compute
buffers plus runtime allocations use more memory. Other applications and the
desktop also use VRAM. Larger contexts or more slots need another memory check.
An HTTP 200 response alone does not establish GPU execution.

For an explicit CPU comparison, restart this same command with
`--device none --n-gpu-layers 0` in place of the two GPU flags. CPU execution
is a comparison or compatibility option; normal local runs use CUDA.

The old `:server` Docker image is CPU-only. A container setup needs the
NVIDIA Container Toolkit, the `:server-cuda` image, and `--gpus all` in addition
to the model offload flags. That container path was not tested in this session;
the native CUDA path above was tested with the real model.

References: [tested llama.cpp release](https://github.com/ggml-org/llama.cpp/releases/tag/b11681),
[llama.cpp Docker instructions](https://github.com/ggml-org/llama.cpp/blob/master/docs/docker.md),
and [the official GLM GGUF model](https://huggingface.co/zai-org/glm-edge-4b-chat-gguf).

## 2. Check the backend and start the gateway in terminal two

```sh
curl -i http://127.0.0.1:8081/health
```

Wait until this returns HTTP 200 before starting the gateway. Initialize
[Postgres](#postgres-key-storage), apply its migration, and start
[Redis](#redis-request-limits). Export the database configuration in this terminal
as described under [authentication](#api-key-authentication).

```sh
cd /Drive2/Coding_Skills/Projects/llmgate

GATEWAY_ADDR=127.0.0.1:8080 \
GATEWAY_UPSTREAM_URL=http://127.0.0.1:8081 \
go run ./cmd/gateway
```

Expected startup message:

```text
Server listening on 127.0.0.1:8080 (gateway)
```

Keep this terminal open to watch request logs. Request logs remain JSON.

## 3. Check gateway health and model listing in terminal three

Create a key and export `API_KEY` in this terminal using the
[authentication setup](#api-key-authentication) before model or chat requests.

```sh
curl -i http://127.0.0.1:8080/v1/healthz
curl -i http://127.0.0.1:8080/v1/readyz
curl -sS http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
```

Health and readiness should return HTTP 200. Health checks the gateway process;
readiness checks the backend's `/health` endpoint.

Readiness requires the backend's [llama.cpp-compatible health endpoint](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md#api-endpoints)
to return `200`. Backend base paths are preserved, so a base URL ending in
`/api` probes `/api/health`. Loading responses, redirects, connection failures,
and timeouts return a fixed JSON `503`. Backend response bodies and headers
stay private; the probe closes the body after receiving headers without waiting
for its contents. Client disconnects cancel the probe. Readiness does not gate
chat traffic or reserve backend capacity.

Without `GATEWAY_UPSTREAM_URL`, the gateway still starts if Postgres and Redis
are reachable. Liveness returns `200`; readiness and authenticated model and
chat requests return `503`. Database configuration remains required and
migrations run separately. See [configuration](#configuration) for timeout and
connection settings, and [cache outage checks](#authentication-cache-and-database-outages)
for behavior when Postgres becomes unavailable after startup.

The model-list response should contain this ID under `data`:

```text
glm-edge-4b-chat
```

Use the ID actually returned if it differs from the examples below.

## 4. Test streaming chat

```sh
curl -N --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
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
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL_ID\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain why a gateway needs timeouts in three short sentences.\"}],\"stream\":true,\"max_tokens\":128}"
```

Both versions send the same request.

## 5. Test non-streaming chat

```sh
curl --fail-with-body \
  http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
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
  -H "Authorization: Bearer $API_KEY" \
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
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
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

Export an active `API_KEY` in this terminal first. The script sends the key only
to the gateway and omits it from the JSON report. The default model is
`glm-edge-4b-chat`. This sends one warm-up per path, then
100 sequential measured requests, split evenly between direct llama.cpp and the
gateway. See [benchmarks](benchmarks.md) for timing definitions and historical
results. Save GLM measurements separately from the retired model's data.

For a short backend-only CPU/GPU comparison, the gateway and `API_KEY` are not
needed. Start the backend with the CPU flags from step 1, then run:

```sh
python bench/single_stream.py --direct-only --requests-per-path 5 \
  > /tmp/llmgate-glm-cpu-5.json
```

Stop that backend. Start it with the normal CUDA command from step 1 and run:

```sh
python bench/single_stream.py --direct-only --requests-per-path 5 \
  > /tmp/llmgate-glm-gpu-5.json
```

Keep the same model, release, context, slot count, and thread count in both
runs. Each command excludes one warm-up and measures five streams. Record
the server flags and GPU memory alongside the reports. Five requests can
establish local feasibility; use larger samples for latency claims.

## Postgres key storage

The gateway authenticates model and chat requests through Postgres. The store
creates, looks up, and revokes hashed API keys. The development Compose file currently starts Postgres and its
migration tool.

From the repository root, initialize a new local database and apply migrations:

```sh
export POSTGRES_PASSWORD="$(openssl rand -hex 32)"

sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml up -d --wait postgres

sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml run --rm migrate
```

Keep the same password for later runs. The named volume preserves data; changing
the environment variable does not rotate an existing database's password.
The local database and development role are both `llmgate`, on localhost port
5432. To use another host port, set `POSTGRES_PORT`, preserve it through sudo,
and change the test URL below. Migrations run separately from gateway startup;
repeating the migration command after it succeeds reports no change.

Run the storage integration tests with the same password:

```sh
export PGPASSWORD="$POSTGRES_PASSWORD"

LLMGATE_TEST_DATABASE_URL='postgres://llmgate@127.0.0.1:5432/llmgate?sslmode=disable' \
  go test -count=1 -v ./internal/store
```

Each database test creates a unique schema, applies the real migration, and
drops only that schema during cleanup. The test role needs schema-creation
permission. Tests use bounded connection pools and cover hash-only persistence,
lookup through a new pool, revocation, cancellation, storage failures,
concurrency, database constraints, and migration rollback. Without the test URL,
database tests skip; validation tests still run. A supplied but unreachable URL
fails the tests. The URL above disables TLS only for the local development setup.

## Redis request limits

After the Postgres setup has established `POSTGRES_PASSWORD`, start the local
Redis service. This starts only Redis and leaves the existing Postgres data alone:

```sh
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml up -d --wait redis
export GATEWAY_REDIS_URL='redis://127.0.0.1:6379/0'
```

The gateway uses that URL by default. `REDIS_PORT` changes the Compose host port;
preserve it through sudo and update `GATEWAY_REDIS_URL` if you change it. The
service binds localhost, stores its AOF in a named volume, and uses a 128 MiB data
memory cap with `noeviction`. Startup requires Redis; the key-creation command
only requires Postgres.

`GATEWAY_REDIS_MAX_CONNS` defaults to 10, `GATEWAY_REDIS_CONNECT_TIMEOUT` to 2s,
and `GATEWAY_RATE_LIMIT_TIMEOUT` to 500ms. Positive overrides are required.
Connection URLs support `redis://` and `rediss://`, optional credentials and a
database index, with no query or fragment. Use a trusted private Redis service
and TLS for deployments beyond this local development setup.

One per-key bucket covers models, HEAD models, and both chat modes. A key with
`requests_per_min=2` starts with two request credits and refills one credit every
30 seconds, up to two. This is a continuous token bucket, so its initial burst
and sustained rate differ from a strict count in each rolling minute. Admission
spends a credit before chat validation; later failures do not refund it. Daily
model-token quotas remain planned.

Run the actual Lua and client tests against Redis:

```sh
LLMGATE_TEST_REDIS_URL="$GATEWAY_REDIS_URL" \
  go test -race -count=1 -v ./internal/ratelimit
```

These tests use random positive key IDs and remove only their own buckets. They
never flush the database. They verify shared admission across two clients,
concurrent spending, different limits under the same HTTP load, refill, expiry,
limit changes, numeric precision, and failure/recovery. Without a test URL they
skip Redis integration cases; handler, configuration, cancellation, and stalled
connection tests still run. A supplied but unreachable URL fails integration tests.

For a manual two-key check, keep the gateway and backend running. Create fresh
keys so earlier requests have not spent their credits:

```sh
LOW_JSON="$(go run ./cmd/gateway key-create -name low-rate -requests-per-min 2 -tokens-per-day 100000)"
HIGH_JSON="$(go run ./cmd/gateway key-create -name high-rate -requests-per-min 4 -tokens-per-day 100000)"
LOW_KEY="$(printf '%s' "$LOW_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')"
HIGH_KEY="$(printf '%s' "$HIGH_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')"
LOW_ID="$(printf '%s' "$LOW_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
HIGH_ID="$(printf '%s' "$HIGH_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
unset LOW_JSON HIGH_JSON

for token in "$LOW_KEY" "$HIGH_KEY"; do
  for attempt in 1 2 3; do
    curl -sS -o /dev/null -w '%{http_code}\n' \
      http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $token"
  done
done
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $LOW_KEY"
```

With no other traffic on these keys and all six requests within 30 seconds, expect
`200, 200, 429` for the first key and `200, 200, 200` for the second. The final
curl shows JSON `rate_limit_exceeded` and a positive `Retry-After` header in whole
seconds. Waiting that long makes a credit available, unless another caller spends
it first. Keep `LOW_ID` and `HIGH_ID` for admin revocation when finished.

For a Redis outage check, use the cached `HIGH_KEY` and stop only development Redis:

```sh
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml stop redis
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $HIGH_KEY"
curl -i http://127.0.0.1:8080/v1/healthz
curl -i http://127.0.0.1:8080/v1/readyz
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml up -d --wait redis
unset LOW_KEY HIGH_KEY
```

The authenticated request returns `503` with `rate_limit_unavailable`; the probes
retain their process/backend meaning. Already-admitted streams continue because
Redis is consulted only at admission. Leave the gateway running; new requests
recover after Redis connectivity is restored. Do not delete the volume to simulate a
normal outage, since lost state grants fresh request bursts. An unclean failure
can still lose recent AOF writes; these request buckets are not billing records.

## API-key authentication

After Postgres setup, export the connection configuration in both the gateway
terminal and the key-creation terminal. Keep the password out of the URL. The
local example disables TLS only for the localhost development database.

```sh
export GATEWAY_DATABASE_URL='postgres://llmgate@127.0.0.1:5432/llmgate?sslmode=disable'
export PGPASSWORD="$POSTGRES_PASSWORD"
```

In terminal three, create a key and capture its JSON output without displaying
the secret in command history. The name and positive limits are stored with the
key. Request admission enforces `requests_per_min`; daily token quota enforcement
remains planned.

```sh
KEY_JSON="$(go run ./cmd/gateway key-create \
  -name manual-test -requests-per-min 60 -tokens-per-day 100000)"
API_KEY="$(printf '%s' "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')"
KEY_ID="$(printf '%s' "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
export API_KEY
unset KEY_JSON
```

Proceed only if creation succeeds. The command returns the raw secret only after
insertion. It cannot retrieve that secret later. If output delivery fails, the
record may exist without a usable secret; revoke that record and create another
key. The command reports this failure without printing the secret.

Start the gateway using step 2. Test missing and active credentials:

```sh
curl -i http://127.0.0.1:8080/v1/models
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
```

The first returns JSON `401` and `WWW-Authenticate: Bearer`. The second forwards
to the backend. Missing, malformed, unknown, and revoked credentials receive the
same error. Logs omit the secret and database diagnostics. Health and readiness
need no key. On a cache miss, an authentication lookup failure or deadline returns
`503` before backend work. Unexpired cached identities bypass the database.
Missing database configuration or failed initial connectivity prevents startup.

To check database-only revocation, update only the key created above using its ID:

```sh
psql "$GATEWAY_DATABASE_URL" -v ON_ERROR_STOP=1 -v key_id="$KEY_ID" <<'SQL'
UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now())
WHERE id = :'key_id'::bigint;
SQL
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
sleep 31 # Default cache TTL is 30 seconds.
curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
unset API_KEY KEY_ID
```

The request can still succeed until its cached identity expires. Wait for
`GATEWAY_AUTH_CACHE_TTL`, which defaults to 30 seconds, then repeat the curl to
verify `401`. Direct SQL does not invalidate the cache or notify the active-request
registry, so already-authenticated requests may finish. For active cancellation,
use the [admin revocation endpoint](#admin-revocation-and-active-requests).
Create a fresh key before continuing chat checks or the benchmark.

Run the isolated command-to-authentication integration test with:

```sh
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -count=1 -v ./cmd/gateway -run TestKeyCreationAndAuthentication
```

It creates and drops only a unique test schema, leaving development keys intact.
Without the test URL it skips; with an unreachable database it fails.

## Operator-local admin key creation

Creation and [revocation](#admin-revocation-and-active-requests) use the same
private listener. Stop the gateway before changing its configuration. With the
same Postgres setup and environment as above, generate an operator token during
first-time setup. Regenerating this file and restarting
rotates the credential. Keep the file outside the repository.

```sh
install -d -m 700 "$HOME/.cache/llmgate/admin"
(umask 077; openssl rand -hex 32 > "$HOME/.cache/llmgate/admin/token")
export GATEWAY_ADMIN_TOKEN="$(cat "$HOME/.cache/llmgate/admin/token")"
export GATEWAY_ADMIN_ENABLED=true
export GATEWAY_ADMIN_ADDR=127.0.0.1:8082

GATEWAY_ADDR=127.0.0.1:8080 \
GATEWAY_UPSTREAM_URL=http://127.0.0.1:8081 \
go run ./cmd/gateway
```

Startup prints a listener line for `gateway` and one for `admin`. Enabled admin
HTTP requires a token of 32 to 256 URL-safe ASCII characters and a literal
loopback address with port 1 through 65535. Wildcards and hostnames are rejected.
Admin HTTP remains disabled unless explicitly enabled; setting a token alone
does not enable it. Defaults are a 4096-byte body limit and a 2-second database
insertion timeout, controlled by `GATEWAY_ADMIN_MAX_BODY_BYTES` and
`GATEWAY_ADMIN_CREATE_TIMEOUT`.

In another local terminal, load the token and capture the newly issued key:

```sh
export GATEWAY_ADMIN_TOKEN="$(cat "$HOME/.cache/llmgate/admin/token")"
KEY_JSON="$(curl --fail-with-body -sS http://127.0.0.1:8082/v1/admin/keys \
  -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"http-client","requests_per_min":60,"tokens_per_day":100000}')"
API_KEY="$(printf '%s' "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')"
KEY_ID="$(printf '%s' "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
export API_KEY
unset KEY_JSON

curl -i http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
```

Creation returns `201` with `Cache-Control: no-store`. The issued key authorizes
normal model and chat requests. Redis enforces the stored request rate. Daily
token quotas are not enforced yet. If creation fails, inspect its safe error response before running
the extraction commands. A lost response after insertion can leave a stored key
whose secret cannot be recovered. Do not automatically retry issuance; inspect
the created key ID in request logs or database records, and revoke an unused
key by ID using the admin revocation endpoint below.

Check the two access boundaries:

```sh
curl -i -X POST http://127.0.0.1:8080/v1/admin/keys \
  -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN"
curl -i -X POST http://127.0.0.1:8082/v1/admin/keys
curl -i -X POST http://127.0.0.1:8082/v1/admin/keys \
  -H "Authorization: Bearer $API_KEY"
```

The public port returns `404`. Missing admin credentials and ordinary inference
keys on the private port return `401`, before body parsing or insertion. Invalid
input returns `400`, an unsupported content type `415`, an oversized body `413`,
and a database failure or insertion deadline `503`. Request IDs remain present
on rejected requests and logs omit credentials, issued secrets, and bodies.

For a remote operator, SSH can forward a local port to the gateway host's
loopback listener:

```sh
ssh -N -L 18082:127.0.0.1:8082 operator@gateway-host
```

Supply the admin token securely on the operator machine and use
`http://127.0.0.1:18082/v1/admin/keys`. This assumes a host-run gateway. Container
loopback is not host loopback; Compose admin reachability is not implemented.
Do not publicly publish or proxy this listener. Local processes can reach it
and still require the token; this boundary does not protect a compromised host.

On shutdown both listeners stop accepting and drain under one deadline. If
either enabled port cannot bind, startup releases the other and fails. If a
serving listener fails later, its peer shuts down too. The shared database pool
closes after serving and shutdown goroutines finish.

Run admin handler, lifecycle, and optional Postgres checks with:

```sh
go test ./internal/admin ./cmd/gateway
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -count=1 -v ./cmd/gateway -run TestAdminCreationAndAuthentication
```

The integration test uses a unique temporary schema and the existing migration.
Without its database URL it skips. With a supplied but unreachable URL it fails.

## Authentication cache and database outages

Defaults are `GATEWAY_AUTH_CACHE_TTL=30s` and
`GATEWAY_AUTH_CACHE_CAPACITY=1024`. Both must be positive. Hits retain the
original expiry, and a full cache evicts the oldest inserted identity.

Run deterministic expiry, eviction, outage, and revocation-race tests:

```sh
go test -race -count=1 ./internal/auth ./cmd/gateway
```

The auth tests use a controlled key store and virtual time. To verify the real
Postgres path, use the configured development database:

```sh
LLMGATE_TEST_DATABASE_URL='postgres://llmgate@127.0.0.1:5432/llmgate?sslmode=disable' \
  go test -race -count=1 -v ./cmd/gateway \
  -run 'TestKeyCreationAndAuthentication|TestAuthenticationCacheDatabaseOutage|TestAdminRevocationAndStreams'
```

For a manual outage check, first create two active keys and start the gateway
with the real backend and a longer `GATEWAY_AUTH_CACHE_TTL=60s`. Use the first
key for `/v1/models` to warm its cache entry; leave the second unused. Stop only
the development Postgres service:

```sh
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml stop postgres
```

Before the first entry expires, its key should still reach the backend. The
unused key should return `503`. After 60 seconds from the warm request, the first
key should also return `503`, even if you kept using it. Restart Postgres:

```sh
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml up -d --wait postgres
```

Both keys should work again. Admin creation and revocation require database
writes throughout the outage. Cache expiry affects new authentication only and
does not end an already-admitted stream. The admin revocation check below must
still cancel active streams immediately after revocation is published locally,
even when the key was cached.

## Admin revocation and active requests

This check consumes the test key issued in
[operator-local admin key creation](#operator-local-admin-key-creation). Keep the
admin listener enabled and the CUDA backend running. In terminal three, show
its ID and start a long stream with that key:

```sh
printf '%s\n' "$KEY_ID"

curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-edge-4b-chat","messages":[{"role":"user","content":"Count from 1 to 1000, writing each number separately. Do not skip numbers."}],"stream":true,"max_tokens":1024}'
```

After content starts arriving, open terminal four and enter that test key ID.
The token file is the same operator credential used for creation:

```sh
export GATEWAY_ADMIN_TOKEN="$(cat "$HOME/.cache/llmgate/admin/token")"
read -r 'KEY_ID?Test key ID shown in terminal three: '

curl -i -X DELETE "http://127.0.0.1:8082/v1/admin/keys/$KEY_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN"
```

Expect `204` with no body and `Cache-Control: no-store`. Terminal three's stream
should end without a normal `[DONE]`. Curl may report an incomplete transfer;
the response had already started with `200`, which cannot be replaced by `401`.
The backend should log cancellation and release the task. The gateway logs the
revoked key ID without its secret. A `204` means persistence succeeded and
cancellation was signaled, not that GPU cleanup has already finished.

Repeat the DELETE command. It should still return `204`, preserving the original
revocation timestamp. A new protected request from terminal three now fails:

```sh
curl -i http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $API_KEY"
```

Expect `401`. Issue another test key through the creation command to continue
chat checks. To verify isolation, run streams with two different keys before
revoking one; only the revoked key's stream should end. A request with the
other key must still succeed afterward.

Also check the route boundary in terminal four:

```sh
curl -i -X DELETE "http://127.0.0.1:8080/v1/admin/keys/$KEY_ID" \
  -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN"
curl -i -X DELETE "http://127.0.0.1:8082/v1/admin/keys/$KEY_ID"
```

Expect `404` on the public port and `401` without the operator token on the
private port. With valid operator credentials, malformed or nonpositive IDs
return `400`, and unknown positive IDs return `404`. IDs use canonical decimal
notation, so signs and leading zeros are rejected. A storage failure or deadline
returns a fixed `503`. If storage reports failure, active requests are not
signaled. A timeout can make the commit result uncertain; retrying DELETE is safe.
The storage deadline defaults to `2s` through `GATEWAY_ADMIN_REVOKE_TIMEOUT`.

Run deterministic admission-race, cleanup, HTTP, and optional real-Postgres
checks with:

```sh
go test -race ./internal/auth ./internal/admin
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -race -count=1 -v ./cmd/gateway -run TestAdminRevocationAndStreams
```

The integration test creates a unique schema and two keys, opens two real HTTP
streams to a controlled backend, and revokes one through the private route.
It verifies upstream cancellation, the other stream's survival, future `401`,
repeated revocation, and the persisted timestamp. Cleanup removes its schema.
Without a test database URL it skips. This covers one process; another gateway
or a direct database update cannot signal this process's requests.

## Persistent inference usage

Apply the existing migration command again before running the updated gateway.
Migration `000002_usage` adds the usage table and its key/time index:

```sh
sudo --preserve-env=POSTGRES_PASSWORD docker compose \
  -f deploy/docker-compose.yml run --rm migrate
```

This needs the Postgres setup and credentials from the earlier sections. Start
the gateway with its normal Postgres, Redis, and backend configuration. Request
streaming usage explicitly from the backend and capture only response headers:

```sh
USAGE_HEADERS="$(mktemp)"
curl -sS -N -D "$USAGE_HEADERS" -o /dev/null \
  http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-edge-4b-chat","messages":[{"role":"user","content":"Hello"}],"stream":true,"stream_options":{"include_usage":true}}'
USAGE_REQUEST_ID="$(python3 -c 'import pathlib,sys; print(next(line.split(":",1)[1].strip() for line in pathlib.Path(sys.argv[1]).read_text().splitlines() if line.lower().startswith("x-request-id:")))' "$USAGE_HEADERS")"

psql "$GATEWAY_DATABASE_URL" -v ON_ERROR_STOP=1 -v request_id="$USAGE_REQUEST_ID" <<'SQL'
\pset null 'unknown'
SELECT request_id, key_id, backend, model, streaming, prompt_tokens,
       completion_tokens, observed_sse_chunks, ttft_ms, total_ms, http_status, status
FROM usage WHERE request_id = :'request_id';
SQL
rm -f "$USAGE_HEADERS"
unset USAGE_HEADERS USAGE_REQUEST_ID
```

Expect one row, the same request ID, the key's ID, backend `primary`, and model
`glm-edge-4b-chat`. A normally completed stream has HTTP 200 and status
`completed`. Backend token counts appear only if the backend emits its usage
object; otherwise both are `unknown`. A JSON response can also supply counts,
while its TTFT is unknown. Observed SSE chunk counts are separate from tokens.
For timing and outcome definitions, see
[architecture](architecture.md#usage-records-and-missing-information).

Repeat the [client-disconnect check](#6-test-client-disconnect-cancellation),
then inspect recent rows for the same key. Expect `canceled`, potentially with
HTTP 200, and unknown counts if the final usage did not arrive. To check a
mid-stream backend failure or a nonresponding backend, use controlled tests
below rather than stopping a backend serving unrelated work.

```sh
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -race -count=1 -v ./cmd/gateway -run '^TestInferenceUsageRecords$'
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -race -count=1 -v ./internal/store -run '^TestUsage'
go test -race -count=1 ./internal/usage ./internal/gateway
```

The integration tests use real Postgres in temporary schemas and a controlled
HTTP backend. They cover JSON/SSE completion, upstream HTTP errors, header
timeouts, mid-stream connection loss, client cancellation, and admin revocation.
They do not exercise a real GPU backend. The focused unit tests cover fragmented
SSE framing, observation bounds, partial writes, concurrent requests, and bounded
usage-write failures. Database tests skip without the test URL.

A final usage write happens after proxy copying and can extend handler or response
completion by up to `GATEWAY_USAGE_WRITE_TIMEOUT`. If Postgres is unavailable,
expect a request-correlated `usage record write failed` error log and potentially
no row. The inference response is not replaced. No retry or durable recovery is
implemented; crash or forced shutdown can also lose a final record. Quotas are
not enforced by this slice.

## Configuration

| Environment variable | Default | Controls |
| --- | --- | --- |
| `GATEWAY_DATABASE_URL` | Required | PostgreSQL connection configuration |
| `GATEWAY_DATABASE_MAX_CONNS` | `10` | Maximum shared database connections |
| `GATEWAY_DATABASE_CONNECT_TIMEOUT` | `5s` | Pool startup and individual connection deadlines |
| `GATEWAY_AUTH_LOOKUP_TIMEOUT` | `2s` | Authentication lookup and local key insertion deadline |
| `GATEWAY_AUTH_CACHE_TTL` | `30s` | Fixed successful-identity lifetime, must be positive |
| `GATEWAY_AUTH_CACHE_CAPACITY` | `1024` | Maximum cached identities, must be a positive integer |
| `GATEWAY_REDIS_URL` | `redis://127.0.0.1:6379/0` | Redis or TLS Redis URL; optional credentials and database index, no query or fragment |
| `GATEWAY_REDIS_MAX_CONNS` | `10` | Maximum shared Redis connections |
| `GATEWAY_REDIS_CONNECT_TIMEOUT` | `2s` | Redis startup and connection deadline |
| `GATEWAY_RATE_LIMIT_TIMEOUT` | `500ms` | Redis decision, pool wait, and command deadline |
| `GATEWAY_USAGE_WRITE_TIMEOUT` | `2s` | Final usage insert deadline, independent of client cancellation |
| `GATEWAY_USAGE_MAX_BYTES` | `65536` | JSON response capture and each SSE line/event observation bound |
| `GATEWAY_ADMIN_ENABLED` | `false` | Enable operator-local admin HTTP |
| `GATEWAY_ADMIN_ADDR` | `127.0.0.1:8082` | Literal loopback IP and numeric port |
| `GATEWAY_ADMIN_TOKEN` | Unset | Required when enabled; 32 to 256 URL-safe ASCII characters |
| `GATEWAY_ADMIN_MAX_BODY_BYTES` | `4096` | Admin creation JSON body limit |
| `GATEWAY_ADMIN_CREATE_TIMEOUT` | `2s` | Admin database insertion deadline |
| `GATEWAY_ADMIN_REVOKE_TIMEOUT` | `2s` | Admin database revocation deadline |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GATEWAY_UPSTREAM_URL` | Unset | Backend base URL |
| `GATEWAY_UPSTREAM_TIMEOUT` | `30s` | Total deadline for model lists and JSON chat |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | `5s` | TCP connect and TLS handshake deadlines |
| `GATEWAY_UPSTREAM_HEADER_TIMEOUT` | `30s` | Backend response-header wait |
| `GATEWAY_STREAM_IDLE_TIMEOUT` | `30s` | Each upstream stream-body read wait |
| `GATEWAY_READINESS_TIMEOUT` | `2s` | Backend readiness-probe deadline |
| `GATEWAY_MAX_BODY_BYTES` | `1048576` | Chat request body limit |
| `GATEWAY_MAX_TOKENS` | `1024` | Output-token limit and omitted-value default |
| `GATEWAY_READ_TIMEOUT` | `10s` | Incoming headers and body read deadline |
| `GATEWAY_READ_HEADER_TIMEOUT` | `5s` | Incoming header read deadline |
| `GATEWAY_IDLE_TIMEOUT` | `60s` | Wait for the next request on an idle client connection |
| `GATEWAY_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown deadline |

Oversized chat bodies return `413`, invalid JSON returns `400`, and unsupported
content types return `415`. Duration settings accept Go values such as `500ms`
or `30s` and must be positive.

## Automated tests

For the full Go test suite:

```sh
go test ./...
```

For race detection and static checks:

```sh
go test -race ./...
go vet ./...
```

For the authentication, request-limit, and proxy packages and whitespace checks:

```sh
go test ./internal/admin ./internal/auth ./internal/ratelimit ./internal/usage ./internal/gateway ./internal/proxy ./cmd/gateway
GOTOOLCHAIN=go1.27.0 wsl ./internal/admin ./internal/auth ./internal/ratelimit ./internal/usage ./internal/store ./internal/gateway ./internal/proxy ./cmd/gateway
```

These commands use test backends. Postgres and Redis integration tests require
`LLMGATE_TEST_DATABASE_URL` and `LLMGATE_TEST_REDIS_URL` respectively and otherwise
skip. The installed whitespace checker needs Go 1.27.0 to read export data on
this machine. These checks do not replace the real llama.cpp checks above.
