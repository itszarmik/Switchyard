# Quickstart

Two ways to run Switchyard (DESIGN §18.1):

- **[Docker Compose](#docker-compose)**: the gateway with Redis for cache and
  state and Postgres for the usage ledger, plus optional Prometheus, Grafana,
  Jaeger and a local embedding model. This is the "small production" tier.
- **[A single binary](#single-binary)**: in-memory state, nothing else to
  run. State is lost on restart; use it to try things out and in development.

Either way you need one provider key: `OPENAI_API_KEY` or
`ANTHROPIC_API_KEY`. The configuration reference is
[configuration.md](configuration.md); what to do when something breaks is
in [runbook.md](runbook.md).

## Docker Compose

You need Docker with Compose v2 and Buildx (Docker Desktop includes both)
and a checkout of this repository. On this project's reference machine
(Apple M-series, Colima), the image builds in about 20 s and the stack is
healthy about 10 s later; on a fresh machine add a few minutes to download
the base images (about 400 MB) and Go modules.

```sh
cd deploy
export OPENAI_API_KEY=sk-...          # or ANTHROPIC_API_KEY=sk-ant-...
./init.sh                             # writes .env and prints your gateway key
docker compose up -d --wait           # builds the image on the first run
```

`init.sh` writes `deploy/.env` (mode 0600, gitignored): your provider key, a
generated admin token, and the hash of a first gateway API key named
`quickstart`. It prints that key once and also keeps it in `.env` as
`SWITCHYARD_API_KEY`, for the commands below; the gateway itself only
reads the hash. With exactly one provider key, it also limits `auto`
routing to that provider (`SWITCHYARD_ALLOW`). It never overwrites an
existing `.env` (pass `--force` to start over, which replaces the keys).

From the repository root, `make compose-up` does the same steps, with
`PROFILES=observability` (or `tei`, or both) for the optional services and
`DOCKER="colima ssh -- docker"` when the host Docker CLI has no Buildx.

What runs, all published on 127.0.0.1 only:

| Service | Host port | Role |
|---|---|---|
| `gateway` | 8080 (`SWITCHYARD_PORT`) | The API. Read-only root filesystem, non-root, no capabilities. |
| `gateway` admin | 9090 (`SWITCHYARD_ADMIN_PORT`) | `/metrics`, `/healthz` and `/readyz` (open) and `/admin/v1/*` (admin token). |
| `redis-cache` | 6379 | Response cache: `allkeys-lru`, 256 MB, no persistence. |
| `redis-state` | 6380 | Budgets, rate limits, cache namespaces: `noeviction`, AOF. |
| `postgres` | 5433 | Usage ledger. |

To serve other machines, set `SWITCHYARD_BIND=0.0.0.0` in `.env` (and put
TLS in front, or set `server.tls`). Never publish the admin port.

### First request

```sh
. ./.env     # loads SWITCHYARD_API_KEY into this shell
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hello"}]}'
```

`auto` lets the router pick the cheapest model whose estimated quality
clears the `balanced` profile; `auto:cheap`, `auto:quality` and `auto:fast`
pick other profiles, and a concrete name such as `gpt-6-luna` or
`claude-haiku-5-5` asks for that model. Add `-i` to see why: the
`X-Switchyard-Model`, `X-Switchyard-Provider`, `X-Switchyard-Route-Reason`,
`X-Switchyard-Cost-Usd` and `X-Switchyard-Cache` headers. Send the same
request again and it is answered from the exact cache (`X-Switchyard-Cache:
exact`, cost 0).

Any OpenAI SDK works against the gateway:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="<SWITCHYARD_API_KEY>")
print(client.chat.completions.create(model="auto", messages=[{"role": "user", "content": "Say hello"}]).choices[0].message.content)
```

### Reading savings

Every request is recorded in the usage ledger with its exact cost and its
baseline cost: what the same tokens would have cost on the policy's
`baseline_model` (`claude-sonnet-5-5` in `deploy/config/switchyard.yaml`). Your
key reads its tenant's report:

```sh
curl -s "localhost:8080/switchyard/v1/usage?group_by=model" \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY" | jq .totals
```

`cost_usd` is what you paid, `baseline_cost_usd` what you would have paid,
and `savings_usd` splits the difference into `routing` (cheaper models and
providers) and `cache` (answers served from the cache), with `net` the
total. `group_by` also takes `day`, `deployment`, `provider`, `key`,
`profile`, `category` and `tag:<key>` (from the `X-Switchyard-Tags` request
header). Reports across tenants are on the admin listener:

```sh
curl -s "127.0.0.1:9090/admin/v1/usage?group_by=day" \
  -H "Authorization: Bearer $SWITCHYARD_ADMIN_TOKEN"
```

### Dashboards and traces

```sh
docker compose --profile observability --env-file .env --env-file observability.env up -d --wait
```

`observability.env` turns tracing on (or set `SWITCHYARD_TRACING_ENABLED=true`
in `.env`). Every later command that recreates the gateway, such as `docker
compose up -d --force-recreate gateway` below, must then carry the same
`--profile observability --env-file .env --env-file observability.env`, or
the new container starts without the overlay and tracing goes off. Setting
the variable in `.env` instead avoids that. Then:

- **Grafana** at <http://127.0.0.1:3000> (user `admin`, password
  `GRAFANA_ADMIN_PASSWORD`, default `admin`) opens the Switchyard dashboard:
  traffic, overhead, upstream errors, breakers, cost and savings, cache hit
  rate, routing mix and relaxations.
- **Prometheus** at <http://127.0.0.1:9091>, with the alert rules loaded;
  firing alerts are at `/alerts`. There is no Alertmanager in the stack:
  add yours under `alerting:` in `prometheus/prometheus.yml`.
- **Jaeger** at <http://127.0.0.1:16686>: one trace per request, with
  spans for auth, rate limiting, policy, the caches, routing, budget
  admission, each upstream attempt and usage recording.
  `SWITCHYARD_TRACING_SAMPLE_RATIO` (default 1 here) sets the share of new
  traces sampled; lower it for real traffic.

### A local embedding model

`--profile tei` adds Hugging Face Text Embeddings Inference on CPU serving
`BAAI/bge-small-en-v1.5`, for the semantic cache without sending prompts to
a provider. Set `SWITCHYARD_SEMANTIC_CACHE=true` and
`SWITCHYARD_EMBEDDING_MODEL=bge-small-en-v1.5` in `.env`, then
`docker compose --profile tei up -d --wait`. The default policy always
permits `bge-small-en-v1.5@tei`, whatever `SWITCHYARD_ALLOW` is, so this
works with the `.env` `init.sh` wrote. The image is over 1 GB and the
model is downloaded on first start.

### Changing the configuration

The gateway reads [deploy/config/switchyard.yaml](../deploy/config/switchyard.yaml) and the
curated catalog in [configs/catalog](../configs/catalog), both mounted
read-only, as directories: an edit saved by renaming a new file over the old
one, as most editors and `sed -i` do, is seen too. After an edit, reload
without dropping requests:

```sh
docker compose kill -s HUP gateway
# or
curl -s -X POST -H "Authorization: Bearer $SWITCHYARD_ADMIN_TOKEN" 127.0.0.1:9090/admin/v1/reload
```

An invalid file is rejected and the running config keeps serving; the
reload response and log say why. Keys, tenants, policies, the catalog,
routing, budgets, cache policies and rate limits apply at once. Changes to
the listeners and other `server.*` settings (except `admin_token`), TLS,
`storage`, `logging`, `telemetry`, `evaluation`, `cache` and
`semantic_cache` are reported as `restart_required` and need
`docker compose up -d --force-recreate gateway`; the full list is in the
[runbook](runbook.md#config-reload-failed). A change to `.env` also needs
that.

Add gateway keys with `switchyard keys generate` (from a `make build`
binary, or `docker compose run --rm --no-deps --entrypoint
/usr/local/bin/switchyard gateway keys generate`), paste the printed entry
under `tenants[0].keys`, and reload.

### Before production

- Set `POSTGRES_PASSWORD` in `.env` **before the first start** (it is fixed
  when the volume is created) and keep Redis and Postgres on the private
  compose network; remove their `ports:` in `compose.yaml` if nothing on
  the host needs them. For managed Redis or Postgres, set `REDIS_CACHE_URL`,
  `REDIS_STATE_URL` and `DATABASE_URL` in `.env`, with passwords and TLS
  (`rediss://`, `sslmode=verify-full`), and recreate the gateway: compose
  passes them to the container, and `deploy/config/switchyard.yaml` reads
  them. Unset, they default to the stack's own services.
- Keep `.env` out of version control and backups you share; prefer
  `${file:/run/secrets/...}` references with Docker secrets for the
  provider keys and admin token.
- Review `deploy/config/switchyard.yaml`: the monthly budget
  (`SWITCHYARD_MONTHLY_BUDGET_USD`, default 100), the per-key rate limit
  (600 requests a minute, 20 concurrent streams) and the per-request cost
  cap (0.50 USD).
- Lower `SWITCHYARD_TRACING_SAMPLE_RATIO`, and route the alerts.

`docker compose down` stops everything and keeps the data; `down -v`
deletes it.

## Single binary

You need Go 1.26 or newer.

```sh
make build                                   # writes bin/switchyard
bin/switchyard keys generate                 # prints a key and a config snippet
```

Paste the printed `tenants:` snippet at the end of `configs/quickstart.yaml`
(or a copy of it), then:

```sh
export OPENAI_API_KEY=sk-...
bin/switchyard serve --config configs/quickstart.yaml
```

```sh
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sy_..." -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-luna","messages":[{"role":"user","content":"Say hello"}]}'
curl -s localhost:8080/switchyard/v1/usage -H "Authorization: Bearer sy_..." | jq .totals
curl -s 127.0.0.1:9090/metrics | grep switchyard_requests_total
```

The quickstart config has one OpenAI model and no routing policy, keeps
everything in memory, and binds the admin listener to loopback, where it
needs no admin token. The container image runs the same config:
`docker run --rm -p 8080:8080 -e OPENAI_API_KEY switchyard:dev` (after
`make docker`); to add keys, mount a directory with your own config and
pass `--config` (see the README's Docker section). For every key, see [configuration.md](configuration.md) and the
commented [configs/switchyard.example.yaml](../configs/switchyard.example.yaml).
