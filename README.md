# Switchyard

Switchyard is a cost-aware, OpenAI-compatible LLM inference gateway written in Go. Applications point their OpenAI SDK at it, and it routes each request to the cheapest deployment that is good enough, across OpenAI, Anthropic, Gemini and self-hosted open-source models. It also handles fallback, caching, budgets and cost reporting.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Delivery plan and acceptance criteria: [docs/PLAN.md](docs/PLAN.md)

## Status

**v0.1: Phases 0 to 8 are done** (see [CHANGELOG.md](CHANGELOG.md)); the v0.1.0 tag is not cut yet. Switchyard serves `POST /v1/chat/completions` and `POST /v1/completions` (JSON and streaming), `POST /v1/embeddings` and `GET /v1/models` behind gateway API keys. Each request goes to the cheapest deployment that has every capability it needs and meets its policy, on OpenAI, Anthropic (translated from and to the OpenAI format), Gemini's OpenAI-compatible endpoint or any OpenAI-compatible host. Failures are retried and fall back to the next cheapest deployment of the same model, within a deadline and a retry budget, with circuit breakers, rate-limit cooldowns and per-provider concurrency limits (see [Reliability](#reliability)). Every request gets an exact cost in a usage ledger (memory or Postgres) with tenant-scoped reports, and tenants and keys can have daily or monthly budgets (see [Usage ledger and reporting](#usage-ledger-and-reporting) and [Budgets](#budgets)). With `model: "auto"`, a policy with a baseline model, and quality priors in the catalog, the gateway picks the cheapest model whose estimated quality for the request clears the profile's bar, explains every decision, and measures the savings against the baseline (see [Routing](#routing)). `switchyard eval` measures the quality priors on your own prompts, and opt-in shadow sampling and client feedback check on live traffic that the savings do not cost quality (see [Evaluation and verification](#evaluation-and-verification)). Policies can turn on an exact response cache and an opt-in semantic cache for chat completions (see [Caching](#caching)). For production there are per-key rate limits and concurrent-stream caps shared through Redis, OpenTelemetry tracing, hot reload, an admin token, TLS, a compose stack with dashboards and alerts, and operator docs (see [Rate limits](#rate-limits), [Tracing](#tracing), [Hot reload](#hot-reload), [Admin API and TLS](#admin-api-and-tls) and [Operations](#operations)).

Not verified on this machine, which has no provider keys: the live smoke test, fixture recording, a live judge or semantic-cache check, and the GitHub Actions workflows, which have not run yet.

## Quickstart

With Docker and one provider key, the full stack (gateway, Redis for cache
and state, Postgres for the usage ledger) runs in a few minutes:

```sh
cd deploy
export OPENAI_API_KEY=sk-...        # or ANTHROPIC_API_KEY=...
./init.sh                           # writes .env: admin token, a gateway API key
docker compose up -d --wait

. ./.env
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hello"}]}'
curl -s localhost:8080/switchyard/v1/usage -H "Authorization: Bearer $SWITCHYARD_API_KEY" | jq .totals.savings_usd
```

Add `--profile observability` for Prometheus with the alert rules, Grafana
with the Switchyard dashboard, and Jaeger for traces. Without Docker, `make
build` and `bin/switchyard serve --config configs/quickstart.yaml` run a
single in-memory gateway.

- [docs/quickstart.md](docs/quickstart.md): both paths, the first request,
  reading savings, dashboards, and hardening before production.
- [docs/configuration.md](docs/configuration.md): every config key, with its
  type and default.
- [docs/runbook.md](docs/runbook.md): symptoms, alerts and fixes for every
  degraded mode.

## Configuration basics

**Which file.** `serve` and `config validate` read the file given by `-c`/`--config`, else the one named by `SWITCHYARD_CONFIG`, else `./switchyard.yaml`.

**Precedence.** Settings resolve from built-in defaults, then the YAML file and its includes, then a few operational environment overrides (`SWITCHYARD_LISTEN`, `SWITCHYARD_ADMIN_LISTEN`, `SWITCHYARD_LOG_LEVEL`), then CLI flags such as `serve --log-level`. Later sources win.

**References.** Secrets are never written literally into config. Any value can reference the environment or a file:

| Form | Meaning |
|---|---|
| `${VAR}` | The environment variable `VAR`. It is an error if it is unset. |
| `${VAR:-default}` | `VAR`, or `default` when it is unset. `${VAR:-}` means empty when unset. |
| `${file:/run/secrets/name}` | The contents of a file, for Docker and Kubernetes secrets. |

**Includes.** `include:` lists glob patterns, relative to the root file's directory. Included files may only define `providers`, `models`, `aliases` and `quality_overrides`; list entries are appended in sorted file-name order, and a duplicate ID or alias name, in any files, is an error. The example config includes the model catalog from [configs/catalog/](configs/catalog/).

**Limits.** `server.max_header_bytes` (default 64 KiB) caps request headers; net/http allows up to 4 KiB beyond it before it answers 431. A `${file:…}` reference reads only a regular file of at most 64 KiB.

**Anchors and merge keys.** YAML anchors, aliases and merge keys (`<<: *base`) work within one file, except that `include:` and the top-level keys of an included file must be written out. Keys written next to a merge key override the merged ones, and in a merge sequence (`<<: [*a, *b]`) the earlier mapping wins. Nested references such as `${A:-${B}}` are not supported and are reported as errors.

**Strict validation.** Unknown keys are rejected. Validation does not stop at the first problem: it reports every error at once, each with its file, line and config path, such as `providers[2].base_url`. Secret values never appear in errors, logs or CLI output: messages name a referenced value by its reference (`the value of ${VAR}`), and describe a literal value or key that looks like a credential only as `a string value` or `<hidden key>`. A credential written literally, including as the default of `${VAR:-default}`, gets a warning.

Every supported key is documented, with its default, in [configs/switchyard.example.yaml](configs/switchyard.example.yaml).

## Model catalog

The catalog says which models exist, which providers serve them, what each deployment can do and what it costs. It is ordinary config, usually kept in its own files under `include:` (see [configs/catalog/](configs/catalog/)), and follows DESIGN §16.4.

```yaml
models:
  - id: llama-3.3-70b                  # the name clients send
    type: chat                         # chat (default) or embedding
    context_window: 131072             # input + output tokens; 0 = unknown, no context filter
    max_output_tokens: 8192            # also the default max_tokens for Anthropic
    capabilities: [streaming, tools, json_mode, sampling]
    quality: { general: 0.70, coding: 0.62 }   # priors in [0, 1], used from Phase 5
    token_ratio: 1.05                  # scales the bytes/4 token estimate; default 1
    deployments:                       # each is model@provider
      - provider: together
        upstream_model: meta-llama/Llama-3.3-70B-Instruct-Turbo
        pricing: { input: 0.88, output: 0.88, source: "https://…", checked: 2026-11-11 }
      - provider: gpu-cluster
        upstream_model: meta-llama/Llama-3.3-70B-Instruct
        capabilities: [streaming, tools, json_mode]   # replaces the model's list here
        tags: [quantized]                             # added to the provider's tags
        pricing: { input: 0.10, output: 0.10 }        # amortized

  - id: text-embedding-3-small
    type: embedding
    dimensions: 1536                   # required for embedding models
    deployments:
      - { provider: openai, upstream_model: text-embedding-3-small, pricing: { input: 0.02 } }

aliases:                               # operator-defined names
  code-model: llama-3.3-70b            # a model id…
  cheap-llama: llama-3.3-70b@gpu-cluster   # …or a pinned model@provider

routing:
  drop_unsupported_params: false
```

**Prices** are USD per million tokens (`input`, `output`, `cached_input`, `cache_write`), written as a number or a decimal string such as `"0.0375"`. Negative values, exponents and more than 9 decimal places are rejected, and the exact text is kept for cost accounting. A chat deployment without both `input` and `output`, or an embedding deployment without `input`, gets the warning "pricing unknown; this deployment is ranked last".

**Capabilities** come from a fixed vocabulary: `streaming`, `stream_usage`, `tools`, `parallel_tools`, `forced_tools`, `vision`, `json_mode`, `json_schema`, `n`, `logprobs`, `seed`, `sampling`, `temperature_2`, `penalties`, `completions` and `prompt_caching`. A request is only sent to a deployment that has every capability it needs. With `routing.drop_unsupported_params: true`, the parameters behind `n`, `logprobs`, `seed`, `penalties`, `temperature_2` and `parallel_tools` are removed instead, and the response lists them in `X-Switchyard-Dropped-Params`. `sampling` (the deployment accepts `temperature` and `top_p`) is always dropped for a deployment without it, because such models document fixed sampling. Every other capability is a hard requirement. Embedding models can't have chat capabilities such as `tools`.

**The `model` field** resolves as follows. A catalog id uses that model's cheapest eligible deployment, and `model@provider` pins one deployment. An alias stands for either; aliases don't chain. A name that starts with `auto` is reserved for virtual models (Phase 5), and anything else is unknown.

**Choosing a deployment.** The candidates that pass the hard filters (capabilities, and context window ≥ estimated input + output tokens) are ranked by estimated cost. Unknown prices rank last, and ties keep config order. If nothing is eligible, the error names every deployment and why it was excluded.

**Stream usage.** For a streamed request to a deployment with `stream_usage`, the gateway sets `stream_options.include_usage` so it can log the usage. A deployment without it gets the client's `stream_options` unchanged (or none), because some hosts reject the option; usage may then be missing, and the request line omits `prompt_tokens` and `completion_tokens` instead of logging 0. They are logged as 0 only when no tokens were spent: the request was rejected before or by the provider. When a provider repeats usage on several chunks (Gemini's endpoint may), the last one is kept.

**The curated catalog.** [configs/catalog/models.yaml](configs/catalog/models.yaml) lists current OpenAI, Anthropic, Gemini, Together and self-hosted models with prices checked on 2026-11-11, each with its source. [configs/catalog/README.md](configs/catalog/README.md) explains every price and capability decision. The self-hosted vLLM deployment is commented out until you set its amortized price.

**`switchyard catalog lint [-c FILE]`** loads the config the same way `config validate` does. It then reports catalog findings, one per line in the same format as config problems, followed by a summary. It warns about:
- missing prices;
- a missing `pricing.source` or `pricing.checked`;
- a `pricing.checked` date more than 90 days old.

It prints notices, which don't fail it, for an output price below the input price and for a chat model without `streaming`. It exits 0 when there are no warnings, 1 on warnings or an invalid config, and 2 on usage errors. Errors that `config validate` reports, such as a quality prior outside [0, 1], an unknown provider or a duplicate model id, fail it too.

```sh
$ bin/switchyard catalog lint -c configs/switchyard.example.yaml   # with the provider keys set
configs/switchyard.example.yaml: catalog lint OK (11 models, 3 aliases; 0 warnings, 0 notices)
```

## Providers

Each provider has a `type`, its wire protocol.

- **`openai`**: OpenAI and every OpenAI-compatible host (Together, Fireworks, Groq, vLLM, Ollama and others). Requests are forwarded losslessly: the body reaches the provider unchanged except for `model` and the parameters the gateway removes (see [Model catalog](#model-catalog)). `base_url` is the API root that ends in `/v1`. Declare each host's capabilities per deployment; a host that accepts fewer parameters than the model's list says gets a deployment-level `capabilities` list.
- **`anthropic`**: the Messages API at `{base_url}/v1/messages`, with `base_url: https://api.anthropic.com`. The adapter translates the OpenAI request into a Messages request and the answer (or every stream event) back into `chat.completion` objects, following DESIGN §6.2: system and developer messages become the system prompt, tools and tool results map one to one, images must be data URLs, `max_tokens` defaults to the model's `max_output_tokens`, `stop` becomes `stop_sequences`, and `user` becomes `metadata.user_id`. `response_format: json_schema` uses native structured outputs, so only a deployment with `json_schema` gets it; the schema is reduced to the subset Anthropic supports, as Anthropic's SDKs do (unsupported keywords such as `minimum` or `maxLength` move into the field's description, and every object gets `additionalProperties: false`), whether or not the client set `strict`. `json_object` is a system-prompt instruction. Tool-call ids that don't match Anthropic's pattern are mapped to valid ones. Usage is normalized (cache reads into `prompt_tokens_details.cached_tokens`), and errors map to OpenAI envelopes, with 529 `overloaded_error` as a 502. Fields without a Messages API equivalent are never forwarded, so the catalog must not declare `n`, `logprobs`, `seed`, `penalties` or `temperature_2` for Anthropic deployments. Client headers such as `anthropic-beta` pass only when listed in `forward_headers`.
- **Gemini** uses Google's OpenAI-compatible endpoint with `type: openai`:

  ```yaml
  - id: google
    type: openai
    base_url: https://generativelanguage.googleapis.com/v1beta/openai
    api_key: ${GEMINI_API_KEY}
  ```

  Google calls this endpoint beta. The curated catalog declares only what Google's compatibility page documents (see [configs/catalog/README.md](configs/catalog/README.md)).

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
  -d '{"model":"gpt-6-luna","messages":[{"role":"user","content":"Hello"}]}'
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

**2. Add the tenant and a model.** Paste the printed entry into your config next to a provider and a model. The request's `model` is resolved through the catalog (a model id, an alias or `model@provider`; see [Model catalog](#model-catalog)), and the chosen deployment gets the request with `model` rewritten to its `upstream_model`. A model needs a `capabilities` list: a request that needs a capability the model lacks, `streaming` included, is never sent to it.

```yaml
providers:
  - id: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
models:
  - id: gpt-6-luna
    capabilities: [streaming, stream_usage, vision, json_mode, json_schema, prompt_caching]
    deployments:
      - provider: openai
        upstream_model: gpt-6-luna
        pricing: { input: "0.10", output: "0.50", source: "https://developers.openai.com/api/docs/pricing", checked: "2026-11-11" }
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
    model="gpt-6-luna",
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
  -d '{"model":"gpt-6-luna","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

**What is forwarded.** On an OpenAI-wire provider the request body reaches the provider unchanged, unknown fields included, except that `model` becomes the deployment's `upstream_model`, parameters the deployment cannot honor are removed (listed in `X-Switchyard-Dropped-Params`), and streams to a deployment with `stream_usage` ask for `stream_options.include_usage`, so usage is logged. An Anthropic deployment gets a translation instead (see [Providers](#providers)). The usage-only final chunk is passed on only to clients that set `include_usage` themselves. Non-streaming responses are returned byte for byte. The client's `Authorization` header is never forwarded; the provider's `api_key` is sent instead, along with only the client headers listed in the provider's `forward_headers`. When the client disconnects, the upstream request is canceled.

**Response headers.**

| Header | Value |
|---|---|
| `X-Request-Id` | The gateway's request ID, also in every log line for the request. |
| `X-Switchyard-Model` | The catalog model that served the request; with `auto`, or after a cross-model fallback, the model routing chose. |
| `X-Switchyard-Provider` | The provider id of the deployment that answered (on an error, of the last one tried). |
| `X-Switchyard-Attempts` | The number of upstream attempts, retries and fallbacks included; `0` on a cache hit. |
| `X-Switchyard-Route-Reason` | `cheapest_eligible`, `pinned` (a `model@provider` request), `preferred` (a policy `prefer` tier decided over a cheaper deployment), `quality_relaxed` or `latency_relaxed` (no deployment met the soft constraint, so it was relaxed), `budget_downgrade` (a budget's `soft_profile` is in force), `observe` (observe mode served what was asked for), or `fallback:<kind>` when a fallback answered, `<kind>` being the failure that caused the first fallback, such as `fallback:server_error`; `cache_exact` or `cache_semantic` for a cache hit. See [Routing](#routing) and [Caching](#caching). |
| `X-Switchyard-Profile` | The routing profile in force (chat and legacy completions). |
| `X-Switchyard-Policy-Warning` | The request headers the policy ignored because they would have loosened it, `; `-separated. |
| `X-Switchyard-Shadow-Model` | In observe mode, the model routing would have chosen. |
| `X-Switchyard-Alias` | The alias the client sent, when it sent one. |
| `X-Switchyard-Dropped-Params` | The request parameters removed because the deployment cannot honor them, such as `n, temperature`. |
| `X-Switchyard-Shim` | `chat` when a legacy completion was served through Chat Completions. |
| `X-Switchyard-Upstream-Request-Id` | The provider's own `x-request-id`, from the attempt that answered, for support tickets. Omitted unless it is at most 128 bytes of printable ASCII. |
| `X-Switchyard-Cost-Usd` | The request's exact cost in USD, such as `0.000165`, on a successful non-streaming response whose usage the provider reported and whose deployment has prices; `0` on a non-streaming cache hit. A stream's cost is in the [usage ledger](#usage-ledger-and-reporting). |
| `X-Switchyard-Cache` | `miss`, `exact` or `semantic`, on every chat completion response of a request its policy caches; omitted when caching is off for the request. See [Caching](#caching). |
| `Retry-After` | On 429, copied from the provider; on `budget_exceeded`, the seconds until the budget's window resets. |

Streams carry `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The response is committed only when the first upstream event arrives, so failures before that are retried or fall back, and otherwise get a normal error status. A client can shorten its request's deadline with the request header `X-Switchyard-Timeout-Ms: <milliseconds>`; it can never extend it past `routing.request_timeout`. It can attach attribution tags to the request's usage event with `X-Switchyard-Tags: team=search,feature=autocomplete` (see [Usage ledger and reporting](#usage-ledger-and-reporting)), and narrow its policy's caching with `X-Switchyard-Cache: off`, `no-store` or `exact-only` (see [Caching](#caching)).

**Errors.** Every error uses OpenAI's envelope, `{"error":{"message","type","param","code"}}`, so SDKs raise their usual exceptions.

| Situation | Status | `code` |
|---|---|---|
| Missing, malformed or unknown gateway key | 401 | `invalid_api_key` |
| Invalid JSON, missing `model` or `messages`, `model` over 256 bytes, non-boolean `stream` | 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `invalid_type` or `empty_array`; `param` names the field |
| Body larger than `server.max_request_bytes` | 413 | `request_too_large` |
| Body not received within `server.body_read_timeout` (default 60s) | 408 | `request_timeout` |
| Unknown model or `model@provider` | 404 | `model_not_found` |
| `auto` or `auto:<profile>` the policy does not allow (no `baseline_model`, an unknown or disallowed profile), a virtual model on embeddings, or a model of the wrong type (an embedding model on chat, a chat model on embeddings) | 400 | `model_not_supported` |
| A malformed `X-Switchyard-Profile`, `-Max-Cost-Usd`, `-Min-Quality`, `-Max-TTFT-Ms`, `-Task`, `-Difficulty` or `-Cache` header, or one given twice; the value is never echoed | 400 | `invalid_header`; `param` names the header |
| No deployment has every capability the request needs, fits the context window, passes the policy (deny, allow, `require_tags`, cost cap), or (with `on_unmet: reject`) meets the soft constraints | 400 | `no_eligible_model`; the message names each deployment and why it was excluded |
| A legacy completion parameter the chat shim cannot honor | 400 | `unsupported_parameter`; `param` names it |
| `X-Switchyard-Timeout-Ms` not a positive whole number | 400 | `invalid_timeout` |
| `X-Switchyard-Tags` malformed (see [Usage ledger and reporting](#usage-ledger-and-reporting)); the value is never echoed | 400 | `invalid_tags` |
| A blocking budget has no room for the request's estimated cost; no upstream is called | 429, with `Retry-After`; `type` is `insufficient_quota` | `budget_exceeded` |
| The budget store (Redis) is down and `budgets.fail_mode` is `closed` | 503 | `budget_unavailable` |
| A request an adapter cannot translate (for example a content part or tool type the Anthropic Messages API cannot express), when no other deployment in the plan could serve it | 400 | `invalid_value`; `param` names the field |
| Every eligible deployment is unhealthy: breaker open, rate-limit cooldown, or provider at `max_concurrency` | 503 | `no_healthy_upstream`; the message names each deployment and why |
| Provider 4xx (other than 401/402/403), including 429 | the provider's status | the provider's body |
| Provider 401/402/403: the gateway's credential or account is wrong | 502 | `upstream_error` (the body is never echoed) |
| Provider 5xx, connection failure | 502 | `upstream_error` |
| First-token (streams), idle or total timeout, or the request's deadline | 504 | `timeout` |
| Provider failure after the stream started | an SSE `data: {"error":…}` event, then the stream closes without `[DONE]` | `upstream_error` |

When a request was retried or fell back and every attempt failed, the answer is the most informative failure: 504 if the deadline expired, the provider's 429 if every failure was a rate limit, and otherwise, in order of preference, a context-length or not-found error, a 5xx, a timeout, an authentication failure, a connection failure, and last an adapter's local rejection (`unsupported`), which is the answer only when every attempt was rejected so. The `X-Switchyard-Provider` and `X-Switchyard-Upstream-Request-Id` headers are those of the attempt whose failure is the answer.

Each request is logged at INFO as one `request` access-log line. For the chat, completions and embeddings endpoints it also carries the tenant, key id, requested model, the `deployment` of the last attempt, the number of `excluded` deployments, `attempts`, `attempt_chain` (each attempt as `deployment:outcome`, such as `m@a:server_error,m@a:server_error,m@b:ok`, at most ten), `fallback_reason` (the failure kind that caused the first fallback, when there was one), `deadline_ms`, latency, TTFT, gateway overhead, token counts and `cost_usd` (when the cost is known), plus the `alias` and the `dropped_params` (comma-separated) when there are any, never the prompt or the response. Chat and legacy completion requests also log their routing: `profile`, `matched_rule` and `matched_quality_rule` (when a rule of each layer applied), `policy`, `routing_mode`, the assessment's `category` and `difficulty`, `quality_bar`, `served_q` (the served deployment's estimated quality), `relaxed` (the relaxed soft constraints, when any) and, in observe mode, `shadow_deployment`.

**Legacy completions.** `POST /v1/completions` takes the legacy OpenAI shape (`prompt` a string or an array of strings; token arrays get 400). A deployment with the `completions` capability on an OpenAI-wire provider gets the request natively and losslessly. Any other deployment is served through the chat shim, marked `X-Switchyard-Shim: chat`: the prompt becomes one user message; `max_tokens`, `temperature`, `top_p`, `stop`, `n`, `stream`, `stream_options`, `user`, `seed` and the penalties carry over (other legacy fields, such as `logit_bias`, are not sent); and the answer, streamed or not, comes back as `text_completion` objects with `choices[].text`. The shim cannot honor `echo`, `suffix`, `logprobs`, `best_of` above 1 or more than one prompt: such a request goes to a native deployment if the model has one, and otherwise gets 400 `unsupported_parameter`. Capability filtering and dropping work as for chat. The legacy default of 16 `max_tokens` is not applied by the shim.

```sh
curl localhost:8080/v1/completions -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -H "Content-Type: application/json" -d '{"model":"gpt-6-luna","prompt":"Say hello","max_tokens":20}'
```

**Embeddings.** `POST /v1/embeddings` takes `model`, `input` (a string, an array of strings, or token arrays, which are passed through), `encoding_format` (`float` or `base64`), `dimensions` and `user`, and is forwarded losslessly. The model must be an embedding model, never `auto`. Its deployments are tried cheapest input price first, with retries and fallback as for chat, so the provider may change but the model never does: vectors from different models are not interchangeable. The policy's `deny`, `allow` and `require_tags` apply to embedding deployments, and so does its `max_cost_per_request_usd` (or a lower `X-Switchyard-Max-Cost-Usd`), against each deployment's estimated input cost; its profiles, substitution and routing mode do not. Only OpenAI-wire providers serve embeddings; an Anthropic deployment is excluded with "provider does not serve embeddings".

**Models.** `GET /v1/models` lists every catalog model, chat and embedding, then the virtual models the key's policy allows (`auto` and `auto:<profile>` for each profile it may select; none without a `baseline_model`), then every alias, all with `owned_by: "switchyard"`. Metrics (`switchyard_requests_total`, which also counts requests rejected for a bad key, `switchyard_upstream_requests_total`, `switchyard_overhead_duration_seconds` and others) are on the admin listener's `/metrics`.

## Reliability

Every request runs on a **plan**: the deployments that can serve it, best first (see [Routing](#routing)), at most `routing.fallback.max_deployments` (default 3). Each fallback slot prefers a provider that is not in the plan yet. A pinned `model@provider` is a plan of one. A concrete model name under `pinned` substitution (the default) plans only that model's deployments, so fallback never switches models; `auto` and `equivalent` substitution plan across models, so a fallback may be another model that passed the same checks. Deployments that are unhealthy right now are left out of the plan: an open circuit breaker, a rate-limit cooldown, or a provider at its `max_concurrency`. If that leaves none, the answer is 503 `no_healthy_upstream`, naming each deployment and why ("circuit open", "rate-limit cooldown 4s", "provider at max_concurrency 64"). A request that no deployment could honor at all still gets 400 `no_eligible_model`.

Each plan entry gets up to `1 + retries_per_deployment` attempts, then the next entry is tried. What happens after a failure depends on its kind:

| Kind | Typical upstream signal | Retry the same deployment | Fall back | Counts toward the breaker |
|---|---|---|---|---|
| `transport` | connection refused or reset, DNS, TLS | yes | yes | yes |
| `timeout` | connect, first token, idle between chunks, total | yes, except a non-streaming call's `total` timeout | yes | yes |
| `server_error` | 500, 502, 503, 504, Anthropic 529 | yes | yes | yes |
| `rate_limited` | 429 | only if `Retry-After` fits the remaining deadline | yes | no; a cooldown starts instead |
| `auth` | 401, 402, 403 on the gateway's credential | no | yes | yes, and an ERROR log line |
| `not_found` | 404, unknown upstream model | no | yes | no |
| `context_length` | OpenAI `context_length_exceeded`; Anthropic "prompt is too long" or 413 | no | yes | no |
| `content_filter` | OpenAI `content_policy_violation` or `content_filter` | no | no | no |
| `client_error` | any other 400 or 422 | no | no | no |
| `unsupported` | the adapter cannot translate the request (Anthropic: file or audio parts, the deprecated `functions`, non-function tools, a `response_format` the deployment lacks the capability for); nothing is sent upstream | no | yes | no |
| `canceled` | the client went away | no | no | no |

A successful answer is never retried, including one whose `finish_reason` is `content_filter` and an Anthropic refusal (`stop_reason: refusal`): a refused request is never sent to another provider.

- **Retries** wait with exponential backoff and full jitter, a random time in `[0, min(backoff_max, backoff_base × 2^n)]` before retry n, or the provider's `Retry-After` for a 429. A wait that does not fit the remaining deadline is skipped and the next deployment is tried at once. Each deployment also has a **retry budget**: its retries may not exceed `retry_budget_ratio` (default 20 %) of its requests over a sliding 10 s window, plus `retry_budget_min` (default 5) that are always allowed. Once it is spent, the gateway falls back instead of retrying, so an outage cannot turn into a retry storm.
- **Deadline.** `routing.request_timeout` (default 10m) bounds the whole request, across every attempt, backoff and the full streamed response. `X-Switchyard-Timeout-Ms` can lower it. Each attempt also has its provider's own `timeouts`. When the deadline expires the answer is 504 `timeout`.
- **Provider timeouts.** `connect` bounds connection setup. `first_token` applies to streams only: it bounds the wait for the first event. A non-streaming response arrives only after the whole generation, so `first_token` does not apply to it (chat, completions or embeddings); `total` and the request's deadline bound it, and a non-streaming call that exceeds `total` falls back to the next deployment without a retry on the same one, which would repeat a long, billed generation (it still counts toward the breaker). `idle` bounds each gap between stream events, measured only while the gateway waits for the upstream: time spent writing to a slow client does not count. `total` bounds the whole upstream call.
- **Slow clients.** `server.write_timeout` (default 60s) bounds each write to the client, a whole JSON response or one stream event. A client that stops reading for longer is disconnected, which cancels its upstream call; that is not counted against the deployment.
- **Streams** are retried or fall back only until the first upstream event. Once the response has started, nothing is retried: a failure is sent as an error event, which the OpenAI SDKs raise, and the stream closes.
- **Circuit breakers**, one per deployment and replica, open after `consecutive_failures` failures in a row, or when at least `min_requests` requests within `window` failed at `failure_ratio` or more. Only the kinds marked above count. An open breaker skips the deployment for `open_for`, then lets `half_open_probes` concurrent requests through: one success closes it, one failure opens it again. A timeout caused by the request's own deadline, or a client that went away, is not counted against the deployment. A half-open probe ends when the upstream answers, for a stream at its first event, so a probe that is a long stream closes the breaker at once. Upstream failures after that point still count, as any failure does: an idle timeout, a dropped connection, an error event mid-stream. The provider's `total` timeout mid-stream (the request's own budget), and a client that went away or stopped reading for longer than `server.write_timeout`, do not.
- **Rate-limit cooldowns.** After a 429, the deployment is skipped for `Retry-After` (or `rate_limit_cooldown.default` without one), at most `rate_limit_cooldown.max`.
- **Concurrency.** A provider's `max_concurrency` caps its in-flight requests on each replica; a saturated provider is skipped without queueing, so traffic spills over to the next cheapest deployment.
- **Keep-alives** (`routing.stream_keepalive`, off by default): for intermediaries that close idle connections while a model is slow to start, a stream that has not received its first upstream event after that long is committed, with 200, the SSE headers and a `: keep-alive` comment, repeated at that interval until the first event. SDKs ignore the comments. This is a trade-off: after the first keep-alive, fallback is no longer possible, and a later failure becomes an error event in the stream.

All of it is configured under `routing`, shown here with the defaults (each key is documented in [configs/switchyard.example.yaml](configs/switchyard.example.yaml)):

```yaml
routing:
  request_timeout: 10m
  fallback:
    max_deployments: 3          # 1..10
    retries_per_deployment: 1   # 0..5
    backoff_base: 250ms
    backoff_max: 2s
    retry_budget_ratio: 0.2     # (0, 1]
    retry_budget_min: 5
  breaker:
    consecutive_failures: 5
    failure_ratio: 0.5          # (0, 1]
    min_requests: 20
    window: 30s
    open_for: 30s
    half_open_probes: 3
  rate_limit_cooldown:
    default: 1s
    max: 30s
  stream_keepalive: 0s          # 0s is off
```

**Visibility.** Besides the response headers and log fields above, the admin listener's `/metrics` has `switchyard_retries_total{deployment,kind}`, `switchyard_fallbacks_total{from,to,kind}`, `switchyard_circuit_state{deployment}` (0 closed, 1 half-open, 2 open), `switchyard_circuit_transitions_total{deployment,to}`, `switchyard_rate_limit_cooldowns_total{deployment}`, `switchyard_provider_in_flight{provider}`, `switchyard_provider_saturated_total{provider}` (attempts refused because the provider reached `max_concurrency` between planning and the attempt; routing around a saturated provider, and explaining a request, count nothing) and `switchyard_retry_budget_exhausted_total{deployment}`. `switchyard_upstream_requests_total` counts every attempt, by outcome. `GET /admin/v1/providers` on the admin listener reports, for every provider, its `in_flight` count and `max_concurrency`, and for each of its deployments the breaker state and the time of its next transition, `cooldown_until`, the recent request and failure counts, the retry budget used, and the TTFT and tokens-per-second statistics (EWMAs, seeded from the catalog's `latency_hint` until there are 20 samples), with their sample counts. It holds no secrets.

```sh
curl -s localhost:9090/admin/v1/providers | jq '.providers[].deployments[] | {id, breaker: .breaker.state, cooldown_until}'
```

All of this state is per replica and kept in memory.

## Usage ledger and reporting

Every request gets one usage event when it completes, whatever the outcome: successes, upstream failures, budget rejections, client cancellations, and requests that authenticated but failed validation. (A request rejected by auth has no known tenant and is not recorded.) The event holds the tenant, key, endpoint, requested model, the deployment and provider that served it, status, attempts, fallback reason, cache status, token counts, cost, the prices it was computed from, the baseline cost, latency, time to first token, attribution tags and the config version. It never holds prompts or completions.

**Cost is exact.** A request costs, for each token class, tokens × the deployment's price for that class / 1,000,000. Prices are taken as the exact decimal text from the catalog and computed with arbitrary-precision integers in femto-USD (1e-15 USD), so no floating-point rounding is involved anywhere between the catalog and the ledger. The classes are:

| Class | From the OpenAI-wire usage | Price when the class has none |
|---|---|---|
| input (uncached) | `prompt_tokens − cached_tokens − cache_write_tokens`, at least 0 | none: the cost is unknown |
| cached input | `prompt_tokens_details.cached_tokens` | the input price |
| cache write | `prompt_tokens_details.cache_write_tokens` | the input price |
| output | `completion_tokens`, which includes reasoning tokens | none: the cost is unknown (embeddings need no output price) |

- **Unknown prices.** A deployment without an input price (or, for chat, an output price) has an unknown cost. Such requests are recorded with cost 0, counted in sums as 0, and reported separately as `unpriced_requests`.
- **Missing usage.** When the upstream reports no usage, for example a stream that broke off after it started, the input is the gateway's own estimate (bytes/4 of the request, scaled by the model's `token_ratio`) and the output is estimated at one token per 4 bytes of the output text relayed (content, refusals, reasoning and tool-call arguments), and the event is marked `usage_estimated`. A stream that broke off after reporting part of its usage (Anthropic reports the input when the stream starts and the output only at its end) keeps the input it reported and counts the larger of the reported output and the relayed output, also marked `usage_estimated`.
- **Requests no upstream served.** A request rejected before any upstream had it (routing errors, budget rejections, a parameter the deployment cannot take, a connection that failed before the request was sent) and a request an upstream refused with a 4xx cost 0. A request that reached an upstream but was never served, because the client went away, the deadline expired, the connection broke or the upstream answered 5xx, may still have been billed: it is recorded with the deployment and provider of the last such attempt, its estimated tokens and cost (see [Budgets](#budgets)), `usage_estimated`, and an unknown cost, so it counts in `unpriced_requests`.
- **Prices are snapshotted** into each event, so a later catalog edit never rewrites history.
- **Savings.** The baseline is the requested model for a concrete model name, or the policy's `baseline_model` for `auto`, priced at its cheapest priced deployment for the same token counts. Routing savings are `baseline − cost`: from provider arbitrage, and from `auto` or `equivalent` routing to a cheaper model. They are negative when a fallback went to something pricier. In observe mode, the event also records the deployment routing would have chosen (`shadow_deployment`) and the same tokens priced at it. A cache hit (`cache_status` `exact` or `semantic`) costs 0; its baseline cost, the cached answer's tokens priced at the baseline, is its cache savings, and it adds nothing to routing savings. Overhead is the gateway's own spend: the semantic cache's embedding calls, on the event of the request that made them, and shadow sampling (see [Caching](#caching)).

**Storage.** `storage.ledger.driver` selects where events go:

```yaml
storage:
  ledger:
    driver: postgres        # memory (default) | postgres
    dsn: ${DATABASE_URL}
    batch_size: 500         # events per write
    flush_interval: 1s      # the longest an event waits
    queue_size: 100000      # events buffered in memory
```

- **`memory`** keeps the most recent 100,000 events in process memory, per replica. It suits the quickstart and tests; nothing survives a restart.
- **`postgres`** must be reachable at startup: the gateway exits with an error naming `storage.ledger` (never the DSN) if it cannot connect or migrate. It applies its embedded migrations at startup (recorded in `schema_migrations`, in one transaction, safe with several replicas starting at once), into the first schema of the connection's `search_path`. Events go to `usage_events`, which is append-only, partitioned by month on `ts` (`usage_events_p202611`, …) and indexed on `(tenant, ts)`. Partitions are created at startup for the current and the next month, and before writing any other month. Money is stored as `NUMERIC(38,15)`, tags as `jsonb`. To drop old data, drop old partitions.

**Requests never wait for the database.** Events go into a bounded in-memory queue, and a background writer copies them to Postgres with `COPY`, in batches of `batch_size` or every `flush_interval`. If Postgres is down or failing, the writer keeps the events and retries with exponential backoff (100 ms doubling to 10 s); requests are unaffected. A retried batch is never stored twice. If the queue fills up, the oldest events are dropped and counted. On shutdown, once the server has drained, the writer flushes what is queued, within the shutdown's last 15 s (it shares 11 s with the shadow samples and cache writes, which go first, and comes before the span flush and the log drain, so a dead collector cannot delay it); events it cannot write by then are counted as dropped and the exit status is 1. The process exits within `server.shutdown_delay + server.shutdown_grace + 15s` of SIGTERM; size a container's stop grace period (compose's `stop_grace_period`, Kubernetes' `terminationGracePeriodSeconds`) to that. Watch these metrics:

| Metric | Meaning |
|---|---|
| `switchyard_usage_queue_depth` | Events waiting to be written, including the batch being written |
| `switchyard_usage_events_written_total` | Events written |
| `switchyard_usage_write_errors_total` | Failed batch writes; each is retried |
| `switchyard_usage_events_dropped_total` | Events lost: dropped from a full queue, rejected by the database, or left at shutdown. Alert on any increase. |

Log lines about the ledger carry event IDs and counts, never event contents.

**Attribution tags.** A client sends `X-Switchyard-Tags: k=v,k2=v2` to tag a request's usage event, for example by team or feature, and reports group by `tag:<key>`. At most 10 tags; keys are 1 to 32 characters of `a-z`, `0-9`, `_`, `.` and `-`, starting with a letter or digit, each at most once; values are 1 to 64 printable ASCII characters other than `,` and `=`; spaces around keys and values are ignored, and several header lines are joined. A malformed header gets 400 `invalid_tags`, without echoing it.

**Metrics.** On the admin `/metrics`: `switchyard_cost_usd_total{provider,model}` (a float approximation of the ledger's exact sums; over a run they agree to within 1e-6), `switchyard_tokens_total{provider,model,class}` with class `input` (uncached), `cached_input`, `cache_write` and `output`, `switchyard_savings_usd_total{source="routing"}`, the running total of `baseline − cost`, which decreases when a fallback costs more than the baseline, `switchyard_savings_usd_total{source="cache"}`, the running total of cache hits' baseline cost, and `switchyard_routing_projected_savings_usd_total`, observe mode's running total of projected savings. Both savings metrics are gauges, not counters, because a request's savings can be negative.

**Reports.** A report aggregates the events of one tenant over a time range `[from, to)`, in UTC, optionally grouped by one or more of `day`, `model` (the requested model), `deployment`, `provider`, `key`, `endpoint`, `status_class` (`2xx`, `4xx`, …), `cache_status`, `profile` (the routing profile; empty for embeddings), `routing_mode` (`enforce` or `observe`), `category` (the quality category; see [Shadow sampling and feedback](#shadow-sampling-and-feedback)) and `tag:<name>`. It returns totals for the whole range and, per group, the same fields:

- `requests`, `errors` (status 400 and above) and `unpriced_requests`;
- token counts per class;
- `cost_usd` (the known cost plus overhead), `baseline_cost_usd`, `overhead_usd` (the gateway's own spend, shadow sampling's included; see [Shadow sampling and feedback](#shadow-sampling-and-feedback)), and `savings_usd` with `routing`, `cache` and `net` (`net = baseline_cost_usd − cost_usd`); money is an exact decimal string;
- `projected_savings_usd`: what observe mode estimates routing would have saved, `baseline − shadow` over the observe-mode events whose cost and shadow cost are known. It is **projected, not realized**: the shadow deployment never served those requests and its quality is unverified, so it is never part of `savings_usd`;
- `cache_hit_rate` and `fallback_rate`.

Groups are sorted by cost, highest first, then by their values, and capped at `limit` (default 1,000, at most 10,000); the totals always cover every matching event. On Postgres the sums are computed in SQL over `NUMERIC`, so they are exact. The tenant filter is applied in the store itself: a tenant's report can never include another tenant's events, and only the admin API can report across tenants.

Two endpoints serve reports:

- **`GET /switchyard/v1/usage`** on the public listener, with a gateway key: the report of the key's own tenant. Any `tenant` parameter is ignored.
- **`GET /admin/v1/usage`** on the admin listener: the report of the tenant named by `tenant=`, or of every tenant without it.

Both take `from` and `to`, as RFC 3339 times or `YYYY-MM-DD` dates in UTC; a date as `to` includes that whole day. The default is the 7 days before now, and a report may span at most 400 days. `group_by` is a comma-separated list of the dimensions above, at most 8 of them and at most 3 tags, and `limit` caps the groups. A malformed parameter, or a report the database finds too complex, gets 400 `invalid_query`, and a ledger that cannot answer (Postgres down) 503 `usage_unavailable`. The Postgres ledger runs reports on a small connection pool of their own, so that they never hold up the writer, one at a time per tenant (the admin report across every tenant counts as one tenant) and at most 4 at once, each statement limited to 25 s; a report beyond those limits gets 429 `report_busy` with `Retry-After`.

```sh
curl "localhost:8080/switchyard/v1/usage?from=2026-11-01&to=2026-11-11&group_by=model" \
  -H "Authorization: Bearer $SWITCHYARD_API_KEY"
```

```json
{
  "object": "usage_report",
  "period": { "from": "2026-11-01T00:00:00Z", "to": "2026-11-12T00:00:00Z" },
  "tenant": "acme",
  "group_by": ["model"],
  "totals": {
    "requests": 3, "errors": 1, "unpriced_requests": 0,
    "tokens": { "input": 2595, "cached_input": 2351, "cache_write": 2251, "output": 553 },
    "cost_usd": "0.02230155", "baseline_cost_usd": "0.02230155", "overhead_usd": "0",
    "savings_usd": { "routing": "0", "cache": "0", "net": "0" },
    "projected_savings_usd": "0",
    "cache_hit_rate": 0, "fallback_rate": 0
  },
  "groups": [
    { "model": "claude-sonnet", "requests": 1, "errors": 0, "unpriced_requests": 0,
      "tokens": { "input": 2095, "cached_input": 2051, "cache_write": 2051, "output": 503 },
      "cost_usd": "0.02213655", "baseline_cost_usd": "0.02213655", "overhead_usd": "0",
      "savings_usd": { "routing": "0", "cache": "0", "net": "0" },
      "projected_savings_usd": "0",
      "cache_hit_rate": 0, "fallback_rate": 0 },
    { "model": "gpt-6-luna", "requests": 2, "errors": 1, "unpriced_requests": 0,
      "tokens": { "input": 500, "cached_input": 300, "cache_write": 200, "output": 50 },
      "cost_usd": "0.000165", "baseline_cost_usd": "0.000165", "overhead_usd": "0",
      "savings_usd": { "routing": "0", "cache": "0", "net": "0" },
      "cache_hit_rate": 0, "fallback_rate": 0 }
  ]
}
```

Each group is one flat object: its dimension values (here `model`; a tag dimension appears as `"tag:team"`), then its totals. `tenant` is `null` in an admin report across every tenant:

```sh
curl "127.0.0.1:9090/admin/v1/usage?group_by=day,tag:team"
curl "127.0.0.1:9090/admin/v1/usage?tenant=acme&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z"
```

## Budgets

A tenant can cap its spend per calendar day or month, in UTC, either in total (`scope: tenant`) or for each of its keys separately (`scope: key`):

```yaml
tenants:
  - id: acme
    keys: [...]
    budgets:
      - scope: tenant           # tenant | key
        window: month           # day | month, calendar periods in UTC
        limit_usd: "2000"       # exact decimal, > 0 and at most 1000000000
        soft_at: 0.8            # optional, in (0, 1)
        on_exhausted: block     # block (default) | notify
      - scope: key
        window: day
        limit_usd: "50"
budgets:
  fail_mode: open               # open (default) | closed
```

Each tenant may have at most one budget per scope and window. A `key` budget on a tenant without keys applies to nothing, and `config validate` warns about it.

**How a request is charged.** Before the first upstream attempt, the gateway estimates the request's cost on the chosen deployment, pricing the output at `max_tokens` (or `max_completion_tokens`) when the request sets it, else at 500 tokens, for each completion the request generates (`n` for chat; `max(n, best_of)` for each prompt of a legacy completion), and admits that estimate against every budget that applies: the tenant's budgets and, for `key` budgets, those of the calling key. Admission is atomic across all of them. If any `block` budget lacks room (spent + estimate > limit), nothing is charged anywhere and the request gets `429` with type `insufficient_quota`, code `budget_exceeded`, and a `Retry-After` giving the seconds until that budget's window resets (the latest one, if several are full). Otherwise the estimate is added to every budget. A fallback to a pricier deployment admits the difference first, under the same rule. When the request ends, the charge is settled once: to its exact cost rounded up to micro-USD when that is known; to the admitted estimate when it is not (a deployment without prices, or a stream that broke off on a deployment without prices); to the admitted estimate too when an upstream had the request but never served it (the client went away, the deadline expired, the connection broke, or the upstream answered 5xx), since it may have billed it anyway; and to 0 when no upstream had the request (routing errors, budget rejections, a parameter the deployment cannot take, a connection that failed before the request was sent) or every upstream refused it with a 4xx. The actual cost may be lower or higher than the estimate.

**Deployments without prices can't be budget-controlled precisely.** Their estimate is 0, so a request under a `block` budget is admitted at 1 micro-USD instead, and settled at that minimum: a full budget refuses it, but its spend is not counted. `switchyard catalog lint` flags deployments without prices.

Because concurrent admissions are serialized, a budget can be overshot only by requests that were admitted and then cost more than estimated, never by a race. If a replica dies mid-request, the estimate stays counted: budgets err towards over-counting.

- **`notify`** budgets never refuse. Requests over the limit are served, charged, logged and counted.
- **`soft_at`** reports, once per budget and window across all replicas, the first admission that brings spend to `soft_at × limit` or above. Phase 5 adds downgrading to a cheaper profile at that point.
- **Money.** Counters hold whole micro-USD (1e-6 USD). Limits, soft thresholds and charges are rounded up, so rounding never lets spend through. The usage ledger keeps exact decimal costs.

**Where counters live.** With `storage.mode: memory` they are in process memory, per replica, and reset on restart. With `storage.mode: redis` they are in the state instance (`storage.redis.state_url`, which must not evict keys), shared by every replica, as `sy:b:v1:<scope>:<window start, yyyymmdd>`, for example `sy:b:v1:acme/month:20261101` or `sy:b:v1:acme/key:acme-backend/day:20261111`. Each counter expires two days after its window ends. Admission is a single Lua script over every budget of the request. The state instance must be a single Redis server or a failover pair, not a Redis Cluster.

**When Redis is down**, `budgets.fail_mode` decides. `open` (the default) serves the request without budget checks, logs a warning and counts it in `switchyard_budget_store_errors_total`; `closed` rejects it with `503` and code `budget_unavailable`. A fallback to a pricier deployment follows the same rule (with `closed`, that deployment is skipped). A budget call that fails, or gets no answer within 1 s, marks the store unavailable for 5 s: during that time requests skip it and get the `fail_mode` outcome at once (counted in `switchyard_budget_store_errors_total`, with the warning logged only for the failure that marked it), then one request tries Redis again. Redis may be down at startup: the gateway starts and warns. With `closed`, `/readyz` fails its `budget_store` check until Redis answers; with `open`, the replica stays ready, since it serves requests anyway, and `GET /admin/v1/providers` reports `"budget_store": {"status": "ok" | "unavailable", "fail_mode": "open"}`.

**Metrics.** `switchyard_budget_rejections_total` counts requests answered 429 `budget_exceeded`; `switchyard_budget_fallback_skips_total` pricier fallback deployments passed over because a blocking budget had no room for the difference; `switchyard_budget_store_errors_total` budget store calls that failed or were skipped while the store was marked unavailable; `switchyard_budget_notifications_total{scope,kind}` the first time each window reaches `soft_at` (`kind="soft_threshold"`) and each request admitted over a `notify` budget (`kind="exceeded"`); and `switchyard_budget_utilization_ratio{scope}` is each budget's spend in its current window as a fraction of its limit, read from the store at scrape time. `scope` is the budget's name, such as `acme/month` or `acme/key:acme-backend/day`, so its values are bounded by the config.

## Routing

Each chat and legacy completion request goes to the cheapest deployment that meets its requirements (DESIGN §9). Embeddings never change models.

**The model field.**

| `model` | What is routed |
|---|---|
| `auto` | Every chat model the policy allows, under the policy's `profile` (or `routing.default_profile`). Needs a policy with a `baseline_model`; otherwise `400 model_not_supported` ("this policy has no baseline_model; auto routing is disabled"). |
| `auto:<profile>` | The same, under that profile, which must exist and be in the policy's `allowed_profiles`; otherwise `400 model_not_supported` naming the allowed profiles (so is `auto:` with no profile). A matching governance rule's `profile` replaces it (see [Policies and rules](#policies-and-rules)). |
| An alias | Whatever it targets; an alias may target `auto` or `auto:<profile>`. |
| A model id, `substitution: pinned` (the default) | Only that model's deployments, the cheapest eligible first, as in Phase 4. |
| A model id, `substitution: equivalent` | Every allowed chat model whose estimated quality for this request is at least the requested model's minus `max_quality_drop` (default 0.05), and at least a rule's or the `X-Switchyard-Min-Quality` header's `min_quality` when one is set; the profile's own `min_quality` does not apply. |
| `model@provider` | Only that deployment, whatever the substitution mode. The policy's `deny`, `allow` and `require_tags` still apply. |

**How a request is routed.** The gateway assesses the request (its category and difficulty, with the client's `X-Switchyard-Task` and `X-Switchyard-Difficulty` hints: `low`, `medium`, `high` or a number in [0, 1]; see [Quality scoring](#quality-scoring)), resolves its effective policy (see [Policies and rules](#policies-and-rules)), switches an `auto` request to a budget's `soft_profile` if that budget's soft threshold has been crossed, and then the router:

1. takes the candidates for the model field, as in the table;
2. applies the hard filters, which are never relaxed: the policy's `deny` (which wins) and `allow` globs over deployment ids, `require_tags`, the capabilities and parameters the request needs, the context window (which must hold the input estimate plus `max_tokens` when the request sets it), chat models only, the cost cap (the lower of `max_cost_per_request_usd` and the remaining room of the caller's blocking budgets; a deployment without prices is excluded whenever a cap is in force), and health (an open breaker, a rate-limit cooldown, a provider at `max_concurrency`);
3. estimates each candidate's quality `q̂`, its cost (the input at bytes/4, without inline image bytes, scaled by the candidate model's own `token_ratio`, plus 1,600 tokens per image, plus the smaller of `max_tokens` and `routing.expected_output_tokens`, default 500; the context check uses the same input estimate) and its time to first token (measured, or the catalog's `latency_hint` until 20 samples, or 1000 ms);
4. applies the soft checks: `q̂` at least the profile's `min_quality` as rules and headers adjust it (in `equivalent` mode, the requested model's `q̂` minus `max_quality_drop`, or a rule's or header's `min_quality` if higher; pinned requests skip it), and the estimated TTFT within `max_ttft_ms` when the profile or header sets one. When nothing passes and `on_unmet` is `best_effort` (the default), the latency target is dropped first, then the quality bar, keeping the best quality available; with `reject`, the request gets `400 no_eligible_model`;
5. ranks what is left by `prefer` tier, then the profile's objective (cost or TTFT), then the other one, then config order;
6. plans up to `routing.fallback.max_deployments`, each fallback preferring a provider not yet in the plan.

The executor then runs the plan with retries and fallback (see [Reliability](#reliability)). Budget admission charges the estimate on the primary deployment, at its model's `token_ratio`, with output at `max_tokens`, as before; a fallback to another model admits the difference at that model's ratio. When the remaining budget rules out every candidate, the plan is made without it, so the request gets `429 budget_exceeded` rather than `400 no_eligible_model`. A decision is deterministic: the same request, config and health state give the same plan.

**Observe mode.** A policy (or rule) with `routing_mode: observe` makes every decision as above but serves exactly what was asked for: the requested model, or the `baseline_model` for `auto`, as a pinned plan (so health skips and provider diversity still apply). The response says `X-Switchyard-Route-Reason: observe` and names the model routing would have chosen in `X-Switchyard-Shadow-Model`. The usage event records that deployment, and reports show `projected_savings_usd` (the baseline cost minus the same tokens on the shadow deployment) apart from the realized `savings_usd`, which in observe mode come only from provider arbitrage. It is the zero-risk way to see what routing would do on real traffic.

**Savings** are measured against the baseline: the requested model for a concrete name, the policy's `baseline_model` for `auto`, with the same tokens at its cheapest priced deployment. That is an approximation, since another model would tokenize and answer differently. See [Usage ledger and reporting](#usage-ledger-and-reporting).

**Explain.** `POST /switchyard/v1/route/explain` on the public listener, with a gateway key, takes a chat completions body and the same headers, and returns the routing decision without calling any upstream, charging any budget or recording any usage; requests chat would reject get the same error. It uses the replica's live health and budget state. In observe mode it shows the decision routing would make. `switchyard route explain` prints the same explanation offline, from the config, with every deployment healthy and every budget untouched (its whole limit left, no soft profile in force), as a freshly started gateway would; `--header 'Name: value'` (repeatable) passes request headers. It needs none of the config's secrets: references in API keys, Redis URLs and the ledger DSN resolve to their default or a placeholder, without reading the environment or the file. This is the request of DESIGN §10.5 with the `code-needs-quality` quality rule matching:

```sh
echo '{"model":"auto","messages":[{"role":"user","content":"Fix the failing test: ..."}]}' |
  bin/switchyard route explain -c switchyard.yaml --tenant acme --key acme-backend -
```

```json
{
  "policy": "default",
  "matched_rule": "",
  "matched_quality_rule": "code-needs-quality",
  "profile": "balanced",
  "mode": "auto",
  "assessment": { "category": "coding", "difficulty": 0.55, "signals": ["code", "length"], "hinted": false },
  "estimates": { "input_tokens": 1500, "output_tokens": 500 },
  "quality_bar": 0.85,
  "candidates": [
    { "deployment": "small@together", "model": "small", "q": 0.78, "est_cost_usd": "0.00035", "est_ttft_ms": 400,
      "tier": 0, "eligible": false, "excluded_by": "quality 0.78 < 0.85" },
    { "deployment": "medium@openai", "model": "medium", "q": 0.8625, "est_cost_usd": "0.00175", "est_ttft_ms": 500,
      "tier": 0, "eligible": true, "rank": 1 },
    { "deployment": "large@anthropic", "model": "large", "q": 0.9725, "est_cost_usd": "0.012", "est_ttft_ms": 900,
      "tier": 0, "eligible": true, "rank": 2 }
  ],
  "plan": ["medium@openai", "large@anthropic"],
  "relaxed": [],
  "dropped_params": [],
  "warnings": []
}
```

Every candidate is listed in catalog order with its estimates and either its rank or why it was excluded; money is a decimal string. `downgrade` appears when a budget's soft profile is in force ("budget acme/month soft threshold crossed; profile cheap"), and `notes` flag estimates that used a fallback (no quality prior, so q = 0.5; no latency estimate; unknown pricing).

**Demo.** `make demo-routing` runs the P5 mixed-workload demo against fake upstreams: three tiers modeled on DESIGN §10.5 (`demo-s` at $0.10 / $0.40 per million input / output tokens, `demo-m` at $0.50 / $2.00, `demo-l` on the Anthropic wire at $3 / $15), a policy routing `auto:balanced` with `demo-l` as the baseline, and 90 prompts from the quality sanity set, sent without hints:

```
    band  prompts  demo-s  demo-m  demo-l
    easy       30      30       0       0
  medium       30      17      12       1
    hard       30       0       9      21

Usage API (GET /switchyard/v1/usage), 90 requests:
  cost             0.0157383 USD
  baseline         0.037818 USD (every request on demo-l, the same tokens)
  routing savings  0.0220797 USD (58.4% of the baseline)
```

**Metrics.** `switchyard_route_decisions_total{profile,served_model,reason}` counts served routed requests; `switchyard_route_relaxations_total{constraint}` decisions that relaxed `latency` or `quality`; `switchyard_route_profile_downgrades_total{scope}` `auto` requests switched to a budget's soft profile; and `switchyard_routing_projected_savings_usd_total` observe mode's projected savings.

## Policies and rules

A policy is the governance attached to a tenant or a key (DESIGN §13): which deployments may serve its requests, which routing profiles its callers may pick, a per-request cost cap, and ordered rules for specific traffic.

```yaml
routing:
  default_profile: balanced            # used when a policy names no profile
  expected_output_tokens: 500          # output estimate when a request sets no max_tokens
  profiles:                            # omit to get the built-ins below; configured profiles replace them all
    cheap:    { min_quality: 0.65, objective: cost }
    balanced: { min_quality: 0.80, objective: cost }
    quality:  { min_quality: 0.92, objective: cost }
    fast:     { min_quality: 0.80, objective: latency, max_ttft_ms: 800 }
  assessor:
    type: heuristic                    # the only type
    weights: { length: 0.3 }           # optional signal weights in [-1, 1]

aliases:
  code-review: auto:quality            # an alias may target auto or auto:<profile>

policies:
  - id: default                        # used by every tenant that names no policy
    profile: balanced                  # default profile for plain "auto"; default routing.default_profile
    allowed_profiles: [cheap, balanced, fast]   # profiles callers may select; default all
    allow: ["*"]                       # globs over deployment ids (model@provider); default all
    deny: ["*@experimental"]           # deny wins over allow
    prefer: ["*@gpu-cluster", "llama-*"]        # earlier globs form higher preference tiers
    require_tags: [zdr]                # provider tags plus deployment tags
    substitution: { mode: pinned }     # pinned (default) | equivalent, with max_quality_drop (default 0.05)
    routing_mode: enforce              # enforce (default) | observe
    baseline_model: claude-sonnet-5-5  # savings baseline; required for auto routing
    max_cost_per_request_usd: "0.50"   # hard cap on a request's estimated cost; default none
    on_unmet: best_effort              # best_effort (default) | reject
    rules:                             # governance rules: first match wins; quality rules: first match, on top
      - name: staging-is-cheap
        match: { header: { x-env: staging } }
        set:   { profile: cheap }
      - name: code-needs-quality
        match: { task: coding, difficulty_gte: 0.5 }
        set:   { min_quality: 0.85 }

tenants:
  - id: acme
    policy: default                    # default: the policy with id "default", else built-in defaults
    keys:
      - { id: acme-batch, hash: "sha256:…", policy: batch }   # a key's policy replaces the tenant's; batch is another policy
    budgets:
      - { scope: tenant, window: month, limit_usd: 2000, soft_at: 0.8, soft_profile: cheap }
```

**Which policy applies.** The key's `policy`, else the tenant's, else the policy with id `default`, else the built-in policy: the default profile, everything allowed, `pinned` substitution, `enforce`, `best_effort`, no cost cap and no `baseline_model`. Without a `baseline_model`, `auto` requests are answered `400 model_not_supported` ("this policy has no baseline_model; auto routing is disabled"), and `config validate` warns about a tenant whose policy allows every profile but has no baseline.

**Profiles.** A request selects a profile with `model: "auto:<profile>"` or the `X-Switchyard-Profile` header, and plain `auto` uses the policy's `profile` or `routing.default_profile`. A profile in the model field that is unknown or not in `allowed_profiles` is answered `400 model_not_supported`, naming the allowed profiles. The profile in force is the matching governance rule's, which replaces the one the request selects (rules are trusted config, and the response says so in `X-Switchyard-Policy-Warning`); else the one the request selects; else the policy's; else `default_profile`. Its `min_quality` and `max_ttft_ms` are the quality bar and the latency target; `objective` ranks the eligible deployments by cost or by estimated time to first token.

**Rules.** Every condition of a rule's `match` that is set must hold, and an empty `match` always holds. Rules form two layers, so that what a request says about itself can never steer it past a rule that governs it:

- **Governance rules** match only on `model`, `profile`, `key`, `key_tags`, `header`, `tags`, `stream`, `has_tools` and `has_images`, and may set any field. They are evaluated in order and the first that matches applies; an empty `match` therefore shadows every governance rule after it (`config validate` warns).
- **Quality rules** are the rules whose `match` uses `task`, `difficulty_gte`, `difficulty_lt` or `est_input_tokens_gte`, which the request's content and the client's `X-Switchyard-Task` and `X-Switchyard-Difficulty` hints decide (they may use governance conditions too). They may set only `min_quality` and `prefer`; `config validate` rejects any other field. The first that matches applies on top of the governance rule's result, whatever the order of the two.

The explanation names both, as `matched_rule` and `matched_quality_rule`, and so does the access log. Clients set request headers and `X-Switchyard-Tags` themselves, so a rule that matches them should only restrict, never loosen, and should come before the governance rules it must not shadow: a client can always send a header, or leave it out. Conditions:

| Condition | Holds when |
|---|---|
| `model` | the model field as sent matches the glob (`path.Match` syntax: `*`, `?`, `[...]`; `*` does not cross `/`) |
| `profile` | the request selected this profile |
| `task` | the assessed or hinted category is this one (`general`, `coding`, `reasoning`, `extraction`, `creative`, `tool_use`) |
| `difficulty_gte`, `difficulty_lt` | the difficulty is at least, or below, the bound (0 means unset) |
| `key`, `key_tags` | the calling key has this id, or carries every one of these tags |
| `header` | for each header name (case-insensitive), some value matches the glob (case-sensitive) |
| `tags` | for each `X-Switchyard-Tags` key, its value matches the glob |
| `stream`, `has_tools`, `has_images` | the request streams, has tools, or has images (`true` or `false`) |
| `est_input_tokens_gte` | the estimated input is at least this many tokens |

A governance rule's `set` may override `profile`, `min_quality`, `prefer`, `allow`, `deny`, `require_tags`, `substitution`, `routing_mode`, `max_cost_per_request_usd` and `on_unmet`; a quality rule's only `min_quality` and `prefer`, which replace the governance rule's. A field left out (or 0) keeps the policy's value, and a list set to `[]` clears it. Rules are operator config, so they may loosen the policy as well as tighten it.

**Headers can only tighten.** These request headers narrow what the policy allows, and never widen it:

| Header | Effect |
|---|---|
| `X-Switchyard-Profile` | selects one of the allowed profiles |
| `X-Switchyard-Max-Cost-Usd` | lowers the cost cap, e.g. `0.05` |
| `X-Switchyard-Min-Quality` | raises the quality bar, a decimal in [0, 1], up to the highest `min_quality` among the profiles the caller may select |
| `X-Switchyard-Max-TTFT-Ms` | lowers the latency target, in whole milliseconds |

A value that would loosen the policy (a higher cap, a lower bar, a longer target, or a profile the policy doesn't allow) is ignored, and the response says so in `X-Switchyard-Policy-Warning`. So is a `X-Switchyard-Min-Quality` above every selectable profile's `min_quality`, which would otherwise reach a stronger tier than `allowed_profiles` permits. A malformed value, or a header given twice, is answered `400 invalid_header`; the message names the header but never echoes its value. Costs are compared exactly, to the nano-USD a cap may be written in (at most 9 decimal places).

**Budget soft profile.** A budget with `soft_at` and `soft_profile` switches `auto` requests to `soft_profile` once its soft threshold has been crossed in the current window, whatever profile the request or a rule selects; rule `min_quality` and header tightening still apply on top of it, with `X-Switchyard-Min-Quality` raising the bar no higher than the soft profile's own, and the explanation says so. `soft_profile` requires `soft_at` and must name a profile.

**Validation.** `config validate` checks profile names and ranges (`min_quality` in [0, 1], `objective` `cost` or `latency`, `max_ttft_ms` ≥ 0), that `default_profile`, every policy and rule profile, every `allowed_profiles` entry, alias targets `auto:<profile>` and budget `soft_profile`s exist, that policy ids are unique and every tenant and key `policy` exists, that globs are valid, that `baseline_model` is a chat model id (not an alias or `model@provider`), that `max_cost_per_request_usd` is greater than 0, that rule names are unique within their policy, that quality rules set only `min_quality` and `prefer`, that `task` is a known category and difficulty bounds lie in [0, 1] with `difficulty_gte` < `difficulty_lt`, that header names are valid, and that assessor weights name known signals with values in [-1, 1].

## Quality scoring

The router sends a request to a cheaper model only when that model is likely to be good enough for it (DESIGN §10). Two things decide that: the model's quality prior for the request's category, from the catalog's `quality` map, and the request's difficulty `d` in [0, 1], from the assessor. The estimate for a deployment is

```
q̂ = clamp(1 − d · (1 − Q[model][category]) + quality_offset, 0, 1)
```

where `Q[model][category]` falls back to the model's `general` prior. An easy request (`d` near 0) scores close to 1 on every model, so the cheapest wins; a hard one (`d` near 1) scores close to the prior, so only strong models clear the profile's `min_quality`.

**The heuristic assessor** rates a request without any network call, in about 15 µs for a 4 KB prompt. It reads only the last user message, the system (and developer) messages, `tools`, `tool_choice` and `response_format`, and at most the first 32 KiB of message text. It keeps nothing after the request is assessed, and its output names the signals that fired, never any content. Each signal yields a feature `f` in [0, 1], and

```
d = clamp(bias + Σ weight[signal] · f[signal], 0, 1)
```

| Signal | Looks for | Default weight | Category |
|---|---|---|---|
| `length` | estimated input tokens, log-scaled from 100 to 32k | 0.25 | |
| `code` | fenced code (more lines count more), stack traces, code punctuation, programming terms | 0.25 | `coding` |
| `math` | "prove", "derive", "show that", LaTeX, equations, mathematical terms | 0.55 | `reasoning` |
| `reasoning_cues` | "step by step", "trade-offs", "design", "analyze", "explain why" | 0.30 | |
| `constraints` | bullet and numbered lines, "must", "never", "at least" | 0.25 | |
| `conversation_depth` | turns before the last user message | 0.15 | |
| `tools` | tool definitions (more count more), a forced tool call | 0.20 | `tool_use` |
| `strict_schema` | `response_format` `json_schema` (larger schemas count more) or `json_object` | 0.10 | `extraction` |
| `simple_task` | a request that starts with "translate", "rewrite", "classify", "extract" and similar, asks for "one word" or "yes or no", or is short | −0.35 | `extraction` for classify/extract |
| `creative` | "poem", "story", "toast", "sonnet" and similar | 0.10 | `creative` |
| `bias` | the constant term | 0.40 | |

The category is the strongest category signal, or `general` when none is strong enough. Override any weight, each in [-1, 1], under `routing.assessor.weights`; the names are those in the table.

**Hints.** A caller that knows better can say so: `X-Switchyard-Task` sets the category (one of the categories above) and `X-Switchyard-Difficulty` sets `d` (`low` = 0.2, `medium` = 0.5, `high` = 0.85, or a number in [0, 1]). Either one marks the assessment as hinted in the route explanation. Any other value, or a header given twice, is answered `400 invalid_header`.

**How well it works.** `internal/quality/testdata/sanity.json` holds 105 realistic prompts across every category, each labeled easy (`d` < 0.35), medium or hard (`d` ≥ 0.65) by reading the prompt, not by running the heuristic. With the default weights, 98 of them (93 %) land in their labeled band, and `go test -v -run TestSanitySet ./internal/quality/` lists the ones that don't. The heuristic is English-centric and can be fooled by phrasing, which is why prompt content only ever affects the quality estimate, never authorization, allow lists, budgets or cost caps; `observe` routing mode and the explain endpoint let you check its decisions before you rely on them.

## Evaluation and verification

The quality feedback loop (DESIGN §10.7, PLAN P8) in five steps: measure the catalog's quality priors on your prompts, route with the measurements, then check on live traffic that the savings hold up. `make demo-eval` runs steps 1 to 3 end to end against fake upstreams. The details are in [Quality evaluation](#quality-evaluation) and [Shadow sampling and feedback](#shadow-sampling-and-feedback).

**What leaves the gateway.** Evaluation and shadow sampling send prompts to the candidate, reference and comparison deployments, and **the prompt and both answers to the judge deployment**. Choose deployments you may send that data to; shadow sampling enforces the policy's `allow`, `deny` and `require_tags` on both. Neither ever logs or stores prompt or answer content: results files and `shadow_evals` rows hold ids, models, categories, difficulties, verdicts, costs and error kinds only. Feedback comments are stored with the tenant's feedback and never logged.

1. **Run an evaluation, after a dry run.** Every command that can spend money needs `--max-cost-usd`, a cap on that invocation's spend (see [Cost](#quality-evaluation) for exactly what it guarantees); `--dry-run` prints the planned calls and their estimated cost first. Rerunning with the same `--out` resumes, with a fresh cap for the new spend only.

   ```sh
   switchyard eval run -c switchyard.yaml --prompts test/eval/prompts/starter.jsonl \
     --candidates claude-haiku-5-5,gpt-6.1-sol --reference claude-sonnet-5-5 --judge claude-fable-5-2 \
     --max-cost-usd 5 --dry-run
   switchyard eval run ... --max-cost-usd 5      # the same flags, without --dry-run
   ```

   `make eval-live EVAL_CONFIG=… EVAL_CANDIDATES=… EVAL_REFERENCE=… MAX_COST_USD=… [JUDGE=…]` runs the same against your real config (`DRY_RUN=1` to estimate, `DRY_RUN=0` or unset for a real run); it skips unless every variable is set. It needs the real provider keys in the environment variables the config references; without them it fails at config load, before any call, and spends nothing.

2. **Fit.** `eval fit` prints the report (Q, its 90% interval and the change from the current prior, per model and category) and writes the estimates with enough samples:

   ```sh
   switchyard eval fit --results results.jsonl -c switchyard.yaml --out configs/catalog/quality-overrides.yaml
   ```

3. **Include the overrides.** The example config's `catalog/*.yaml` glob already includes `configs/catalog/quality-overrides.yaml` (shipped empty, with a commented example); elsewhere, add `include: [quality-overrides.yaml]`. Check with `switchyard config validate` and see the effect with `switchyard route explain` before you reload.

4. **Verify on live traffic in observe mode.** Give the onboarding policy `routing_mode: observe` and `shadow: {sample_rate: 0.05}`, and set `evaluation.judge.model` and `evaluation.shadow.daily_budget_usd`. The gateway keeps serving the baseline, and for 5% of requests it also asks the model routing would have chosen, in the background, and has the judge compare the answers. Each tenant's shadow spend stays within the daily budget, and it counts as overhead.

5. **Read the verified savings.** `GET /switchyard/v1/usage?group_by=category` (or `/admin/v1/usage`) reports per category the judged samples, the pass rate and its 90% Wilson lower bound, and `verified` once a category has `verified_min_samples` samples and a lower bound of at least `verified_min_pass_rate`. `verified_savings_usd` is the routing plus projected savings of the verified categories, beside `projected_savings_usd`, which it never replaces. Switch a policy to `enforce` once the categories you care about are verified, and keep a small `sample_rate` there to keep checking. Clients can add their own judgment with `POST /switchyard/v1/feedback`.

## Quality evaluation

`switchyard eval` replaces the catalog's hand-set quality priors with measurements on your own prompts (DESIGN §10.7). It runs a prompt set against candidate models and a reference model, has an LLM judge compare each candidate answer with the reference answer, fits each model's prior `Q` per category with a confidence interval, and writes the estimates as `quality_overrides` that the config includes.

**Privacy.** `eval run` sends every prompt to the reference and to each candidate, and **the prompt and both answers to the judge deployment**. Pick deployments you may send that data to. The results file holds no prompt or answer content: only ids, models, categories, difficulties, outcomes, costs and error kinds. Nothing is logged.

**Cost.** `eval run` calls paid models, so `--max-cost-usd` is required, and `--dry-run` prints the planned calls and their estimated cost without calling anything:

```sh
switchyard eval run -c switchyard.yaml --prompts test/eval/prompts/starter.jsonl \
  --candidates claude-haiku-5-5,qwen3.5-9b --reference claude-sonnet-5-5 --judge claude-opus-5-5 \
  --max-cost-usd 5 --dry-run
```

Before any call, its worst-case cost is admitted against the cap. Each call's estimate is its input at the same bound the gateway and shadow sampling use (one token per byte of the request, and 1,600 tokens per image in place of its bytes) plus its output at its token cap: an answer's whole output budget (`--max-output-tokens`, default 1024; prompts' own `max_tokens` are lowered to it), a judge call's 256 tokens. Each candidate answer is admitted together with its 4 judge calls (two runs, each retried once) as one unit, and each prompt's reference answer with the prompt's first pair, so a pair is all or nothing and a cap stop never leaves a paid answer unwritten. What has been spent plus the estimates of the calls in flight never exceeds the cap. Each call is then charged at its actual cost, from the reported usage and the catalog's exact prices; a call that fails without usage after it may have been billed (a timeout, a transport error, a 5xx) is charged its estimate, and only a refused call (a 4xx) nothing.

What the cap guarantees: the run's spend stays within `--max-cost-usd` whenever each call costs no more than its estimate. The estimate can be exceeded only by provider-side tokens it cannot see, such as a hidden system prompt a provider adds for tool use, or an image a provider bills above 1,600 tokens. The cap covers one invocation: a resumed run prints what the results file already records and gets a fresh cap for its new spend, so rerun with what you are willing to spend on the remaining pairs. When the next pair does not fit, the run stops cleanly with exit code 3, keeping what it finished, and prints this run's spend and the file's total. Every deployment involved needs known prices. There are no provider keys in the test suite: the tests run against fake upstreams, and a live run is simply `eval run` with a real config.

**The run.** For each prompt, the reference answers once, then each candidate answers through its provider's adapter, pinned to one deployment for the whole run (a `model@provider`, or a model's cheapest priced deployment), and the judge compares the two. Answers are never streamed. `--concurrency` (default 4) prompts run at once. Each (prompt, candidate) pair appends one line to `--out` (default `results.jsonl`). Rerunning with the same `--out` resumes: pairs with a verdict are skipped, and pairs that failed (an answer error, a judge failure, a deployment that lacks a capability the prompt needs) are retried. Judge failures are counted and reported, never scored. If a run was killed mid-write and left a torn last line, the next `eval run`, `fit` or `report` warns (naming the file and the size, never the content) and ignores it, and `eval run` cuts it off before appending; a line that does not parse anywhere else is still an error.

**Prompt sets** are JSONL, one `{"id": …, "request": {chat request}, "category"?: …}` per line. `category` is optional, and the assessor's category is used without it; the difficulty `d` always comes from the assessor. `test/eval/prompts/starter.jsonl` is a starter set of 105 prompts across every category, built from the labeled sanity set.

**The judge** comes from `--judge`, else `evaluation.judge.model`. Use a strong model, at least as capable as the reference, and not one of the candidates: judges tend to prefer their own answers, and `eval run` warns when a candidate is the judge. Each pair is judged twice, the candidate first as answer A and then as answer B, to cancel position bias, against a rubric for the prompt's category, at temperature 0 when the deployment accepts it and with structured output when it supports it. The judge is not told which answer is the reference, and is told not to reward length. It replies `{"winner": "A"|"B"|"tie", "reason": …}` within 256 tokens (the judge's own fixed output cap, in `eval` and shadow sampling alike); a reply that cannot be parsed is retried once. The prompt's delimiters are escaped in the request and answers whatever their case or spacing, and the judge's last `{"winner": …}` object decides, since it gives its own verdict last and an earlier one may be quoted from an answer. Even so, the judge reads text that the request and answers control: a tenant whose prompts make both models append instructions to the judge can steer it and inflate its own verification. This is an accepted residual risk; the judge never changes routing by itself, and the eval prompts are the operator's own. Both runs tie: `equivalent`; the candidate wins at least one and loses neither: `candidate_better`; the reference wins at least one and loses neither: `reference_better`; the runs disagree: `inconsistent`. Only `equivalent` and `candidate_better` are acceptable.

**Fitting.** `eval fit` estimates `Q` for every (model, category) and writes the overrides; `eval report` prints the same table without writing:

```sh
switchyard eval fit --results results.jsonl -c switchyard.yaml --out quality-overrides.yaml
```

```
MODEL             CATEGORY  N   N_INFORMATIVE  Q      CI (90%)        PRIOR  CHANGE  NOTE
claude-haiku-5-5  coding    26  26             0.812  [0.701, 0.903]  0.780  +0.032
claude-haiku-5-5  tool_use  9   9              0.934  [0.811, 1.000]  0.700  +0.234  insufficient; not written (needs 10 informative)
```

Under the model of §10.4, a candidate answer is acceptable with probability `1 − d·(1 − Q)`; `Q` is the maximum-likelihood estimate over [0, 1] from the judged samples, with a 90% percentile-bootstrap interval of 1000 resamples, seeded so that the same results always give the same numbers. Samples with `d = 0` carry no information about `Q` and are left out (`N` counts them, `N_INFORMATIVE` does not). With fewer than 10 informative samples an estimate is marked insufficient, and an estimate is written only with at least `--min-samples` (default 10). When every informative sample is acceptable (or none is), every resample gives the same estimate and the interval collapses to `[1, 1]` (or `[0, 0]`): that is inherent to the percentile bootstrap, so read `N_INFORMATIVE` as the measure of confidence when the interval has no width. `PRIOR` is what routing uses today (with `-c`), and `CHANGE` the difference. Each override records its value, interval, sample count, date (`--date`, default today) and source (the prompt set, the judge and the reference).

**Using the overrides.** Include the file from the main config; included files may define `quality_overrides`:

```yaml
include: [quality-overrides.yaml]
```

Each entry replaces its model's catalog prior for its category, and `config validate` checks that the model exists, the category is known, `ci_low ≤ value ≤ ci_high` within [0, 1], `samples ≥ 0`, `evaluated` is a date, and no (model, category) pair appears twice. `switchyard route explain` shows the effect on a request before you deploy it.

**Fitting the assessor's weights.** `eval fit-weights` fits the heuristic's weights to a labeled set by least squares (targets 0.2, 0.5 and 0.85 for easy, medium and hard), with projected gradient descent keeping every weight in [-1, 1], and reports the in-band accuracy before and after:

```sh
switchyard eval fit-weights --labeled internal/quality/testdata/sanity.json --out weights.yaml
```

It writes a `routing.assessor.weights` section to merge into the main config (included files cannot set `routing`); the defaults in code never change. On the shipped sanity set, the hand-tuned defaults already place 94.3% of prompts in their band and the least-squares fit 93.3%, so the fit earns its keep on your own labeled prompts rather than on this one.

## Shadow sampling and feedback

Shadow sampling checks, on live traffic, that routing to cheaper models does not cost quality (DESIGN §10.7). For a sample of a policy's requests, after the client has its answer, Switchyard asks a **comparison** deployment for a second answer in the background and has an **LLM judge** compare the two:

- in `enforce` mode the served (routed) answer is the candidate, and the reference is the model the request's savings are measured against: the policy's `baseline_model` for `auto` and `auto:<profile>`, or the requested model when an equivalent substitute served it. A request pinned to a concrete model and served by it is not sampled: the client chose the model, so there are no routing savings to verify;
- in `observe` mode the deployment routing would have chosen gives the candidate and the served baseline answer is the reference.

The judge decides whether the candidate could replace the reference (`equivalent` or `candidate_better` are acceptable; `reference_better` and `inconsistent` are not).

**It is off unless a policy opts in**, and it needs a judge and a budget:

```yaml
evaluation:
  judge:
    model: gpt-5            # a chat model, or model@provider
  shadow:
    daily_budget_usd: 5     # per tenant per UTC day; required with sampling
    concurrency: 4          # evaluations in flight per replica (default 4)
    max_output_tokens: 1024 # cap on each comparison answer (default 1024); the judge's is a fixed 256
    verified_min_samples: 30     # default 30
    verified_min_pass_rate: 0.9  # default 0.9
policies:
  - id: default
    baseline_model: gpt-5   # required with sampling: the comparison in enforce mode
    shadow:
      sample_rate: 0.02     # 2% of requests, in [0, 1]
```

**Privacy.** Shadow sampling sends the request's prompt to the comparison deployment, and the prompt and both answers to the judge deployment. Both must pass the policy's `allow`, `deny` and `require_tags`. The judge is pinned to one deployment (a `model@provider`, or the model's cheapest priced deployment), and `config validate` and `serve` reject the config unless that exact deployment passes every sampling policy's rules; name a permitted one as `evaluation.judge.model: model@provider`. The config is also rejected if a sampling policy permits no deployment of its baseline, and at run time a sample is dropped if the request's effective policy (after rules) does not permit the comparison or the judge. The comparison call is sent without the client's `store` (set to false), `metadata`, `user` and `service_tier`, since the client never asked for it; `eval` answers likewise. Prompts and answers are never logged and never stored; only verdicts and metadata are.

**Cost.** Each tenant's shadow spend is capped by its own daily budget, `shadow:<tenant>/day` in the budget store (Redis or in memory, like the other budgets). The budget is reserved one call at a time: before the comparison call, its worst case is admitted (one token per byte of the request, 1,600 per image, and its output cap), and the call is settled at its actual cost right after; then each judge call (up to 4: two runs, each retried once) is admitted at its own worst case, from its real prompt and the judge's 256-token output cap, and settled after it. Costs come from the reported usage and the catalog prices, and a call that may have been billed without reporting usage is charged its estimate. A sample whose comparison does not fit is dropped; one whose next judge call does not fit stops there, with its spend so far charged and stored as a row with error kind `judge_budget` and no verdict. Both count as `dropped_budget`. Since no call starts before its worst case is admitted, concurrent samples never take the budget past its limit, except by a single call's estimator error: the estimate ignores cache-write prices for requests with `cache_control`, images a provider bills above 1,600 tokens, and hidden provider tokens such as a tool-use system prompt. Requests are sampled deterministically by a hash of their request ID, so every replica agrees.

**Runtime.** Shadow calls run asynchronously after the response and never delay it: at most `concurrency` at once per replica, with a bounded queue (a full queue drops the sample, and so does one whose queued requests and served answers would exceed 64 MiB in all), each answer capped at `max_output_tokens`, each sample bounded by a timeout. Comparison calls are never streamed. A request is sampled only when it is served with status 200 and ends cleanly, by another model than the comparison, with a chat answer that no content filter stopped; a sampled stream collects its answer text (at most 64 KiB, or the sample is skipped) as it is relayed, and unsampled requests collect nothing. On shutdown, after the server has drained, the samples get 4 s to finish, then running ones are canceled and queued ones dropped, and their last settlements and rows get no more than that (a settlement that cannot be made keeps its estimate counted); the usage ledger flushes after them.

**Results.** Each evaluated sample stores one row (`shadow_evals` in Postgres, or in memory): request ID, time, tenant, key, category, difficulty, profile, mode, candidate and reference deployments, judge model, outcome, acceptable, cost, and an error kind when there was no verdict. Rows join to the usage ledger by request ID and tenant; usage events also record their quality `category`.

**Verified savings in reports.** Every set of totals in a usage report (see [Usage ledger and reporting](#usage-ledger-and-reporting)) adds:

| Field | Meaning |
|---|---|
| `quality` | Per category with shadow samples: `judged`, `acceptable`, `errors`, `pass_rate` (acceptable / judged), `wilson_lower` (the lower bound of its 90% Wilson score interval) and `verified` |
| `verified_savings_usd` | Routing savings plus projected savings of the requests in verified categories. It is reported beside `savings_usd` and `projected_savings_usd` and never replaces them |
| `shadow_cost_usd` | What these requests' shadow evaluations (comparison and judge calls) cost. Shadow spend is the gateway's own (DESIGN §12.4), so it is part of `overhead_usd`, and so of `cost_usd` and net savings; this field breaks it out. It is joined from the shadow evaluations when the report runs, since they finish after their request is recorded |
| `feedback` | Feedback on these requests: `negative`, `neutral`, `positive` |

A category is **verified** when it has at least `verified_min_samples` judged samples and `wilson_lower ≥ verified_min_pass_rate`; the report's `verification` object states the rule. Each set of totals decides from its own samples: grouped by `day`, a day is verified only if that day has enough samples. Add `group_by=category` to break everything down by quality category. The judge is itself a model and can be wrong, and the pass rate measures agreement with the baseline, not absolute quality.

**Feedback.** Clients can score their own requests:

```sh
curl localhost:8080/switchyard/v1/feedback -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -d '{"request_id":"019a7c1e-...","score":1,"comment":"spot on"}'
```

`request_id` is the response's `X-Request-Id`; `score` is -1, 0 or 1; `comment` is optional, at most 500 characters, stored and never logged. The answer is 202. A tenant can score only its own requests: an ID it never made gets 404 `request_not_found` where the ledger can tell (the in-memory ledger), and otherwise (Postgres, written asynchronously) the feedback is stored with the tenant and reports join it only to that tenant's requests, so a wrong ID reveals nothing. Later feedback on the same request replaces earlier. A malformed body gets 400 `invalid_feedback`, without echoing it. The usage event is recorded just after the response completes, so with the in-memory ledger, feedback sent the instant a response arrives (a stream's last byte, say) can get a 404 before the event is in; retry it after a moment.

**Metrics.** `switchyard_shadow_samples_total{outcome}` with outcome `judged`, `dropped_budget`, `dropped_queue`, `dropped_policy` or `error`; `switchyard_shadow_cost_usd_total`; and `switchyard_shadow_acceptable_ratio{category}`, the share of judged samples found acceptable since the replica started.

## Caching

Policies can answer repeated chat completions from a cache (DESIGN §11). Only `POST /v1/chat/completions` is cached; legacy completions and embeddings never are. Both layers are off unless a policy turns them on, and only a governance rule's `set.cache` can change a policy's cache block (it replaces the whole block):

```yaml
policies:
  - id: default
    cache:
      exact: { enabled: true, ttl: 24h, scope: tenant, deterministic_only: false }
      semantic: { enabled: false, min_similarity: 0.95, ttl: 72h, max_chars: 2000 }
cache: { lookup_timeout: 25ms, max_entry_bytes: 1048576, memory_max_entries: 10000 }
semantic_cache: { embedding_model: text-embedding-3-small, lookup_timeout: 150ms }
```

**The lookup** happens once the request is authenticated, decoded and its policy resolved, before budgets and routing: a hit is served even when the budget is exhausted or every deployment is unhealthy, costs nothing and makes no chat call upstream (a semantic lookup, hit or miss, does make its embedding call; see below). An unknown model (404) and a malformed request or header (400) are answered before any lookup. The exact cache is looked up first, then, on an exact miss, the semantic cache.

**A hit must be one the request's own policy could have produced.** Entries are shared by every key of the scope, whatever policy each key has, so before serving an entry the gateway checks it against the requesting key's effective policy (after rules): the deployment that produced it must still be in the catalog and pass the policy's `allow`, `deny` and `require_tags`, and a model routed pinned (`substitution: pinned`, a `model@provider`, or observe mode) is answered only by its own deployments; under `substitution: equivalent`, and for `auto`, any deployment the filters permit will do. Health and budgets are not checked. An entry that fails the check is a miss, counted `result="policy"`: the request is routed as usual, and its own answer is written. The same holds for a request coalesced with another (below).

**The exact key** is `sy:x:v1:<scope>:<namespace version>:<sha256>` over the request's canonical form: sorted-key JSON of every request field, unknown pass-through fields included, with `model` replaced by the resolved target. The target is the catalog model for a model name, alias or `model@provider`, and for `auto` `auto:<profile>@<policy>#q=<quality floor>,t=<max TTFT ms>,c=<max cost nano-USD>`, with every constraint of the effective policy that a rule or the `X-Switchyard-Min-Quality`, `-Max-TTFT-Ms` or `-Max-Cost-Usd` header can tighten, so a request that demands more never shares an entry with one that did not (an `auto` hit returns whatever model answered first under the same constraints). Routing headers do not change a concrete model's key, since the model is fixed; the policy check above applies instead. `messages`, `tools`, `tool_choice`, `response_format`, `temperature`, `top_p`, `max_tokens`, `stop`, `seed`, `n`, `logprobs`, the penalties and reasoning parameters all change the key; `stream`, `stream_options`, `user`, `metadata`, `store`, `service_tier` and Switchyard's other headers never do. A request with a field that is not valid UTF-8, or with a `\u` escape of a lone UTF-16 surrogate (which would decode to the same character as other strings that upstreams receive as different), is neither looked up nor written. Only complete answers are written: status 200, a `chat.completion` whose every choice finished with `stop`, `tool_calls` or `length`, at most `cache.max_entry_bytes` (default 1 MiB). Errors, content-filtered answers and broken or disconnected streams are never cached, and neither is an `auto` answer routed with a budget's `soft_profile`, since its key names the profile it was asked for (counted `not_cacheable`). An entry keeps the parameters its miss dropped (`routing.drop_unsupported_params`), and a hit reports them in `X-Switchyard-Dropped-Params` as the miss did.

**Scopes** say who shares entries, always within one tenant: tenants never share entries, under any scope.

| `exact.scope` | Entries are shared by |
|---|---|
| `tenant` (default) | every key of the tenant |
| `key` | one API key |
| `user` | one end user, by the request's `user` field; a request without one is scoped by its key |

The semantic cache uses the same scope. Under the `user` scope, the cache keys hold an unsalted SHA-256 of `user`, and key names are visible to anyone who can run `SCAN` or `MONITOR` on the cache Redis: a hash of an email address or another guessable ID is easily reversed, so send an opaque user ID (a random or keyed-hash identifier your application maps), not an email.

**`deterministic_only: true`** caches only requests with `temperature: 0` or a `seed`. Otherwise identical requests with temperature above 0 all get the same cached sample, which is right for most applications but not for "generate N variations" loops. A request that omits `temperature` is not deterministic, since providers default to 1. It gates both layers: such a request is neither looked up nor written. It judges the request as sent: a `seed` that `drop_unsupported_params` later removes for the serving deployment still makes the request deterministic, so with such deployments prefer `temperature: 0`.

**Headers.** A client narrows its policy's caching with the request header `X-Switchyard-Cache`: `off` (no lookup, no write), `no-store` (look up, but don't write), or `exact-only` (skip the semantic layer). Any other value is 400 `invalid_header`. Every chat response of a request its policy caches carries `X-Switchyard-Cache: miss`, `exact` or `semantic`; the header is omitted when caching is off for the request (off in the policy, the `off` header, or `deterministic_only`). A hit also has `X-Switchyard-Attempts: 0`, `X-Switchyard-Cost-Usd: 0` (non-streaming), the route reason `cache_exact` or `cache_semantic`, and the model and provider that produced the cached answer.

**Streaming.** A hit on a streaming request is replayed as a minimal valid stream: per choice a role chunk, a chunk with the content or tool calls, a finish chunk; then the usage chunk if the client asked for usage (`stream_options.include_usage`); then `data: [DONE]`. The SDKs' stream accumulators rebuild the same final message as the original. A streamed miss is aggregated while it streams and stored once it completed cleanly with `[DONE]`; aggregation stops for good, and nothing is stored (counted `too_large`), once the raw size of the chunks it took passes `max_entry_bytes`. That bounds the memory a stream holds whatever its chunks carry, logprobs included; since each chunk repeats the response's id, model and other framing, a long streamed answer (a few thousand one-token chunks at the 1 MiB default) is not cached even when its non-streamed form would fit. Either form serves the other: a non-streaming miss can be replayed to a streaming client and the reverse.

**Coalescing.** Identical non-streaming misses that arrive at the same time, with the same exact key, share one upstream call on each replica: the first is sent, and when its answer is cacheable the others are served it as exact hits (each with its own usage event); when it is not, or the first request fails or its client goes away, the others go upstream themselves. Streams are not coalesced, and coalescing across replicas is post-MVP.

**The semantic cache** matches a request whose context is identical (the same scope, target, system prompt, earlier messages, tools, `response_format` and parameters: everything but the last user message, hashed into a partition) and whose last user message is similar enough: the cosine similarity of the two messages' embeddings must reach `min_similarity` (default 0.95). It applies only when the last message is a user message of at most `max_chars` characters (default 2,000), of text only, with no image or audio anywhere and `n` absent or 1. Its main risk is a false hit on a near-duplicate question with a different answer, such as "What's the capital of France?" and "What's the capital of Germany?", or "5 km" and "50 km": the high threshold, the exact partition, the short-message limit and the opt-in all guard against it, but check it on your own model with `make semantic-eval` before turning it on, and tune the threshold from `switchyard_cache_similarity`. Each lookup embeds the last user message with `semantic_cache.embedding_model` (a catalog embedding model, or `model@provider`; otherwise its cheapest priced deployment), so **the last user message is sent to that deployment**: pick one your data-handling terms allow. Its policy filters apply as to any deployment: config validation (and startup) rejects an embedding deployment that a policy enabling the semantic cache, or a rule whose `set.cache` enables it, does not permit by its `allow`, `deny` and `require_tags`; and a request whose effective policy (after rules) excludes it skips the semantic layer, counted `result="policy"`, without embedding. The embedding and the vector search must finish within `semantic_cache.lookup_timeout` (default 150 ms), and the embedding's cost is recorded as the request's overhead, hit or miss, and charged to its budgets (see Accounting). A request whose tenant or key has a blocking budget with no room left skips the semantic layer, counted `result="budget"`, rather than pay for an embedding; exact hits are still served, since they are free. The vector index is named after the embedding deployment and its dimensions, so changing the model starts a new, empty index.

**Storage and Redis roles.** With `storage.mode: memory`, exact entries, the vector index and namespace versions live in process, each cache capped at `cache.memory_max_entries` entries and at 256 MiB of entries (bodies, names, keys and vectors), the least recently used evicted first, and lost on restart. Vector searches in memory run concurrently and stop at `semantic_cache.lookup_timeout`; a partition of many thousands of entries still costs a brute-force scan per lookup, so use Redis for a large semantic cache. With `storage.mode: redis`:

- the **cache instance** (`storage.redis.cache_url`) holds the exact entries (one key each, with its TTL) and the semantic index. Run it with `maxmemory` and `maxmemory-policy allkeys-lru`, private, with AUTH and TLS: it is the only place Switchyard stores prompt and response content. The semantic cache needs the Redis query engine (Redis 8, or Redis Stack), which indexes only database 0, so with semantic caching on `cache_url` must select database 0, and config validation rejects any other, whether named by the path or by a `db` query parameter (which wins over the path, as in the client).
- the **state instance** (`storage.redis.state_url`, `noeviction`) holds each tenant's namespace version beside the budgets, since an evicted version would bring purged entries back.

A small install can point both roles at one Redis server: put the cache on database 0 and the state on another database (`redis://host:6379/0` and `redis://host:6379/1`). That is unsuitable for production, because the eviction policy applies to the whole server: with `allkeys-lru` budgets can be evicted, and with `noeviction` a full cache refuses writes.

**Purge.** `POST /admin/v1/cache/purge` on the admin listener, with the body `{"tenant":"<id>"}`, bumps the tenant's namespace version, which every exact key and semantic partition of the tenant includes. All of the tenant's entries stop matching at once, in O(1), and expire with their TTLs; other tenants are untouched. It answers 200 with `{"tenant":"<id>","namespace_version":N}`, 400 for a missing or unknown tenant, and 503 when the namespace store is unavailable.

**Accounting.** A hit's usage event has `cache_status` `exact` or `semantic`, cost 0 (known), 0 attempts, the cached answer's tokens and deployment, and as baseline cost those tokens priced at the baseline model; reports count it in `cache_hit_rate` and its baseline cost as `savings_usd.cache`, never as routing savings. Budgets are never charged for the cached answer, and hits are not shadow sampled. Embedding calls are recorded as overhead (`overhead_usd`) on the request that made them, and charged to its tenant's and key's budgets: on a miss with the chat cost when the request settles, and on a hit, or a request refused before admission, on their own. A charge that fails never fails the response; it is counted in `switchyard_cache_overhead_uncharged_total`.

**Failures are misses.** A cache that is down, slow or returns something unreadable never fails a request: the lookup is a miss, bounded by `cache.lookup_timeout` (default 25 ms) for the exact layer and `semantic_cache.lookup_timeout` for the semantic one, so a hung Redis adds at most the timeout; each failure is counted and logged at WARN at most once per 10 s per layer, without content. When the namespace version cannot be read, both layers are skipped; when the exact entry cannot be read, the semantic layer is skipped too (it lives on the same cache instance), counted as an error, without paying for an embedding. The cache Redis client's socket timeouts are set above the longest of these deadlines (and the write timeout), so the configured timeouts, not a shorter client default, bound each call. Writes are asynchronous, after the response, each within 1 s, on a bounded pool; a write that finds the pool full is dropped and counted. At shutdown, writes in flight get 1 s, after the shadow runner and before the usage ledger flush. `/readyz` never checks the cache Redis.

**Metrics and logs.** On the admin `/metrics`: `switchyard_cache_lookups_total{layer,result}` (layer `exact` or `semantic`; result `hit`, `miss`, `error`, `skipped` for a request of a caching policy that is not eligible for the layer, `policy` for an entry, or for the semantic layer the embedding deployment, that the request's policy does not permit, and `budget` for a semantic lookup skipped on a spent budget), `switchyard_cache_writes_total{layer,result}` (`ok`, `error`, `dropped`, `too_large`, `not_cacheable`), `switchyard_cache_similarity{outcome}` (the nearest semantic entry's similarity, `hit` or `miss`, with buckets dense between 0.8 and 1), `switchyard_cache_lookup_seconds{layer}`, `switchyard_cache_coalesced_total`, `switchyard_cache_coalesce_waiting` (requests waiting on another's upstream call now), `switchyard_cache_overhead_uncharged_total`, and `switchyard_savings_usd_total{source="cache"}`. The access log's `cache` field is `miss`, `exact` or `semantic`. Prompts and responses are never logged.

## Rate limits

Each key's request rate and concurrent streams can be limited (DESIGN §7 step 3, §17), on the tenant for all its keys or on one key, which replaces the tenant's limit entirely:

```yaml
tenants:
  - id: acme
    rate_limit: {requests_per_minute: 600, burst: 100, max_concurrent_streams: 20}
    keys:
      - {id: acme-batch, hash: "sha256:...", rate_limit: {requests_per_minute: 60}}
rate_limits:
  fail_mode: open                 # or closed: 503 rate_limit_unavailable while the store is down
  auth_failures_per_minute: 30    # per client address; -1 turns it off
```

- **Request rate.** GCRA, charged right after authentication, for every request, cache hits included. `burst` (default a sixth of the rate, at least 1) is how many requests an idle key may send at once on top of the rate. Over the rate, the answer is `429` with code `rate_limit_exceeded` and `Retry-After` in whole seconds, rounded up.
- **Concurrent streams.** A streaming chat or legacy completion request takes a slot once it is decoded and gives it back when its response ends; a cached stream replay takes one too. Over the cap: `429 rate_limit_exceeded` with `Retry-After: 1`.
- **Where the counters live.** With `storage.mode: redis`, in the state Redis (`storage.redis.state_url`), so every replica enforces one limit per key together: each decision is one Lua script on the Redis server's clock. A stream slot is a lease that its replica renews; a replica that crashes frees its slots within the lease TTL (60 s). In memory mode each replica has its own counters.
- **Store down.** `rate_limits.fail_mode` decides: `open` (default) serves the request unlimited, `closed` answers `503 rate_limit_unavailable`. After a failure the store is skipped for a few seconds. Both show in `switchyard_ratelimit_decisions_total{result="error"}`.
- **Failed authentications.** A client address (the TCP peer's IPv4 address, or its IPv6 /64) that fails authentication `auth_failures_per_minute` times within a minute gets `429`, whatever key or admin token it sends (so a refused guess learns nothing), until its failures age out; concurrent attempts cannot exceed the limit, and valid keys never count against it. This is in each replica's memory, not in Redis, so with n replicas an address can fail up to n times the limit. Forwarding headers are never trusted: behind a reverse proxy every client has the proxy's address, so turn it off (`-1`) and limit at the proxy.
- **Reload.** Every rate-limit setting is reloadable and applies to the next request; a raised rate takes effect within one new interval.

Metrics: `switchyard_ratelimit_decisions_total{kind,result}` (`kind` `request` or `stream`; `result` `allowed`, `limited` or `error`) and `switchyard_ratelimit_auth_limited_total`.

## Tracing

Switchyard exports OpenTelemetry traces over OTLP/HTTP (protobuf) to `<endpoint>/v1/traces`, with its own small exporter (no gRPC):

```yaml
telemetry:
  tracing:
    enabled: true
    endpoint: http://jaeger:4318       # a secret: it may hold user:password@
    sample_ratio: 0.05                 # of new traces; a sampled parent is always followed
    service_name: switchyard
    propagate_upstream: false          # send traceparent to providers
```

- **Spans.** One root server span per request (`POST /v1/chat/completions`, with the HTTP method, route and status), and a child for each step: `auth`, `ratelimit`, `policy`, `cache.exact`, `cache.semantic` (with `embed` and `search` under it), `route`, `budget.admit`, one `upstream.attempt` per attempt (`attempt.number`, provider, deployment, outcome, retry reason) and `usage.record`.
- **Attributes.** The GenAI semantic conventions on the attempt and the root (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`), and on the root `switchyard.tenant`, `switchyard.key_id` (never the key), `switchyard.profile`, `switchyard.cache_status`, `switchyard.route_reason`, `switchyard.cost_usd` and `switchyard.attempts`. Spans never hold prompt or response content, nor the URL's query.
- **Propagation.** An incoming W3C `traceparent` is honored, even with tracing disabled, so the client's trace ID reaches the logs: every request log line, the access log included, carries `trace_id` and `span_id` whenever a trace context exists. Upstream calls carry a `traceparent` only with `propagate_upstream: true` and tracing enabled, and only that of the gateway's own upstream-attempt span: with tracing disabled nothing is propagated, a client's own trace context is never forwarded, and neither `tracestate` nor baggage is ever sent. Clients control sampling and trace IDs: a request with a sampled `traceparent` is always sampled (parent-based), whatever `sample_ratio` says, and its spans and log lines carry the client's trace ID.
- **Failing open.** Spans wait at most 5 s in a bounded queue (2,048 spans; a span that finds it full is dropped, never waited for) and are posted once, without retries; a failed export is dropped and logged, rate limited, without the endpoint. Both kinds of drop are counted. Counters: `switchyard_tracing_spans_exported_total` and `switchyard_tracing_spans_dropped_total` (alert `SwitchyardTracingSpansDropped`). With tracing disabled, the spans cost nothing.
- **Collector auth** is the endpoint URL's userinfo, sent as Basic auth. `OTEL_EXPORTER_OTLP_HEADERS` and other header settings are not supported. `OTEL_RESOURCE_ATTRIBUTES` adds resource attributes.
- `telemetry.*` is restart-only.

## Hot reload

`SIGHUP` or `POST /admin/v1/reload` (on the admin listener) loads and validates the configuration again, from the same file, includes, environment overrides and flags the process started with, builds the new snapshot and swaps it in atomically. In-flight requests, streams included, finish on the snapshot they started with.

- **Rejected.** On any error the configuration in force keeps serving; the admin caller gets `400 {"result":"error","error":"…"}` (config paths and problems, never secret values), and the error is logged and counted.
- **Applied at once.** Tenants and keys, the admin token, policies, the catalog, aliases, routing, budgets, cache policies, rate limits and quality overrides.
- **Restart-only.** These are not applied; a reload that changes them answers `200` with them listed in `restart_required`, and logs them: the listeners and every other `server.*` setting except `admin_token` (TLS included), `storage`, `logging`, `telemetry`, `evaluation`, `cache`, `semantic_cache`, the health tracker's settings (`routing.breaker`, `routing.rate_limit_cooldown`, `routing.fallback.retry_budget_ratio` and `retry_budget_min`), and features a reload turns on that are built only at startup: the semantic cache (`policies[].cache.semantic`), shadow sampling (`policies[].shadow.sample_rate`), and with `storage.mode: redis`, a response cache when none was on at startup (`policies[].cache`).
- **Startup-only parts.** The semantic cache's embedder and the shadow runner keep the adapters built at startup: a provider key rotated by a reload (a `${file:…}` reference is read again; the environment is not) reaches the request path at once, but those two only after a restart. A deployment a reload removes is never chosen again, but stays listed with its last health in `/admin/v1/providers` until a restart.
- **Signals.** `switchyard_config_reloads_total{result="ok|error"}`, `switchyard_config_last_reload_success_timestamp_seconds`, the alert `SwitchyardConfigReloadFailed`, and `/readyz`'s `config_version`, which shows config drift between replicas. Every reload writes an audit line (`msg="audit"`, `action="config.reload"`, `result`, `remote_ip`, the fingerprint), as does every cache purge (`action="cache.purge"`); audit lines never hold a token or a body.

## Admin API and TLS

**Admin token.** With `server.admin_token` set, every `/admin/v1/*` route requires `Authorization: Bearer <token>`, compared in constant time; anything else gets `401` with code `invalid_admin_token`. The token must be at least 32 characters, with no whitespace or control characters (validation rejects anything else, at startup and on reload; `openssl rand -base64 32` or `switchyard keys generate` make one). Failed admin tokens count against the client address's `rate_limits.auth_failures_per_minute`, shared with failed API keys: past it, every admin request from that address gets `429` whatever token it sends, and no line is logged for it. `/metrics`, `/healthz` and `/readyz` on the admin listener stay open for Prometheus and probes. The token is required when `server.admin_listen` is not a loopback address (an empty host and `0.0.0.0` are not loopback), and a reload cannot remove it while the listener is exposed. It is reloadable, so rotate it with a `${file:…}` reference and a reload. The admin listener is plain HTTP: keep it on loopback or a private network, never public.

**TLS.** `server.tls` serves the public listener over HTTPS:

```yaml
server:
  tls: {cert_file: /run/secrets/tls.crt, key_file: /run/secrets/tls.key, min_version: "1.3"}   # min_version 1.2 (default) or 1.3
```

The certificate and key are PEM files, read at startup (TLS is restart-only), and both must be set. The admin listener is always plain HTTP and serves `/healthz` and `/readyz` too, open like `/metrics`, so with TLS on point the container healthcheck there: `switchyard healthcheck --url http://127.0.0.1:9090/readyz` (or `/healthz`; use your `server.admin_listen` port). The image binds the admin listener to `127.0.0.1:9090` by default, loopback inside the container, which a healthcheck run in the container reaches; compose's `:9090` does too. The public listener's certificate names your public host, not `127.0.0.1`, so an `https://127.0.0.1` healthcheck would fail verification. Or terminate TLS in front of the gateway instead.

## Operations

- [docs/quickstart.md](docs/quickstart.md): the compose stack and the single binary, the first request, dashboards and traces, changing the configuration, and hardening before production.
- [docs/configuration.md](docs/configuration.md): every config key, with its type and default, generated from `internal/config/types.go` (`make docs-config`).
- [docs/runbook.md](docs/runbook.md): symptom, signal, impact and fix for every degraded mode and alert, and what to do when a key leaks.
- [deploy/](deploy/): `compose.yaml` (gateway, both Redis instances and Postgres; profiles `observability` with Prometheus, Grafana and Jaeger, and `tei`), `init.sh`, the Prometheus alert rules with their unit tests, and the Grafana dashboard.
- [test/load/RESULTS.md](test/load/RESULTS.md): the overhead benchmarks, the 1,000 req/s SLO run and the soak, with the hardware they ran on.
- Releases: `make release-snapshot` builds the archives, checksums and SBOMs locally; a `v*` tag runs [.github/workflows/release.yml](.github/workflows/release.yml), which is written but has never run. Images go to `ghcr.io/itszarmik/switchyard`; a fork changes the owner in `.goreleaser.yaml`, the image labels and the alert `runbook_url`s.

## Docker

```sh
make docker          # builds deploy/Dockerfile as switchyard:dev
make docker-check    # also fails unless the image is <= 40 MB uncompressed, runs as UID 65532,
                     # and serves /healthz and /readyz read-only with no capabilities
```

The image is a static binary on `gcr.io/distroless/static-debian13:nonroot`. It runs as `65532:65532`, with nothing written at runtime, so it runs on a read-only root filesystem, and starts `serve` with the quickstart config baked in at `/etc/switchyard/switchyard.yaml`. Its `HEALTHCHECK` runs `switchyard healthcheck`, which GETs `http://127.0.0.1:8080/healthz` and exits 0 on 200 (`--url` and `--timeout` change it); the image has no shell or curl.

```sh
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -p 8080:8080 -e OPENAI_API_KEY switchyard:dev
```

The admin listener keeps the quickstart's `127.0.0.1:9090`, loopback inside the container, so nothing outside it reaches `/metrics`, `/healthz`, `/readyz` or `/admin/v1/*`, and no admin token is needed; a healthcheck inside the container can use it (see TLS above). To scrape it from another container, bind it to all interfaces (`SWITCHYARD_ADMIN_LISTEN=":9090"`) and set `server.admin_token`, which validation then requires, as [deploy/compose.yaml](deploy/compose.yaml) does; never publish 9090 on a public interface. The image exposes only port 8080, so `docker run -P` cannot publish the admin listener. To use your own config, mount the directory that holds it and point `--config` at the file. Mount the directory, not the file: editors and `sed -i` save by writing a new file and renaming it, which a single-file bind mount never shows the container, so a reload would read the old file.

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/myconfig:/etc/switchyard/custom:ro" \
  -e OPENAI_API_KEY \
  switchyard:dev serve --config /etc/switchyard/custom/switchyard.yaml
```

The Dockerfile uses BuildKit features (cache mounts), so `docker build` needs the buildx plugin. For the whole stack, see [Quickstart](#quickstart) and `make compose-up`.

## Dev dependencies

```sh
make dev-up      # start redis-cache, redis-state and postgres and wait until healthy
make dev-logs    # follow their logs
make dev-down    # stop them; the named volumes are kept
```

`dev-up` starts only the dependencies from [deploy/compose.yaml](deploy/compose.yaml), not the gateway (`make compose-up` runs the whole stack). The ports bind to 127.0.0.1:

| Service | Host port | Role |
|---|---|---|
| `redis-cache` | 6379 | Evictable cache: `maxmemory 256mb`, `allkeys-lru`, no persistence |
| `redis-state` | 6380 | Budgets, rate limits and cache namespaces: `noeviction`, append-only file on a named volume |
| `postgres` | 5433 | Usage ledger. User, password and database are all `switchyard` unless `POSTGRES_PASSWORD` is set, for development only. |

To delete the data as well, run `docker-compose -f deploy/compose.yaml down -v` (or `docker compose …`).

## macOS and Colima notes

- Start the Docker daemon first with `colima start`.
- The Makefile uses `docker compose` when that plugin is registered and falls back to the standalone `docker-compose` binary otherwise, so registering the plugin is optional. To register it, add the line Homebrew prints to `~/.docker/config.json`, for example `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]`.
- `make docker` needs the buildx plugin: `brew install docker-buildx`, with the same `cliPluginsExtraDirs` entry. Without it, `docker build` fails with "BuildKit is enabled but the buildx component is missing or broken". Until it is installed, the Colima VM's own Docker CLI, which includes buildx, can build against the same daemon: `make docker-check DOCKER="colima ssh -- docker"` (Colima mounts your home directory at the same path inside the VM).
- `make test-integration` needs only Colima's Docker daemon and `docker-compose` (or the `docker compose` plugin): it starts the services itself. It uses host ports 5433 and 6380, so stop anything else on them first, `make dev-up` included.

## Make targets

Run `make` or `make help` to list them.

| Target | What it does |
|---|---|
| `build` | Builds `bin/switchyard` with `-trimpath` and version metadata from git. |
| `test` | `go test -race -count=1 ./...` |
| `test-integration` | Starts Postgres, `redis-state` and `redis-cache` from `deploy/compose.yaml` in a separate compose project (`switchyard-test`), runs the tests tagged `integration` with `SWITCHYARD_TEST_POSTGRES_DSN`, `SWITCHYARD_TEST_REDIS_URL`, `SWITCHYARD_TEST_CACHE_REDIS_URL` and `SWITCHYARD_TEST_COMPOSE` (the compose command of that project) set, and always removes the containers and their volumes afterwards. The tests cover the Postgres ledger, the Redis budget store, the Redis cache stores and vector index, the Redis rate limiter, and in `test/integration` the P4 acceptance scenarios through the gateway binary: exact costs checked against the ledger rows, 1,000 concurrent requests against a Redis budget, and Postgres stopped for 60 s under load; the P8 shadow scenarios: the shadow budget in Redis under concurrent sampled requests, with every `shadow_evals` row checked against `usage_events`, and verified savings from the Postgres report; the P6 cache scenarios: an exact hit's ledger row and report, purging one tenant's exact and semantic entries in Redis, and the cache Redis stopped mid-run; and the P7 rate limits across two gateway replicas sharing the state Redis. It takes a few minutes. It uses the same host ports as `dev-up`, so run `make dev-down` first. Without those variables, `go test -tags integration` skips these tests. Override the packages with `INTEGRATION_PKGS` and the flags with `INTEGRATION_FLAGS` (default `-race -count=1 -p 1 -timeout 20m`; `-p 1` because the outage test stops the shared Postgres). |
| `cover` | Writes `coverage.out` and prints a per-function coverage summary. |
| `bench-slo` | The P7 overhead SLO: an open loop at 1,000 req/s for 60 s (80% non-streaming, 20% streaming, caching off; a key rate limit and stream cap never reached, and every request traced to an in-process collector) through the gateway binary in front of a fake with 20 ms latency; fails if the added latency is over 2 ms at p50 or 10 ms at p99, and warns when the load generator itself stalled. Pass flags with `BENCH_SLO_FLAGS`. Results and hardware in [test/load/RESULTS.md](test/load/RESULTS.md). |
| `soak` | Runs the gateway for `DURATION` (default `1h`) at the target load, 1,000 req/s (the SLO's rate), with 2% injected upstream 500s, a per-key rate limit and stream cap it never reaches, and tracing of every request to an in-process collector. It checks that goroutines and memory stay flat, that the client's errors equal the injected ones exactly, and that no span, log line or request was dropped or rate limited; the report goes to `SOAK_OUT` (default `bin/soak-report.md`). Pass flags with `SOAK_FLAGS`, for example `-redis-url redis://127.0.0.1:6380/0` (or `SWITCHYARD_SOAK_REDIS_URL`) for `storage.mode: redis`. The warm-up is a third of the run, at least until the memory usage ledger is full (110 s at 1,000 req/s), so `make soak DURATION=10m` works at the default rate. |
| `compose-up`, `compose-down` | Start the full stack in `deploy/compose.yaml` (writing `deploy/.env` with `init.sh` on the first run), with `PROFILES="observability tei"` for the optional services; stop it, keeping the volumes unless `COMPOSE_DOWN_FLAGS=-v`. |
| `release-snapshot` | A local GoReleaser dry run: binaries for linux and darwin on amd64 and arm64, their archives, checksums and CycloneDX SBOMs in `dist/`, without images and without publishing. |
| `docs-config` | Regenerates `docs/configuration.md` from `internal/config/types.go`; a test fails while it is stale. |
| `check-deploy` | Validates `deploy/`: the Prometheus config and alert rules and their unit tests (promtool), the Grafana dashboard, that every metric they query exists in the code, the compose file with every profile, `deploy/config/switchyard.yaml` and `init.sh`. |
| `demo-eval` | The quality evaluation demo (`test/demoeval`): one scripted fake plays the candidates, the reference and a judge that rejects the small tier on coding; runs `eval run --dry-run`, `eval run` under a cost cap and `eval fit` on the starter prompt set, prints the report, then starts the gateway without and with the overrides and shows how the coding prompts are routed. Fails unless the small tier's coding share drops and the prompts move to the mid tier. See [Evaluation and verification](#evaluation-and-verification). |
| `eval-live` | `switchyard eval run` against real providers. Opt-in and billed: it skips unless `EVAL_CONFIG`, `EVAL_CANDIDATES`, `EVAL_REFERENCE` and `MAX_COST_USD` are set, and needs the real provider keys in the environment variables the config references (without them it fails at config load and spends nothing). `MAX_COST_USD` caps the invocation's spend, `JUDGE` is passed as `--judge`, `DRY_RUN=1` prints the estimate only (`DRY_RUN=0` runs for real), `EVAL_PROMPTS` and `EVAL_OUT` default to the starter set and `results.jsonl`. |
| `demo-routing` | The mixed-workload routing demo: fake OpenAI-wire and Anthropic upstreams, a three-tier catalog, 30 easy, 30 medium and 30 hard prompts from the quality sanity set sent as `auto:balanced`; prints how each band was routed and the usage API's cost, baseline and savings, and fails unless most easy prompts went to the small tier, most hard ones to the large tier, and savings are above 0. See [Routing](#routing). Pass flags with `DEMO_FLAGS`. |
| `bench-overhead` | Measures the latency the gateway adds against a zero-latency fake upstream; results in [test/load/RESULTS.md](test/load/RESULTS.md). Pass flags with `BENCH_FLAGS="-mode stream -concurrency 200"`; the default is the P1 acceptance run (non-stream, concurrency 100). `BENCH_FLAGS="-cache miss"` turns the exact cache on and makes every request a miss, to measure the lookup on the miss path. |
| `bench-cache` | Measures exact-cache hits through the gateway with `storage.mode: redis`: starts `redis-state` and `redis-cache` in the `switchyard-test` compose project (the `test-integration` ports), warms one entry, fires 20,000 hits at each of concurrency 1, 8 and 50, prints p50, p90 and p99 of the client-observed latency per level, fails if any request errors, if more than 0.1% of a level's requests miss (a miss is the cache failing open on a slow Redis call), or if p99 at concurrency 1 is over 5 ms (8 and 50 are reported for information: at higher concurrency a closed loop on a shared machine measures queueing and CPU contention, not the hit; use `-gate-concurrency 8` to gate there), and tears the project down. Pass flags with `BENCH_CACHE_FLAGS="-requests 50000 -concurrency 8,100"`. |
| `semantic-eval` | Runs the semantic cache's paraphrase and near-miss sets (`test/cache`) through a real embedding model, at the configured threshold: needs `SWITCHYARD_SEMANTIC_CONFIG` (a config whose `semantic_cache.embedding_model` names it) and its provider key, and skips without them. Billed: a few hundred small embedding calls. |
| `smoke` | Live smoke test against OpenAI with the SDK. Opt-in: it skips unless `OPENAI_API_KEY` is set; `SMOKE_MODEL` defaults to `gpt-5-mini`. |
| `record-fixtures` | Records live Anthropic API responses into `test/fixtures/anthropic/recorded/`, for comparison with the handcrafted fixtures. Opt-in and billed: it skips unless `ANTHROPIC_API_KEY` is set. Until it has been run, the fixtures are handcrafted from Anthropic's documentation. |
| `fmt` | Formats the code with `golangci-lint fmt` (gofmt, goimports). |
| `fmt-check` | Fails if formatting would change anything. |
| `vet` | `go vet ./...` |
| `lint` | `golangci-lint run` with [.golangci.yml](.golangci.yml). |
| `vuln` | `go tool govulncheck ./...` |
| `tidy-check` | `go mod tidy -diff`: fails if go.mod or go.sum are not tidy. |
| `check` | fmt-check, vet, lint, build, test, vuln and tidy-check. Run it before every merge; it needs no Docker. |
| `run` | Builds, then serves `configs/quickstart.yaml`. Override with `make run CONFIG=path`. |
| `docker` | Builds the image `switchyard:dev`. |
| `docker-check` | Builds the image, checks its uncompressed size (at most 40 MB, the sum of its layers as `docker history` reports them) and user, then runs it read-only with no capabilities and waits for `/healthz` and `/readyz`. Without buildx on the host: `make docker-check DOCKER="colima ssh -- docker"`. |
| `dev-up`, `dev-down`, `dev-logs` | Manage the dev dependencies (`redis-cache`, `redis-state`, `postgres`) in `deploy/compose.yaml`. |
| `clean` | Removes `bin/`, `dist/` and coverage files. |

`VERSION`, `COMMIT` and `DATE` come from `git describe`, `git rev-parse` and the clock, with fallbacks for a repository without commits. Override any of them, for example `make build VERSION=v0.1.0`.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs the same checks on every push and pull request: build and race tests on the two supported Go releases, sample-config validation, golangci-lint, govulncheck, `go mod tidy -diff`, and the image size and user checks. CI never calls a real provider. On a `v*` tag, [.github/workflows/release.yml](.github/workflows/release.yml) runs `make check`, then GoReleaser with Buildx and QEMU, pushing the images to ghcr.io; neither workflow has run yet.

## Project layout

```
cmd/switchyard/     CLI: serve, config validate, catalog lint, route explain, keys generate, eval,
                    healthcheck, version
internal/
  api/openaiapi/    HTTP handlers for /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
                    and /switchyard/v1/route/explain
  api/usageapi/     usage reports: /switchyard/v1/usage and /admin/v1/usage; POST /switchyard/v1/feedback
  apierror/         errors in the OpenAI-compatible JSON envelope
  app/              composition root: config snapshot to running service
  auth/             gateway API keys: hashing, store, middleware
  budget/           budget scopes and windows; memory and Redis (Lua) stores
  cache/            response caches: canonical keys, scopes, entry stores (memory, Redis), namespace
                    versions, stream replay and aggregation, coalescing; semantic/ (embedder,
                    vector indexes: memory and the Redis query engine)
  catalog/          models, deployments, prices; resolution, selection, lint
  config/           schema, loading, ${…} expansion, includes, validation, snapshot
  eval/             switchyard eval: runner with a cost cap, Q fit with bootstrap CIs, overrides, weights
  gateway/          request pipeline: routing (assessment, policy, downgrade, router), the
                    retry/fallback executor, completions shim, budget admission, usage events,
                    explain, metrics, log fields, response caching, /admin/v1/providers and
                    /admin/v1/cache/purge
  judge/            pairwise LLM judge, both presentation orders, per-category rubrics
  health/           per-replica breakers, cooldowns, retry budgets, concurrency, latency stats
  policy/           effective policy: base policy, governance and quality rules, header tightening
  quality/          the heuristic assessor, the quality estimate, the labeled sanity set
  ratelimit/        per-key GCRA request rates and stream leases (memory, Redis), the enforcer,
                    the per-address auth-failure limit
  router/           candidates, hard filters, estimates, soft checks, ranking, plan, explanation
  providers/        adapter interfaces; openai/ (OpenAI wire) and anthropic/ (Messages API)
  schema/           lossless chat, legacy completion and embedding requests; capabilities
  shadow/           shadow sampling: deterministic sampler, budgeted background runner
  server/           listeners, middleware, health checks, graceful shutdown
  sse/              server-sent events reader and writer
  telemetry/        slog with secret redaction, the asynchronous log writer, Prometheus registry
  tracing/          OpenTelemetry: tracer provider, server middleware, spans, the OTLP/HTTP exporter
  usage/            exact money and costs, usage events, shadow evaluations, feedback, memory and
                    Postgres ledgers, reports with verified savings
  reqctx/           request ID and request-scoped logger in context
  version/          build metadata
test/
  fakeupstream/     scriptable fake OpenAI-wire server
  fakeanthropic/    scriptable fake Messages API (make record-fixtures records real ones)
  fixtures/         Anthropic fixtures, handcrafted from the docs until recorded
  conformance/      OpenAI SDK conformance suite, direct and through the gateway (OpenAI and
                    Anthropic upstreams), plus the P2 to P6 and P8 acceptance cases
  scenario/         the P4, P6 and P8 acceptance scenarios, shared by conformance/ and integration/
  integration/      P4, P6 and P8 scenarios on Postgres and Redis, Postgres and cache Redis outages
                    (make test-integration)
  cache/            the semantic cache's paraphrase and near-miss sets, and their live harness
                    (make semantic-eval)
  cachebench/       exact-cache hit latency through the gateway with Redis (make bench-cache)
  eval/             fake judge (fakejudge/), the starter prompt set (prompts/starter.jsonl)
  demoeval/         quality evaluation demo (make demo-eval)
  load/             overhead benchmark and the 1,000 req/s SLO (make bench-overhead, make bench-slo)
  soak/             the soak test (make soak)
  demo/             mixed-workload routing demo (make demo-routing)
  smoke/            opt-in live smoke test (make smoke)
configs/            quickstart.yaml, switchyard.example.yaml, catalog/
deploy/             Dockerfiles, compose.yaml (the stack), config/switchyard.yaml, init.sh, prometheus/, grafana/
docs/               DESIGN.md, PLAN.md, quickstart.md, configuration.md (generated), runbook.md
tools/configdoc/    generates docs/configuration.md
```

Later phases add the packages listed in [DESIGN §19](docs/DESIGN.md#19-project-structure).

## License

TBD.
