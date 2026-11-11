# Switchyard

Switchyard is a cost-aware, OpenAI-compatible LLM inference gateway written in Go. Applications point their OpenAI SDK at it, and it routes each request to the cheapest deployment that is good enough, across OpenAI, Anthropic, Gemini and self-hosted open-source models. It also handles fallback, caching, budgets and cost reporting.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Delivery plan and acceptance criteria: [docs/PLAN.md](docs/PLAN.md)

## Status

**Phase 1: transparent OpenAI-compatible proxy.** Switchyard serves `POST /v1/chat/completions` (JSON and streaming) and `GET /v1/models` behind gateway API keys, and forwards each request losslessly to the first deployment of the requested model on an OpenAI-wire provider. Routing, fallback, caching, budgets and the other provider types come in later phases ([docs/PLAN.md](docs/PLAN.md)).

## Quickstart

Prerequisites: Go 1.26 or newer, and Docker (only for the image and the dev dependencies).

```sh
make build        # writes bin/switchyard
make run          # bin/switchyard serve --config configs/quickstart.yaml
```

The quickstart config needs no environment variables. It keeps state in memory, listens on `:8080` for the API and on `127.0.0.1:9090` for admin traffic. In another terminal:

```sh
curl -i localhost:8080/healthz          # 200 while the process is alive
curl -i localhost:8080/readyz           # 200 with the loaded config version
curl -s 127.0.0.1:9090/metrics | head   # Prometheus metrics, admin listener only
```

Validate a config without starting the server:

```sh
bin/switchyard config validate -c configs/quickstart.yaml
```

The reference config, [configs/switchyard.example.yaml](configs/switchyard.example.yaml), uses Redis and Postgres and reads its secrets from the environment. Run without them, validation lists every missing variable with its config path and exits non-zero. With placeholder values it passes, because storage settings are validated but nothing connects to Redis or Postgres yet:

```sh
bin/switchyard config validate -c configs/switchyard.example.yaml   # reports the missing variables

DATABASE_URL=postgres://localhost:5433/switchyard \
OPENAI_API_KEY=placeholder ANTHROPIC_API_KEY=placeholder \
GEMINI_API_KEY=placeholder TOGETHER_API_KEY=placeholder \
  bin/switchyard config validate -c configs/switchyard.example.yaml
```

`bin/switchyard version` prints the build metadata.

## Configuration basics

**Which file.** `serve` and `config validate` read the file given by `-c`/`--config`, else the one named by `SWITCHYARD_CONFIG`, else `./switchyard.yaml`.

**Precedence.** Settings resolve from built-in defaults, then the YAML file and its includes, then a few operational environment overrides (`SWITCHYARD_LISTEN`, `SWITCHYARD_ADMIN_LISTEN`, `SWITCHYARD_LOG_LEVEL`), then CLI flags such as `serve --log-level`. Later sources win.

**References.** Secrets are never written literally into config. Any value can reference the environment or a file:

| Form | Meaning |
|---|---|
| `${VAR}` | The environment variable `VAR`. It is an error if it is unset. |
| `${VAR:-default}` | `VAR`, or `default` when it is unset. `${VAR:-}` means empty when unset. |
| `${file:/run/secrets/name}` | The contents of a file, for Docker and Kubernetes secrets. |

**Includes.** `include:` lists glob patterns, relative to the root file's directory. Included files may only define list sections (`providers`, `models`); their entries are appended in sorted file-name order, and a duplicate ID is an error. The example config includes the model catalog from [configs/catalog/](configs/catalog/).

**Limits.** `server.max_header_bytes` (default 64 KiB) caps request headers; net/http allows up to 4 KiB beyond it before it answers 431. A `${file:…}` reference reads only a regular file of at most 64 KiB.

**Anchors and merge keys.** YAML anchors, aliases and merge keys (`<<: *base`) work within one file, except that `include:` and the top-level keys of an included file must be written out. Keys written next to a merge key override the merged ones, and in a merge sequence (`<<: [*a, *b]`) the earlier mapping wins. Nested references such as `${A:-${B}}` are not supported and are reported as errors.

**Strict validation.** Unknown keys are rejected. Validation does not stop at the first problem: it reports every error at once, each with its file, line and config path, such as `providers[2].base_url`. Secret values never appear in errors, logs or CLI output: messages name a referenced value by its reference (`the value of ${VAR}`), and describe a literal value or key that looks like a credential only as `a string value` or `<hidden key>`. A credential written literally, including as the default of `${VAR:-default}`, gets a warning.

