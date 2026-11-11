# Switchyard

Switchyard is a cost-aware, OpenAI-compatible LLM inference gateway written in Go. Applications point their OpenAI SDK at it, and it routes each request to the cheapest deployment that is good enough, across OpenAI, Anthropic, Gemini and self-hosted open-source models. It also handles fallback, caching, budgets and cost reporting.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Delivery plan and acceptance criteria: [docs/PLAN.md](docs/PLAN.md)

## Status

**Phase 5: cost-aware routing, quality scoring and the rule engine.** Switchyard serves `POST /v1/chat/completions` and `POST /v1/completions` (JSON and streaming), `POST /v1/embeddings` and `GET /v1/models` behind gateway API keys. Each request goes to the cheapest deployment that has every capability it needs and meets its policy, on OpenAI, Anthropic (translated from and to the OpenAI format), Gemini's OpenAI-compatible endpoint or any OpenAI-compatible host. Failures are retried and fall back to the next cheapest deployment of the same model, within a deadline and a retry budget, with circuit breakers, rate-limit cooldowns and per-provider concurrency limits (see [Reliability](#reliability)). Every request gets an exact cost in a usage ledger (memory or Postgres) with tenant-scoped reports, and tenants and keys can have daily or monthly budgets (see [Usage ledger and reporting](#usage-ledger-and-reporting) and [Budgets](#budgets)). With `model: "auto"`, a policy with a baseline model, and quality priors in the catalog, the gateway picks the cheapest model whose estimated quality for the request clears the profile's bar, explains every decision, and measures the savings against the baseline (see [Routing](#routing)). Caching comes in a later phase ([docs/PLAN.md](docs/PLAN.md)).

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

**Includes.** `include:` lists glob patterns, relative to the root file's directory. Included files may only define `providers`, `models` and `aliases`; list entries are appended in sorted file-name order, and a duplicate ID or alias name, in any files, is an error. The example config includes the model catalog from [configs/catalog/](configs/catalog/).

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
| `X-Switchyard-Attempts` | The number of upstream attempts, retries and fallbacks included. |
| `X-Switchyard-Route-Reason` | `cheapest_eligible`, `pinned` (a `model@provider` request), `preferred` (a policy `prefer` tier decided over a cheaper deployment), `quality_relaxed` or `latency_relaxed` (no deployment met the soft constraint, so it was relaxed), `budget_downgrade` (a budget's `soft_profile` is in force), `observe` (observe mode served what was asked for), or `fallback:<kind>` when a fallback answered, `<kind>` being the failure that caused the first fallback, such as `fallback:server_error`. See [Routing](#routing). |
| `X-Switchyard-Profile` | The routing profile in force (chat and legacy completions). |
| `X-Switchyard-Policy-Warning` | The request headers the policy ignored because they would have loosened it, `; `-separated. |
| `X-Switchyard-Shadow-Model` | In observe mode, the model routing would have chosen. |
| `X-Switchyard-Alias` | The alias the client sent, when it sent one. |
| `X-Switchyard-Dropped-Params` | The request parameters removed because the deployment cannot honor them, such as `n, temperature`. |
| `X-Switchyard-Shim` | `chat` when a legacy completion was served through Chat Completions. |
| `X-Switchyard-Upstream-Request-Id` | The provider's own `x-request-id`, from the attempt that answered, for support tickets. Omitted unless it is at most 128 bytes of printable ASCII. |
| `X-Switchyard-Cost-Usd` | The request's exact cost in USD, such as `0.000165`, on a successful non-streaming response whose usage the provider reported and whose deployment has prices. A stream's cost is in the [usage ledger](#usage-ledger-and-reporting). |
| `Retry-After` | On 429, copied from the provider; on `budget_exceeded`, the seconds until the budget's window resets. |

Streams carry `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The response is committed only when the first upstream event arrives, so failures before that are retried or fall back, and otherwise get a normal error status. A client can shorten its request's deadline with the request header `X-Switchyard-Timeout-Ms: <milliseconds>`; it can never extend it past `routing.request_timeout`. It can attach attribution tags to the request's usage event with `X-Switchyard-Tags: team=search,feature=autocomplete` (see [Usage ledger and reporting](#usage-ledger-and-reporting)).

**Errors.** Every error uses OpenAI's envelope, `{"error":{"message","type","param","code"}}`, so SDKs raise their usual exceptions.

| Situation | Status | `code` |
|---|---|---|
| Missing, malformed or unknown gateway key | 401 | `invalid_api_key` |
| Invalid JSON, missing `model` or `messages`, `model` over 256 bytes, non-boolean `stream` | 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `invalid_type` or `empty_array`; `param` names the field |
| Body larger than `server.max_request_bytes` | 413 | `request_too_large` |
| Body not received within `server.body_read_timeout` (default 60s) | 408 | `request_timeout` |
| Unknown model or `model@provider` | 404 | `model_not_found` |
| `auto` or `auto:<profile>` the policy does not allow (no `baseline_model`, an unknown or disallowed profile), a virtual model on embeddings, or a model of the wrong type (an embedding model on chat, a chat model on embeddings) | 400 | `model_not_supported` |
| A malformed `X-Switchyard-Profile`, `-Max-Cost-Usd`, `-Min-Quality`, `-Max-TTFT-Ms`, `-Task` or `-Difficulty` header, or one given twice; the value is never echoed | 400 | `invalid_header`; `param` names the header |
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
- **Savings.** The baseline is the requested model for a concrete model name, or the policy's `baseline_model` for `auto`, priced at its cheapest priced deployment for the same token counts. Routing savings are `baseline − cost`: from provider arbitrage, and from `auto` or `equivalent` routing to a cheaper model. They are negative when a fallback went to something pricier. In observe mode, the event also records the deployment routing would have chosen (`shadow_deployment`) and the same tokens priced at it. Cache savings and overhead are recorded as 0 until the caches arrive in Phase 6.

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

**Requests never wait for the database.** Events go into a bounded in-memory queue, and a background writer copies them to Postgres with `COPY`, in batches of `batch_size` or every `flush_interval`. If Postgres is down or failing, the writer keeps the events and retries with exponential backoff (100 ms doubling to 10 s); requests are unaffected. A retried batch is never stored twice. If the queue fills up, the oldest events are dropped and counted. On shutdown, once the server has drained, the writer flushes what is queued, within `server.shutdown_grace`; events it cannot write by then are counted as dropped and the exit status is 1. Watch these metrics:

| Metric | Meaning |
|---|---|
| `switchyard_usage_queue_depth` | Events waiting to be written, including the batch being written |
| `switchyard_usage_events_written_total` | Events written |
| `switchyard_usage_write_errors_total` | Failed batch writes; each is retried |
| `switchyard_usage_events_dropped_total` | Events lost: dropped from a full queue, rejected by the database, or left at shutdown. Alert on any increase. |

Log lines about the ledger carry event IDs and counts, never event contents.

**Attribution tags.** A client sends `X-Switchyard-Tags: k=v,k2=v2` to tag a request's usage event, for example by team or feature, and reports group by `tag:<key>`. At most 10 tags; keys are 1 to 32 characters of `a-z`, `0-9`, `_`, `.` and `-`, starting with a letter or digit, each at most once; values are 1 to 64 printable ASCII characters other than `,` and `=`; spaces around keys and values are ignored, and several header lines are joined. A malformed header gets 400 `invalid_tags`, without echoing it.

**Metrics.** On the admin `/metrics`: `switchyard_cost_usd_total{provider,model}` (a float approximation of the ledger's exact sums; over a run they agree to within 1e-6), `switchyard_tokens_total{provider,model,class}` with class `input` (uncached), `cached_input`, `cache_write` and `output`, `switchyard_savings_usd_total{source="routing"}`, the running total of `baseline − cost`, which decreases when a fallback costs more than the baseline, and `switchyard_routing_projected_savings_usd_total`, observe mode's running total of projected savings. Both savings metrics are gauges, not counters, because a request's savings can be negative.

**Reports.** A report aggregates the events of one tenant over a time range `[from, to)`, in UTC, optionally grouped by one or more of `day`, `model` (the requested model), `deployment`, `provider`, `key`, `endpoint`, `status_class` (`2xx`, `4xx`, …), `cache_status`, `profile` (the routing profile; empty for embeddings), `routing_mode` (`enforce` or `observe`) and `tag:<name>`. It returns totals for the whole range and, per group, the same fields:

- `requests`, `errors` (status 400 and above) and `unpriced_requests`;
- token counts per class;
- `cost_usd` (the known cost plus overhead), `baseline_cost_usd`, `overhead_usd`, and `savings_usd` with `routing`, `cache` and `net` (`net = baseline_cost_usd − cost_usd`); money is an exact decimal string;
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
- `make test-integration` needs only Colima's Docker daemon and `docker-compose` (or the `docker compose` plugin): it starts the services itself. It uses host ports 5433 and 6380, so stop anything else on them first, `make dev-up` included.

## Make targets

Run `make` or `make help` to list them.

| Target | What it does |
|---|---|
| `build` | Builds `bin/switchyard` with `-trimpath` and version metadata from git. |
| `test` | `go test -race -count=1 ./...` |
| `test-integration` | Starts Postgres and `redis-state` from `deploy/compose.yaml` in a separate compose project (`switchyard-test`), runs the tests tagged `integration` with `SWITCHYARD_TEST_POSTGRES_DSN`, `SWITCHYARD_TEST_REDIS_URL` and `SWITCHYARD_TEST_COMPOSE` (the compose command of that project) set, and always removes the containers and their volumes afterwards. The tests cover the Postgres ledger and the Redis budget store, and in `test/integration` the P4 acceptance scenarios through the gateway binary: exact costs checked against the ledger rows, 1,000 concurrent requests against a Redis budget, and Postgres stopped for 60 s under load. It takes a few minutes. It uses the same host ports as `dev-up`, so run `make dev-down` first. Without those variables, `go test -tags integration` skips these tests. Override the packages with `INTEGRATION_PKGS` and the flags with `INTEGRATION_FLAGS` (default `-race -count=1 -p 1 -timeout 20m`; `-p 1` because the outage test stops the shared Postgres). |
| `cover` | Writes `coverage.out` and prints a per-function coverage summary. |
| `demo-routing` | The mixed-workload routing demo: fake OpenAI-wire and Anthropic upstreams, a three-tier catalog, 30 easy, 30 medium and 30 hard prompts from the quality sanity set sent as `auto:balanced`; prints how each band was routed and the usage API's cost, baseline and savings, and fails unless most easy prompts went to the small tier, most hard ones to the large tier, and savings are above 0. See [Routing](#routing). Pass flags with `DEMO_FLAGS`. |
| `bench-overhead` | Measures the latency the gateway adds against a zero-latency fake upstream; results in [test/load/RESULTS.md](test/load/RESULTS.md). Pass flags with `BENCH_FLAGS="-mode stream -concurrency 200"`; the default is the P1 acceptance run (non-stream, concurrency 100). |
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
| `docker-check` | Builds the image and checks its size and user. |
| `dev-up`, `dev-down`, `dev-logs` | Manage the dev dependencies in `deploy/compose.yaml`. |
| `clean` | Removes `bin/`, `dist/` and coverage files. |

`VERSION`, `COMMIT` and `DATE` come from `git describe`, `git rev-parse` and the clock, with fallbacks for a repository without commits. Override any of them, for example `make build VERSION=v0.1.0`.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs the same checks on every push and pull request: build and race tests on the two supported Go releases, sample-config validation, golangci-lint, govulncheck, `go mod tidy -diff`, and the image size and user checks. CI never calls a real provider.

## Project layout

```
cmd/switchyard/     CLI: serve, config validate, catalog lint, route explain, keys generate, version
internal/
  api/openaiapi/    HTTP handlers for /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
                    and /switchyard/v1/route/explain
  api/usageapi/     usage reports: /switchyard/v1/usage and /admin/v1/usage
  apierror/         errors in the OpenAI-compatible JSON envelope
  app/              composition root: config snapshot to running service
  auth/             gateway API keys: hashing, store, middleware
  budget/           budget scopes and windows; memory and Redis (Lua) stores
  catalog/          models, deployments, prices; resolution, selection, lint
  config/           schema, loading, ${…} expansion, includes, validation, snapshot
  gateway/          request pipeline: routing (assessment, policy, downgrade, router), the
                    retry/fallback executor, completions shim, budget admission, usage events,
                    explain, metrics, log fields, /admin/v1/providers
  health/           per-replica breakers, cooldowns, retry budgets, concurrency, latency stats
  policy/           effective policy: base policy, governance and quality rules, header tightening
  quality/          the heuristic assessor, the quality estimate, the labeled sanity set
  router/           candidates, hard filters, estimates, soft checks, ranking, plan, explanation
  providers/        adapter interfaces; openai/ (OpenAI wire) and anthropic/ (Messages API)
  schema/           lossless chat, legacy completion and embedding requests; capabilities
  server/           listeners, middleware, health checks, graceful shutdown
  sse/              server-sent events reader and writer
  telemetry/        slog with secret redaction, Prometheus registry
  usage/            exact money and costs, usage events, memory and Postgres ledgers, reports
  reqctx/           request ID and request-scoped logger in context
  version/          build metadata
test/
  fakeupstream/     scriptable fake OpenAI-wire server
  fakeanthropic/    scriptable fake Messages API (make record-fixtures records real ones)
  fixtures/         Anthropic fixtures, handcrafted from the docs until recorded
  conformance/      OpenAI SDK conformance suite, direct and through the gateway (OpenAI and
                    Anthropic upstreams), plus the P2 to P5 acceptance cases
  scenario/         the P4 acceptance scenarios, shared by conformance/ and integration/
  integration/      P4 scenarios on Postgres and Redis, Postgres outage (make test-integration)
  load/             overhead benchmark (make bench-overhead)
  demo/             mixed-workload routing demo (make demo-routing)
  smoke/            opt-in live smoke test (make smoke)
configs/            quickstart.yaml, switchyard.example.yaml, catalog/
deploy/             Dockerfile, compose.yaml (dev dependencies)
docs/               DESIGN.md, PLAN.md
```

Later phases add the packages listed in [DESIGN §19](docs/DESIGN.md#19-project-structure).

## License

TBD.
