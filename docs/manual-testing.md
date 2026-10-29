# Manual testing with GLM and llama.cpp

GLM-Edge-4B-Chat Q6_K is the primary model. Use the complete Compose definition
below, or the host-development workflow that follows it. Historical evidence is
labeled with the model used at the time.

## Compose deployment

Run from the repository root. The host needs:

- An NVIDIA GPU and working host driver (`nvidia-smi` must succeed).
- Docker Engine, Compose 2.30 or later, and Buildx for the gateway image build.
- NVIDIA Container Toolkit configured for Docker and permission to use the daemon.

Host Go and CUDA development tools are not required for this Compose deployment.
Go is needed for host development and automated tests.

On Arch Linux, install the container tools and configure NVIDIA support:

```sh
sudo pacman -S --needed docker docker-compose docker-buildx nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
sudo usermod -aG docker "$USER"
```

The runtime command updates `/etc/docker/daemon.json`. Restarting Docker can
interrupt existing containers. Docker-group membership grants root-equivalent
access. Log out and back in, then reopen terminal and agent applications so they
inherit the group. Alternatively, `newgrp docker` opens a child shell with access;
it does not update applications already running. A permission error without
`sudo`, while root can run Docker, indicates an access problem rather than a
failed NVIDIA setup.

Verify the daemon and GPU visibility:

```sh
docker info --format '{{.ServerVersion}}'
docker compose version
docker buildx version
docker run --rm --runtime=nvidia --gpus all ubuntu:24.04 nvidia-smi
```