Every supported key is documented, with its default, in [configs/switchyard.example.yaml](configs/switchyard.example.yaml).

## API keys

Clients authenticate to Switchyard with a gateway API key, sent the same way as an OpenAI key: `Authorization: Bearer sy_…`. Keys belong to tenants, which are defined in the config. The config stores only each key's SHA-256 hash, never the key itself.

**Create a key.** `keys generate` prints a new key once, followed by the config entry that holds its hash:

```sh
bin/switchyard keys generate --tenant acme --key-id acme-backend
```

```
New API key for tenant acme, key id acme-backend:

    sy_…

Store it now; it cannot be shown again. Switchyard keeps only its hash.
...
tenants:
  - id: acme
    keys:
      - id: acme-backend
        hash: sha256:…
```

Copy the key into your secret store right away; nothing can recover it later. Without flags, the tenant is `default` and the key id is `key-` followed by the first 6 hex characters of the hash.

**Add it to the config.** Paste the `tenants` entry into your config. If the tenant already exists, add only the key under its `keys`. Then validate the config and restart the gateway:

```sh
bin/switchyard config validate -c switchyard.yaml
```

**Call the API with it.**

```sh
export SWITCHYARD_API_KEY=sy_…   # the key printed by keys generate

curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-mini","messages":[{"role":"user","content":"Hello"}]}'
```

With an OpenAI SDK, use the key as the API key and point the base URL at `http://localhost:8080/v1`.

A missing, malformed or unknown key gets `401` with `WWW-Authenticate: Bearer` and OpenAI's error envelope, so SDKs report it as an authentication error:

```json
{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}
```

**Rules.**

- Tenant ids and key ids are 1 to 63 characters from `a-z`, `0-9`, `_` and `-`, starting with a letter or digit. Key ids must be unique across all tenants, and so must hashes.
- A hash is `sha256:` followed by 64 lowercase hex characters. Validation rejects anything else without echoing it, in case a key was pasted into the wrong field.
- Key `tags` follow the provider tag rules. Policies will match them in a later phase.
- A config with no tenants is valid, but validation warns that every `/v1` request will be rejected with 401. The quickstart config starts this way. A tenant without keys also gets a warning.
- Logs carry the `tenant` and `key_id` of each authenticated request, never the key. Failed attempts are logged at debug level, also without the key.
- To revoke a key, delete its entry and restart the gateway.

## Using the gateway

Switchyard speaks the OpenAI Chat Completions API, so an OpenAI SDK only needs a different base URL and API key.