The last command downloads Ubuntu if missing and removes the test container on
exit. For a check without downloads, add `--pull=never`, choose a compatible
image from `docker image ls`, and use `--entrypoint nvidia-smi` if it has a server
entrypoint. No large CUDA image is needed just to check GPU visibility. Expect
the host GPU and driver in the output; this does not establish model inference.
See [NVIDIA setup](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html#configuring-docker),
[Docker access](https://docs.docker.com/engine/install/linux-postinstall/), and
[Compose GPU support](https://docs.docker.com/compose/how-tos/gpu-support/).

Download and verify the GGUF using the model commands in
[host setup](#1-prepare-the-model-and-start-the-backend-in-terminal-one).
Only the GGUF download and hash check are needed for Compose; skip the host
binary setup and launch. The weight file
stays on the host and is mounted read-only; a missing path is not created.

Prepare local configuration:

```sh
cp deploy/.env.example deploy/.env
chmod 600 deploy/.env
```

Set `POSTGRES_PASSWORD` to a unique password in that file. If reusing an existing
Postgres volume, use its current password. Set `LLMGATE_MODEL_PATH` to the
absolute GGUF path, normally
`/home/<user>/.cache/llmgate/models/glm-edge-4b-chat/ggml-model-Q6_K.gguf`.
An empty path uses the current user's home cache, which may differ under sudo.
Use daemon-authorized `docker` commands, or prefix them with `sudo` if required.
Do not commit this private file. Avoid printing resolved Compose configuration,
which contains credentials; use `config --quiet` for validation.

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml config --quiet
docker compose --env-file deploy/.env -f deploy/docker-compose.yml up --build -d --wait
docker compose --env-file deploy/.env -f deploy/docker-compose.yml ps -a
```

Expect `migrate` to exit successfully and gateway, backend, Postgres, and Redis
to become healthy. Migration failure prevents gateway startup. Backend loading
has a startup allowance before health failures count. Inspect logs if startup
fails:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml logs --tail=100 gateway backend migrate
```

The gateway publishes `127.0.0.1:8080`; `GATEWAY_PORT` changes that host port.
Postgres and Redis keep their loopback development ports and named volumes.
The backend has no host port. Use its container health check or execute its
installed `curl` inside the backend container. Admin HTTP is disabled by
default. If enabled, it remains on `127.0.0.1:8082` inside the gateway container,
with no published admin port. Run operator HTTP commands inside that container.
The local key-creation command is available without enabling admin HTTP:

```sh
KEY_JSON="$(docker compose --env-file deploy/.env -f deploy/docker-compose.yml exec -T gateway \
  llmgate key-create -name compose-test -requests-per-min 60 -tokens-per-day 100000)"
export API_KEY
API_KEY="$(printf '%s' "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])')"
unset KEY_JSON
curl -fsS http://127.0.0.1:8080/v1/healthz
curl -fsS http://127.0.0.1:8080/v1/readyz
curl -fsS http://127.0.0.1:8080/v1/models -H "Authorization: Bearer $API_KEY"
```

Use the README chat example and the [metrics check](#prometheus-metrics) below.
`deploy/.env.example` lists optional overrides as commented, empty assignments.
Uncomment a setting and supply a value to override it. Compose forwards optional
gateway settings as empty when unset. The [startup configuration loaders](../cmd/gateway/config.go)
supply defaults and validate them by subsystem.
Only variables referenced by Compose reach the container. Internal database,
Redis, upstream, and listener addresses stay fixed in Compose, which also owns
Docker and backend defaults such as published ports and container stop grace.

Stop without removing persistent data:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml down
```

Gateway stop grace defaults to 35 seconds, covering its 10-second drain window,
3-second request-cleanup window, and dependency teardown. The pinned pgx driver
can spend up to 15 seconds closing failed connections when Postgres is frozen. If drain, cleanup, or
accounting deadlines increase, also increase `GATEWAY_STOP_GRACE_PERIOD`.
Cleanup runs after forced cancellation while Postgres and Redis remain open;
its budget should cover Redis settlement followed by the usage write. Health failures do not automatically
restart a running container. Container logs rotate at 10 MiB with three files
per service. Preserve Postgres and Redis volumes, and the host
model, across restarts. No volume deletion is needed for routine startup.

The gateway image build, migrations, full startup, all four service health
checks, authenticated JSON/SSE inference, and metrics scraping passed on the
RTX 3060. The pinned CUDA backend loaded the model and used the GPU; the
gateway ran as UID 10001. Keys, usage rows, request buckets, and quota state
survived clean Postgres/Redis container restarts.

[Recorded verification](../bench/results/2026-10-25-phase1-verification.json)
covers prefill cancellation, a killed GPU backend mid-stream, a paused backend,
controlled header/stream stalls, dependency outages, different per-key rates,
and three concurrent requests during both draining and forced shutdown.
The separate verification gateway used 2s header/idle deadlines, a 3s auth cache
TTL and shutdown deadline, and a 500ms usage-write deadline. The forced-shutdown
recheck used a 10s header deadline so all three requests were still active when
the 3s shutdown deadline expired. These shorten the checks; they do not change
deployment defaults.

The pre-fix verification lost all three final usage rows at forced shutdown.
The lifecycle fix adds a bounded cleanup window before dependency teardown;
the follow-up persisted all three canceled rows before shutdown returned.
[Follow-up evidence](../bench/results/2026-10-25-shutdown-accounting.json) also
covers normal draining and a frozen database. The latter logs bounded write
failures, then allows the driver's separate failed-connection cleanup to finish.
These historical checks predate durable admission. The current queue preserves
admissions across process crashes and replays saved finals after Postgres recovery.
A crash before final handoff produces a recovered interrupted row with unknown
counts, rather than reconstructing exact usage. See the [final verification and
baseline](benchmarks.md) for the complete middleware path.

Run the lifecycle race and cleanup-budget checks without external services:

```sh
go test -race -count=1 ./cmd/gateway \
  -run 'TestForcedShutdownWaitsForAccounting|TestRequestDrain|TestRequestCleanupDeadline'
```

With the test database URL and `PGPASSWORD` configured, exercise actual final
inserts during forced cancellation:

```sh
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
  go test -race -count=1 ./cmd/gateway -run '^TestForcedShutdownPersistsUsage$'
```


For host development, use three terminals for backend logs, gateway logs, and
client output. The host backend listens on localhost port 8081 and the gateway
on localhost port 8080. Do not start them alongside Compose on the same ports.

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

Health returns HTTP 200 while the process is running. Readiness initially returns
503 until the background registry observes two consecutive successful probes.
With default settings, probes start immediately and repeat every 5 seconds; each
has a 2-second deadline. Three consecutive failures mark a healthy backend unready;
two consecutive successes restore it. Opposite results reset the streak.

The backend’s `/health` endpoint must return 200. Base paths are preserved, so
`/api` probes `/api/health`. Loading responses, redirects, connection failures,
and timeouts count as failures. Probes do not follow redirects or forward caller
credentials. Bodies close after headers without waiting for content. Readiness
serves only a snapshot, so caller disconnects do not cancel shared probes.
Models and chat use the same health snapshot for selection. Until a backend is
healthy, valid authenticated chat and model-list requests return `503` with
`error.code: no_backend`. Readiness itself reserves no inference capacity.

To check transitions, stop the configured backend and poll readiness until it
returns 503. Restart it and poll until 200. Look for `backend health changed`
logs naming `primary`. Allow the configured interval, probe deadline, and result
thresholds before expecting a transition. This check has unit-test coverage;
it has not yet been verified against the deployed GPU backend for Phase 2.

Without either `GATEWAY_UPSTREAM_URL` or `GATEWAY_BACKENDS`, the gateway still starts if Postgres and Redis
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

The example above covers disconnection during decode. The recorded GPU
verification also used a long, uncached prompt, waited for the backend to launch
its task, and disconnected before the first complete SSE event. It checked the
canceled usage row with unknown counts and no TTFT, plus the matching backend
cancellation and slot-release logs. See the
[prefill evidence](../bench/results/2026-10-25-phase1-verification.json).

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
creates, looks up, and revokes hashed API keys. The same Compose file can start
only Postgres and migrations for host development.

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
spends a credit before chat validation; later failures do not refund it.
[Daily token quotas](#daily-token-quotas) apply separately to validated chat.

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
key. Request admission enforces `requests_per_min`; validated chat also enforces
`tokens_per_day`.

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
token quotas apply to validated chat. If creation fails, inspect its safe error response before running
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
loopback is not host loopback. For Compose, execute operator HTTP commands
inside the gateway container, as described in the deployment section above.
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

Before the first entry expires, its key should still reach the models backend.
Chat must return `503` once the accounting worker detects Postgres delivery
failure. Until that bounded detection completes, requests may still enter through
the durable Redis queue. The
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
Migration `000002_usage` adds the usage table; apply through
`000004_recovered_usage` for recovery provenance and the retention index:

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
       completion_tokens, observed_sse_chunks, ttft_ms, total_ms, http_status, status, recovered
FROM usage WHERE request_id = :'request_id';
SQL
rm -f "$USAGE_HEADERS"
unset USAGE_HEADERS USAGE_REQUEST_ID
```

Delivery is asynchronous; repeat the query until the next batch arrives.
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
  go test -race -count=1 -v ./internal/store -run '^Test(Usage|Admission)'
go test -race -count=1 ./internal/usage ./internal/gateway
```

The integration tests use real Postgres in temporary schemas and a controlled
HTTP backend. They cover JSON/SSE completion, upstream HTTP errors, header
timeouts, mid-stream connection loss, client cancellation, and admin revocation.
They do not exercise a real GPU backend. The focused unit tests cover fragmented
SSE framing, observation bounds, partial writes, concurrent requests, and bounded
usage-write failures. Store tests also deliver identical records concurrently,
verify only one usage row remains, and reject conflicting retries without
changing the original accounting. Database tests skip without the test URL.

Apply all migrations through `000004_recovered_usage` before running this gateway version.
Redis must use AOF `always`, `maxmemory-policy noeviction`, and persistent storage.
The gateway validates those settings at startup and needs permission for
`CONFIG GET`, Lua, and `WAITAOF` (Redis 7.2+). Compose configures these settings;
rebuild/restart the prepared deployment to apply changes. Do not change persistence
settings underneath a running gateway. Quota/rate writes on this Redis also use
synchronous AOF. The final [baseline](benchmarks.md) measures this configuration.

Each admission and final handoff waits for local Redis disk acknowledgment under
`GATEWAY_USAGE_WRITE_TIMEOUT`. Failed admission returns `503` with
`usage_unavailable` and attempts to refund the unused quota reservation. No
generation starts. Failed final handoff logs `usage record write failed`, keeps
the quota charged, and does not replace the inference response.

Background delivery uses `GATEWAY_ACCOUNTING_BATCH_SIZE` per Postgres transaction
and `GATEWAY_ACCOUNTING_FLUSH_INTERVAL` for idle polling. Full batches drain
immediately, up to eight batches per worker turn. Idle polls skip redundant
Postgres probes while shared health has more than half its lifetime left. Failed delivery logs `accounting batch delivery failed; new inference
paused` and clears the shared health marker. Health expires after twice the write
timeout plus twice the polling interval if the writer stops. This bounds failure
detection; it is not an instantaneous Postgres outage check. Active requests can
still record final outcomes. Delivery recovery reopens admission.

`GATEWAY_ACCOUNTING_MAX_PENDING` counts active and undelivered requests together.
Each reserves two fixed 8 KiB payload slots (16 MiB total at default capacity,
plus Redis/index overhead). A full queue rejects new inference. Pending records
are never trimmed. `GATEWAY_ACCOUNTING_MAX_AOF_BYTES` defaults to 256 MiB; the
gateway reserves Redis maxmemory plus four 8 KiB slot sizes per pending request
for rewrites and final outcomes. With the default 128 MiB Redis memory limit,
new protected traffic stops at 96 MiB current AOF size. The background guard
returns `503 accounting_storage_unavailable` for models and chat before request
credits are consumed, and cancels active generation so finalization can use its
reserved space. Probes and metrics stay accessible.

This is an admission threshold, not a hard filesystem quota. Allow for concurrent
writes, AOF rewrites, backups, and other processes when provisioning the volume.
A failed AOF write also closes admission; a missing disk acknowledgment never
starts inference. Automatic AOF rewriting can make the guard healthy again.
A Redis restart on the same intact volume preserves acknowledged handoffs.
Disk loss and failover to a stale replica are outside this guarantee.

The ready backlog and occupied slots can be inspected on the development stack:

```sh
docker compose -f deploy/docker-compose.yml exec -T redis \
  redis-cli HLEN llmgate:accounting:v1:records
docker compose -f deploy/docker-compose.yml exec -T redis \
  redis-cli ZCOUNT llmgate:accounting:v1:ready -inf 9007199254740991
```

Divide `HLEN` by two for occupied request slots. Delivered admissions without a
final outcome retain their slots while their owner is alive. An owner heartbeat
runs every third of `GATEWAY_ACCOUNTING_LEASE_TIMEOUT` (30s by default). After
expiry, bounded recovery creates interrupted usage with NULL tokens, HTTP zero,
and `recovered = true`; its `total_ms` is admission-to-recovery time, not observed
generation time. A late old owner cannot replace that result. Missing database
rows can still represent active or queued work.

The writer retries quota settlement before freeing final slots, against the
original UTC day. Unknown consumption keeps its conservative charge. Postgres
retention removes usage/admission pairs older than `GATEWAY_ACCOUNTING_RETENTION`
from request start (720h), sweeping every `GATEWAY_ACCOUNTING_PRUNE_INTERVAL` (1h).
Each transaction deletes at most the configured batch size, then continues while
full batches remain. Unfinished admissions and API keys are retained.

Run against isolated test Postgres schemas and a disposable Redis configured with
the settings above. Queue tests use random namespaces; quota tests use random
key IDs. Keep mutation/restart checks separate from the normal suite:

```sh
LLMGATE_TEST_DATABASE_URL="$GATEWAY_DATABASE_URL" \
LLMGATE_TEST_ACCOUNTING_REDIS_URL="$TEST_REDIS_URL" \
  go test -race -count=1 ./internal/accounting ./internal/store ./cmd/gateway
```

`TestDurableAccountingBatches` covers batch limits, cached-key rejection, a lost
Postgres commit reply, and replacement-worker replay. `TestQueueRedisRestart`
supports `LLMGATE_ACCOUNTING_RESTART_PHASE=seed` followed by `replay` around an
external Redis SIGKILL/restart on the same test volume. Run
`TestQueueRedisConfiguration` alone with `LLMGATE_ACCOUNTING_ADMIN_CHECK=1` only
on disposable Redis: it temporarily changes fsync/maxmemory to verify rejection
of unsafe settings and final-slot replacement under memory pressure.
`TestDiskFullRecovery` has separate `LLMGATE_ACCOUNTING_DISK_PHASE=seed` and
`replay` modes for an externally constrained disposable filesystem. Never fill
the host or the learning volume to exercise it. The final verification used a
4 MiB tmpfs, preserved acknowledged admissions through Redis ENOSPC and restart,
and removed the disposable filesystem afterward.

## Daily token quotas

Use the pinned b11681 GLM backend above. The gateway requires its internal
`/apply-template` and `/tokenize` endpoints for text chat. They stay behind the
gateway and are not public gateway routes. Supported requests contain text
messages and one completion. Positive generation-limit aliases are normalized
to the smallest requested limit and the available budget. Unsupported media,
multiple completions, and nonpositive aliases return `400`.

Grace defaults to zero. To test 10% grace, restart the gateway with
`GATEWAY_QUOTA_GRACE_PERCENT=10`. A key with a 1000-token budget has a shared
1100-token ceiling for admitted work. New work stops when spent plus reserved
tokens reaches 1000. A capped response returns generated text with
`finish_reason: "length"`; SSE also ends with `[DONE]`. Exhausted requests
return `429`, code `token_quota_exceeded`, and `Retry-After` until midnight UTC.
Models remain usable subject to the separate request-rate limit.

Create a dedicated small-budget key through the existing key-creation command,
then repeat an authenticated chat request with a large `max_tokens`. The final
request may be capped. Inspect the allowance without displaying key secrets:

```sh
redis-cli HGETALL "llmgate:tokens:v1:$KEY_ID"
redis-cli TTL "llmgate:tokens:v1:$KEY_ID"
```

After finalization, expect `day` and `used` fields and no `r:` pending markers.
The TTL is at most one UTC day. Usage rows keep backend token counts, never
invented counts for missing usage. Quota charging and failure semantics live
in the [decision](decisions.md#daily-input-and-output-quotas).

Run the automated quota checks with the existing test database and Redis URLs.
They create isolated database schemas and temporary per-key Redis state:

```sh
go test -race -count=1 ./internal/quota ./cmd/gateway
```

To include real prompt-count and capped JSON/SSE checks against an already
running, dedicated b11681 GLM backend:

```sh
LLMGATE_TEST_LLAMA_URL=http://127.0.0.1:8081 \
go test -race -count=1 ./internal/quota ./cmd/gateway
```

The real-backend tests skip without `LLMGATE_TEST_LLAMA_URL`; Redis and database
checks need `LLMGATE_TEST_REDIS_URL` and `LLMGATE_TEST_DATABASE_URL` as documented
above. Tested paths cover zero and 10% grace, aliases, shared concurrent budget,
idempotent refunds, missing/interrupted usage, cancellation, failed usage writes,
rollover, and counting/Redis failures. Final token settlement can extend handler
completion by its Redis timeout in addition to the existing usage-write timeout.

## Prometheus metrics

Scrape the public, versioned endpoint without a key:

```sh
curl -fsS http://127.0.0.1:8080/v1/metrics
```

Expect `gateway_requests_total`, `gateway_request_duration_seconds`,
`gateway_ttft_seconds`, `gateway_active_streams`, `gateway_queue_depth`,
`gateway_queue_wait_seconds`, and `gateway_rejected_total`. Histograms expose their
`_bucket`, `_sum`, and `_count` series. Counters include public probes, models,
chat, and rejections. Backend labels use configured names (`primary` for the
legacy single URL); `none` identifies requests without a successful selection. Scrapes and admin
requests are excluded. These metrics are in process memory and reset on restart.

By default, authenticated requests share `key="other"`. A positive
`GATEWAY_METRICS_KEY_CAPACITY` enables individual numeric key-ID labels up to
that capacity; further identities still share `other`.
Unauthenticated attempts use `key="none"`. No path, raw key, model, or request
ID is exported. Status describes observed HTTP headers, so an interrupted
stream may still have `status="200"`; no observed status uses `none`.

During a long streaming request, scrape from another terminal and expect the
active-stream gauge to rise, then return to zero after proxying ends. TTFT
histogram count increases once per stream that emits a nonterminal SSE data
event. Streams with no event and JSON responses add no TTFT sample. Request
duration includes quota settlement and usage recording; active streams do not.
Queue depth counts waiting chats, excluding occupied slots. Queue wait records
each completed capacity acquisition for a selected backend, including immediate
grants, timeouts, cancellations, and unavailable admission. Routing rejections
without a selected target add no wait sample. Rejection counters cover
`reason="queue_full"`, `reason="queue_timeout"`, and `reason="no_backend"`;
cancellations do not increment these counters. The queue metrics cover both JSON and SSE chat.
See [measurement boundaries](architecture.md#prometheus-measurement-boundaries)
for the full timing definition.

Scraping still works while Redis or Postgres is unavailable, provided the
gateway process is running. It does not probe those services. Its default
limits are two simultaneous scrapes and a two-second scrape deadline. These
checks run without external services:

```sh
go test -race ./internal/metrics ./internal/auth ./internal/usage ./internal/gateway
```

With the existing test Postgres/Redis URLs and a dedicated b11681 GLM backend,
include the authenticated GPU scrape check:

```sh
LLMGATE_TEST_LLAMA_URL=http://127.0.0.1:8081 \
go test -race -count=1 ./cmd/gateway -run TestLlamaMetrics
```

The GPU check skips without its backend URL. Prometheus and Grafana containers
are not part of this initial exporter deployment.

## Multiple backends

Configure existing, reachable llama.cpp instances with the same model alias and
quota-compatible tokenizer endpoints. Database, Redis, migrations, and API-key
setup are unchanged. The current Compose file still starts one backend; this
configuration check does not require adding GPU containers.

For a directly started gateway, replace the single URL with a JSON array:

```sh
GATEWAY_UPSTREAM_URL= \
GATEWAY_BACKENDS='[{"name":"a","url":"http://127.0.0.1:8081","slots":1,"queue_capacity":4},{"name":"b","url":"http://127.0.0.1:8083","slots":2,"queue_capacity":4}]' \
go run ./cmd/gateway
```

Names must be unique, 1 to 64 ASCII letters, digits, underscores, or hyphens.
`none` is reserved. The list is limited to 64 KiB and unknown fields are rejected.
URLs follow the single-URL validation rules. Omitted `slots` and `queue_capacity`
use the global defaults; explicit zero queue capacity disables waiting for that
backend. Slots must be positive. Set them to the server's parallel capacity.
The queue-wait timeout and health thresholds are shared settings.

Wait for readiness, then send overlapping authenticated JSON or SSE chats.
Requests select healthy targets by local assigned request count, rotating ties.
Scrape metrics and inspect usage to see the selected backend names. Stop one
server and wait for its health failure threshold: new selections should use the
remaining healthy server. Existing assignments stay on their original target.
Eligible failures can retry once before response commitment; see the
[failover checks](#bounded-chat-failover). Stop both and wait for the
threshold: readiness, model listing, and valid authenticated chat should return
`503`. Full capacity on all healthy targets returns `429` with `Retry-After: 1`.

Run deterministic configuration, routing, and two-server HTTP checks without
external storage or a GPU:

```sh
go test -race ./internal/router ./internal/gateway -run 'TestRouting|TestRoutedChatFlow'
go test ./cmd/gateway -run 'TestLoadBackends|TestBackendConfigIntegration'
```

These tests establish request behavior, not multi-GPU throughput, backend death
under load, or the Phase 2 latency target.

## Inference circuit breakers

Run the deterministic state and authenticated proxy-flow tests:

```sh
go test -race ./internal/router ./internal/gateway ./cmd/gateway -run 'TestCircuit'
```

These cover failure thresholds, streak resets, cooldown boundaries, one concurrent
trial, recovery, stale results, queued assignment cleanup, JSON/SSE failover, and
neutral client/admission failures. They use controlled backends and virtual time;
they do not establish recovery latency under GPU load.

During a controlled deployment test, keep backend A's `/health` returning 200
while its generation endpoint returns 503. After the configured number of failed
A generations, expect a `backend circuit opened` log with A's name. Further chats
should bypass A, or return 503 if no other backend is eligible. After cooldown,
one chat may try A. Restore successful generation and expect `backend circuit
closed` after that trial; another failed trial restarts cooldown. Readiness can
remain 200 throughout because it reports probe health. This deployed scenario has
not yet been run. See [circuit behavior](architecture.md#circuit-breakers).

## Bounded chat failover

`GATEWAY_MAX_ATTEMPTS` defaults to `2`; set `1` to disable chat retries. With two
healthy targets, a pre-response transport failure or backend `502`, `503`, or
`504` can select another backend. No target is tried twice. Normal responses
stream immediately. Once response headers, informational headers, a body write,
or a flush commits delivery, the gateway does not retry. JSON failures after
commitment abort delivery. Interrupted SSE emits one terminal `event: error`
when bounded delivery succeeds, with no success `[DONE]` marker. Client
cancellation stops both generation and any fallback wait.

A failed attempt can consume its full reserved allowance even when the client
saw no output. The second attempt obtains a fresh allowance and can be rejected
by the daily quota. Inspect delivered usage using the public request ID and its
`:attempt-2` suffix to distinguish the attempts. The accounting and timing
boundaries live in [architecture](architecture.md#retry-attempts-and-response-commitment).
If the alternate is unavailable, full, or times out waiting, expect the original
upstream error. Counting, quota, and durable-admission failures are not retried.
The same upstream and queue deadlines apply independently to each attempt.

Run the deterministic failure checks without external storage or GPUs:

```sh
go test -race ./internal/gateway -run 'TestChatFailover|TestChatAfterCommit|TestChatRetryUnavailable|TestChatFallbackWait|TestChatTerminalErrorDeadline'
go test ./cmd/gateway -run TestAttemptConfiguration
```

The tests use controlled transports and an HTTP gateway to exercise unknown
consumption, distinct records, commitment, and cleanup. They do not establish
backend death under GPU load or the Phase 2 latency target.

## Bounded inference admission

Each backend accepts its configured slots into preparation and inference, plus
its configured queue capacity in waiting requests. The single-URL defaults come
from `GATEWAY_BACKEND_SLOTS` and `GATEWAY_BACKEND_QUEUE_CAPACITY`.
JSON and SSE share these limits. Compose derives active slots from `LLAMA_PARALLEL`;
when running the gateway directly, set slots to the backend’s parallel capacity.
These limits apply per gateway process. They do not coordinate multiple replicas.

Run the focused checks without external services:

```sh
go test -race ./internal/queue ./internal/gateway -run 'TestQueue|TestAdmission'
go test ./cmd/gateway -run TestInferenceQueueConfig
```

Tests cover FIFO handoff, zero waiting capacity, full queues, wait expiry,
cancellation racing grants, JSON/SSE waiting, and slot release on preparation
failure, cancellation, and proxy abort. Final accounting runs after slot release.

For a deployed check, use a key whose request and daily token limits will not
reject the test traffic first. Start one long chat request per backend slot,
then enough concurrent chats to fill the waiting queue. Additional valid chats
should return JSON HTTP 429 with `error.code: queue_full` and `Retry-After: 1`.
Keep the active requests running past the queue timeout: waiting chats should
return HTTP 503 with `error.code: queue_timeout` without starting generation.
Stop a waiting client and verify a replacement can enter the queue. Finish or
cancel active chats and verify queued requests can proceed. Health, readiness,
and model listing bypass inference admission.

Scrape `/v1/metrics` while these requests run. `gateway_queue_depth{backend="primary"}`
should match the waiting clients and return to zero after they leave. Each
completed capacity acquisition on a selected backend adds one
`gateway_queue_wait_seconds_count` sample. A routing rejection when every backend
is full adds no per-backend wait sample. Queue-full and timeout responses increment the matching
`gateway_rejected_total{reason="queue_full"}` or `{reason="queue_timeout"}`
counter. Canceling a waiter clears its depth and adds its elapsed wait without
incrementing either overload counter. These checks do not establish the Phase 2
overload latency target; that requires a measured load test.

Rejected and canceled waiters create no token reservation or inference usage
record, though their request-rate credit is already consumed. A crash drops
waiting requests; only requests that reach durable admission have recovery
records. The deployed overload check has not yet been run for this slice.

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
| `GATEWAY_RATE_LIMIT_TIMEOUT` | `500ms` | Request-credit decision and pool-wait deadline |
| `GATEWAY_QUOTA_GRACE_PERCENT` | `0` | Shared daily overrun percentage, integer 0 through 100 |
| `GATEWAY_QUOTA_COUNT_TIMEOUT` | `2s` | Combined template rendering and tokenization deadline |
| `GATEWAY_QUOTA_REDIS_TIMEOUT` | `500ms` | Each token reservation or settlement deadline |
| `GATEWAY_QUOTA_COUNT_MAX_BYTES` | `8388608` | Maximum body bytes for each counting response |
| `GATEWAY_METRICS_KEY_CAPACITY` | `0` | Maximum distinct numeric key labels for the process lifetime; zero groups all keys under `other`, as does overflow |
| `GATEWAY_METRICS_MAX_SCRAPES` | `2` | Maximum simultaneous scrapes |
| `GATEWAY_METRICS_SCRAPE_TIMEOUT` | `2s` | Scrape response deadline |
| `GATEWAY_USAGE_WRITE_TIMEOUT` | `2s` | Each Redis handoff and Postgres batch deadline; final handoff detaches client cancellation |
| `GATEWAY_USAGE_MAX_BYTES` | `65536` | JSON response capture and each SSE line/event observation bound |
| `GATEWAY_ACCOUNTING_MAX_PENDING` | `1024` | Shared capacity including active requests and undelivered outcomes |
| `GATEWAY_ACCOUNTING_BATCH_SIZE` | `64` | Maximum requests per Postgres transaction; cannot exceed capacity |
| `GATEWAY_ACCOUNTING_FLUSH_INTERVAL` | `250ms` | Background idle polling interval; not an inference batching delay |
| `GATEWAY_ACCOUNTING_LEASE_TIMEOUT` | `30s` | Request-owner lifetime; greater than `3 × (write timeout + polling interval)` |
| `GATEWAY_ACCOUNTING_MAX_AOF_BYTES` | `268435456` | AOF admission budget including reserved rewrite/final headroom; not a filesystem quota |
| `GATEWAY_ACCOUNTING_RETENTION` | `720h` | Request-start age before final accounting pairs become eligible for deletion |
| `GATEWAY_ACCOUNTING_PRUNE_INTERVAL` | `1h` | Postgres retention sweep interval |
| `GATEWAY_ADMIN_ENABLED` | `false` | Enable operator-local admin HTTP |
| `GATEWAY_ADMIN_ADDR` | `127.0.0.1:8082` | Literal loopback IP and numeric port |
| `GATEWAY_ADMIN_TOKEN` | Unset | Required when enabled; 32 to 256 URL-safe ASCII characters |
| `GATEWAY_ADMIN_MAX_BODY_BYTES` | `4096` | Admin creation JSON body limit |
| `GATEWAY_ADMIN_CREATE_TIMEOUT` | `2s` | Admin database insertion deadline |
| `GATEWAY_ADMIN_REVOKE_TIMEOUT` | `2s` | Admin database revocation deadline |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `GATEWAY_UPSTREAM_URL` | Unset | Single backend base URL, named `primary`; mutually exclusive with `GATEWAY_BACKENDS` |
| `GATEWAY_BACKENDS` | Unset | JSON array of 1 to 16 named backends; see [multiple backends](#multiple-backends) |
| `GATEWAY_UPSTREAM_TIMEOUT` | `30s` | Total upstream deadline per model-list or JSON-chat attempt |
| `GATEWAY_UPSTREAM_CONNECT_TIMEOUT` | `5s` | TCP connect and TLS handshake deadlines |
| `GATEWAY_UPSTREAM_HEADER_TIMEOUT` | `30s` | Backend response-header wait |
| `GATEWAY_STREAM_IDLE_TIMEOUT` | `30s` | Each upstream stream-body read wait; also bounds terminal SSE error delivery |
| `GATEWAY_READINESS_TIMEOUT` | `2s` | Each background backend health-probe deadline |
| `GATEWAY_BACKEND_SLOTS` | `1` | Local active inference slots, including preparation; match backend `--parallel`. Compose sets this from `LLAMA_PARALLEL` |
| `GATEWAY_BACKEND_QUEUE_CAPACITY` | `4` | Maximum waiting chat requests; zero rejects immediately when slots are busy |
| `GATEWAY_BACKEND_QUEUE_TIMEOUT` | `5s` | Maximum queue wait; expires with HTTP 503 before quota or inference |
| `GATEWAY_BACKEND_HEALTH_INTERVAL` | `5s` | Probe interval per backend; probes never overlap for the same backend |
| `GATEWAY_BACKEND_HEALTH_FAILURES` | `3` | Consecutive failures before marking unhealthy; positive integer |
| `GATEWAY_BACKEND_HEALTH_SUCCESSES` | `2` | Consecutive successes before marking healthy, including startup; positive integer |
| `GATEWAY_BACKEND_CIRCUIT_FAILURES` | `3` | Consecutive eligible generation failures before opening a backend circuit; positive integer |
| `GATEWAY_BACKEND_CIRCUIT_COOLDOWN` | `10s` | Positive delay before one chat recovery trial; a failed or neutral trial restarts the delay |
| `GATEWAY_MAX_BODY_BYTES` | `1048576` | Chat request body limit |
| `GATEWAY_MAX_TOKENS` | `1024` | Output-token limit and omitted-value default |
| `GATEWAY_MAX_ATTEMPTS` | `2` | Total chat attempts, 1 or 2; a retry excludes the first backend |
| `GATEWAY_READ_TIMEOUT` | `10s` | Incoming headers and body read deadline |
| `GATEWAY_READ_HEADER_TIMEOUT` | `5s` | Incoming header read deadline |
| `GATEWAY_IDLE_TIMEOUT` | `60s` | Wait for the next request on an idle client connection |
| `GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT` | `3s` | Additional wait for complete handlers and accounting after forced cancellation |
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