**1. Generate a key.** Run `bin/switchyard keys generate` (optionally with `--tenant ID --key-id ID`). It prints the key once and the `tenants` entry that holds its hash; see [API keys](#api-keys) for details.

**2. Add the tenant and a model.** Paste the printed entry into your config next to a provider and a model. In Phase 1 the request's `model` is looked up by catalog `id`, and the model's first deployment serves it, with `model` rewritten to `upstream_model`:

```yaml
providers:
  - id: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
models:
  - id: gpt-5-mini
    deployments:
      - provider: openai
        upstream_model: gpt-5-mini
tenants:
  - id: default
    keys:
      - id: key-4be1c0
        hash: sha256:…   # printed by keys generate
```

Validate it with `bin/switchyard config validate -c switchyard.yaml`, then start it with `bin/switchyard serve -c switchyard.yaml`. The quickstart config already has the provider and the model; set `OPENAI_API_KEY` and add a tenant.

**3. Point the SDK at it.**

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sy_...")
resp = client.chat.completions.create(
    model="gpt-5-mini",
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.choices[0].message.content)
```

Or with curl; `-N` shows streamed events as they arrive:

```sh
curl localhost:8080/v1/models -H "Authorization: Bearer $SWITCHYARD_API_KEY"

curl -N localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-mini","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

**What is forwarded.** The request body reaches the provider unchanged, unknown fields included, except that `model` becomes the deployment's `upstream_model` and streams always ask for `stream_options.include_usage`, so usage is logged for every stream. The usage-only final chunk is passed on only to clients that set `include_usage` themselves. Non-streaming responses are returned byte for byte. The client's `Authorization` header is never forwarded; the provider's `api_key` is sent instead, along with only the client headers listed in the provider's `forward_headers`. When the client disconnects, the upstream request is canceled.

**Response headers.**

| Header | Value |
|---|---|
| `X-Request-Id` | The gateway's request ID, also in every log line for the request. |
| `X-Switchyard-Model` | The catalog model that served the request. |
| `X-Switchyard-Provider` | The provider id. |
| `X-Switchyard-Upstream-Request-Id` | The provider's own `x-request-id`, for support tickets. Omitted unless it is at most 128 bytes of printable ASCII. |
| `Retry-After` | On 429, copied from the provider. |

Streams carry `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The response is committed only when the first upstream event arrives, so failures before that get a normal error status.

**Errors.** Every error uses OpenAI's envelope, `{"error":{"message","type","param","code"}}`, so SDKs raise their usual exceptions.

| Situation | Status | `code` |
|---|---|---|
| Missing, malformed or unknown gateway key | 401 | `invalid_api_key` |
| Invalid JSON, missing `model` or `messages`, `model` over 256 bytes, non-boolean `stream` | 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `invalid_type` or `empty_array`; `param` names the field |
| Body larger than `server.max_request_bytes` | 413 | `request_too_large` |
| Body not received within `server.body_read_timeout` (default 60s) | 408 | `request_timeout` |
| Unknown model | 404 | `model_not_found` |
| Provider 4xx (other than 401/403), including 429 | the provider's status | the provider's body |
| Provider 401/403: the gateway's credential is wrong | 502 | `upstream_error` (the body is never echoed) |
| Provider 5xx, connection failure | 502 | `upstream_error` |
| First-token or total timeout | 504 | `timeout` |
| Provider failure after the stream started | an SSE `data: {"error":…}` event, then the stream closes without `[DONE]` | `upstream_error` |

Each request is logged at INFO as one `request` access-log line. For a chat completion it also carries the tenant, key id, model, deployment, latency, TTFT, gateway overhead and token counts, never the prompt or the response. Metrics (`switchyard_requests_total`, which also counts requests rejected for a bad key, `switchyard_upstream_requests_total`, `switchyard_overhead_duration_seconds` and others) are on the admin listener's `/metrics`.

## Docker

```sh
make docker          # builds deploy/Dockerfile as switchyard:dev
make docker-check    # also fails unless the image is <= 40 MB and runs as UID 65532
```

The image is a static binary on `gcr.io/distroless/static-debian13:nonroot`. It runs as `65532:65532` and starts `serve` with the quickstart config baked in at `/etc/switchyard/switchyard.yaml`:

```sh
docker run --rm \
  -p 8080:8080 \
  -p 127.0.0.1:9090:9090 \
  switchyard:dev
```

Inside the container the admin listener binds `:9090` (set by `SWITCHYARD_ADMIN_LISTEN`), so Prometheus can scrape it over a private network. Never publish port 9090 on a public interface: publish it explicitly on loopback with `-p 127.0.0.1:9090:9090`, as above, or reach it over a private network. The image exposes only port 8080, so `docker run -P` publishes the API but not the admin listener. To use your own config, mount it and point `--config` at it:

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/switchyard.yaml:/etc/switchyard/custom.yaml:ro" \
  -e OPENAI_API_KEY \
  switchyard:dev serve --config /etc/switchyard/custom.yaml
```

The Dockerfile uses BuildKit features (cache mounts), so `docker build` needs the buildx plugin.

## Dev dependencies

```sh
make dev-up      # start the services and wait until all are healthy
make dev-logs    # follow their logs
make dev-down    # stop them; the named volumes are kept
```

[deploy/compose.yaml](deploy/compose.yaml) runs only the dependencies, not the gateway. The ports bind to 127.0.0.1:

| Service | Host port | Role |
|---|---|---|
| `redis-cache` | 6379 | Evictable cache: `maxmemory 256mb`, `allkeys-lru`, no persistence |
| `redis-state` | 6380 | Budgets and limits: `noeviction`, append-only file on a named volume |
| `postgres` | 5433 | Usage ledger. User, password and database are all `switchyard`, for development only. |

To delete the data as well, run `docker-compose -f deploy/compose.yaml down -v` (or `docker compose …`).

## macOS and Colima notes

- Start the Docker daemon first with `colima start`.
- The Makefile uses `docker compose` when that plugin is registered and falls back to the standalone `docker-compose` binary otherwise, so registering the plugin is optional. To register it, add the line Homebrew prints to `~/.docker/config.json`, for example `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]`.
- `make docker` needs the buildx plugin: `brew install docker-buildx`, with the same `cliPluginsExtraDirs` entry. Without it, `docker build` fails with "BuildKit is enabled but the buildx component is missing or broken". Until it is installed, the Colima VM's own Docker CLI, which includes buildx, can build against the same daemon: `make docker-check DOCKER="colima ssh -- docker"` (Colima mounts your home directory at the same path inside the VM).
- Later phases run integration tests with testcontainers, which on Colima needs:

  ```sh
  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
  export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
  ```

## Make targets

Run `make` or `make help` to list them.

| Target | What it does |
|---|---|
| `build` | Builds `bin/switchyard` with `-trimpath` and version metadata from git. |
| `test` | `go test -race -count=1 ./...` |
| `cover` | Writes `coverage.out` and prints a per-function coverage summary. |
| `bench-overhead` | Measures the latency the gateway adds against a zero-latency fake upstream; results in [test/load/RESULTS.md](test/load/RESULTS.md). Pass flags with `BENCH_FLAGS="-mode stream -concurrency 200"`; the default is the P1 acceptance run (non-stream, concurrency 100). |
| `smoke` | Live smoke test against OpenAI with the SDK. Opt-in: it skips unless `OPENAI_API_KEY` is set; `SMOKE_MODEL` defaults to `gpt-5-mini`. |
| `fmt` | Formats the code with `golangci-lint fmt` (gofmt, goimports). |
| `fmt-check` | Fails if formatting would change anything. |
| `vet` | `go vet ./...` |
| `lint` | `golangci-lint run` with [.golangci.yml](.golangci.yml). |
| `vuln` | `go tool govulncheck ./...` |
| `tidy-check` | `go mod tidy -diff`: fails if go.mod or go.sum are not tidy. |
| `check` | fmt-check, vet, lint, build, test, vuln and tidy-check. Run it before every merge; it needs no Docker. |
| `run` | Builds, then serves `configs/quickstart.yaml`. Override with `make run CONFIG=path`. |
| `docker` | Builds the image `switchyard:dev`. |
| `docker-check` | Builds the image and checks its size and user. |
| `dev-up`, `dev-down`, `dev-logs` | Manage the dev dependencies in `deploy/compose.yaml`. |
| `clean` | Removes `bin/`, `dist/` and coverage files. |

`VERSION`, `COMMIT` and `DATE` come from `git describe`, `git rev-parse` and the clock, with fallbacks for a repository without commits. Override any of them, for example `make build VERSION=v0.1.0`.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs the same checks on every push and pull request: build and race tests on the two supported Go releases, sample-config validation, golangci-lint, govulncheck, `go mod tidy -diff`, and the image size and user checks. CI never calls a real provider.

## Project layout

```
cmd/switchyard/     CLI: serve, config validate, keys generate, version
internal/
  api/openaiapi/    HTTP handlers for /v1/chat/completions and /v1/models
  apierror/         errors in the OpenAI-compatible JSON envelope
  app/              composition root: config snapshot to running service
  auth/             gateway API keys: hashing, store, middleware
  config/           schema, loading, ${…} expansion, includes, validation, snapshot
  gateway/          request pipeline, metrics, completion log fields
  providers/        adapter interface; openai/ is the OpenAI-wire adapter
  schema/           lossless Chat Completions request and response types
  server/           listeners, middleware, health checks, graceful shutdown
  sse/              server-sent events reader and writer
  telemetry/        slog with secret redaction, Prometheus registry
  reqctx/           request ID and request-scoped logger in context
  version/          build metadata
test/
  fakeupstream/     scriptable fake OpenAI-wire server
  conformance/      OpenAI SDK conformance suite, direct and through the gateway
  load/             overhead benchmark (make bench-overhead)
  smoke/            opt-in live smoke test (make smoke)
configs/            quickstart.yaml, switchyard.example.yaml, catalog/
deploy/             Dockerfile, compose.yaml (dev dependencies)
docs/               DESIGN.md, PLAN.md
```

Later phases add the packages listed in [DESIGN §19](docs/DESIGN.md#19-project-structure).

## License

TBD.
