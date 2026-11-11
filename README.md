# Switchyard

Switchyard is a cost-aware, OpenAI-compatible LLM inference gateway written in Go. Applications point their OpenAI SDK at it, and it routes each request to the cheapest deployment that is good enough, across OpenAI, Anthropic, Gemini and self-hosted open-source models. It also handles fallback, caching, budgets and cost reporting.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Delivery plan and acceptance criteria: [docs/PLAN.md](docs/PLAN.md)

## Status

**Phase 3: reliability.** Switchyard serves `POST /v1/chat/completions` and `POST /v1/completions` (JSON and streaming), `POST /v1/embeddings` and `GET /v1/models` behind gateway API keys. Each request goes to the cheapest deployment of the requested model that has every capability the request needs, on OpenAI, Anthropic (translated from and to the OpenAI format), Gemini's OpenAI-compatible endpoint or any OpenAI-compatible host. Failures are retried and fall back to the next cheapest deployment of the same model, within a deadline and a retry budget, with circuit breakers, rate-limit cooldowns and per-provider concurrency limits (see [Reliability](#reliability)). Budgets, cost-aware model choice and caching come in later phases ([docs/PLAN.md](docs/PLAN.md)).

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
| `X-Switchyard-Model` | The catalog model that served the request. |
| `X-Switchyard-Provider` | The provider id of the deployment that answered (on an error, of the last one tried). |
| `X-Switchyard-Attempts` | The number of upstream attempts, retries and fallbacks included. |
| `X-Switchyard-Route-Reason` | `cheapest_eligible`, `pinned` (a `model@provider` request), or `fallback:<kind>` when a fallback answered, `<kind>` being the failure that caused the first fallback, such as `fallback:server_error`. |
| `X-Switchyard-Alias` | The alias the client sent, when it sent one. |
| `X-Switchyard-Dropped-Params` | The request parameters removed because the deployment cannot honor them, such as `n, temperature`. |
| `X-Switchyard-Shim` | `chat` when a legacy completion was served through Chat Completions. |
| `X-Switchyard-Upstream-Request-Id` | The provider's own `x-request-id`, from the attempt that answered, for support tickets. Omitted unless it is at most 128 bytes of printable ASCII. |
| `Retry-After` | On 429, copied from the provider. |

Streams carry `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The response is committed only when the first upstream event arrives, so failures before that are retried or fall back, and otherwise get a normal error status. A client can shorten its request's deadline with the request header `X-Switchyard-Timeout-Ms: <milliseconds>`; it can never extend it past `routing.request_timeout`.

**Errors.** Every error uses OpenAI's envelope, `{"error":{"message","type","param","code"}}`, so SDKs raise their usual exceptions.

| Situation | Status | `code` |
|---|---|---|
| Missing, malformed or unknown gateway key | 401 | `invalid_api_key` |
| Invalid JSON, missing `model` or `messages`, `model` over 256 bytes, non-boolean `stream` | 400 | `invalid_json`, `missing_required_parameter`, `invalid_value`, `invalid_type` or `empty_array`; `param` names the field |
| Body larger than `server.max_request_bytes` | 413 | `request_too_large` |
| Body not received within `server.body_read_timeout` (default 60s) | 408 | `request_timeout` |
| Unknown model or `model@provider` | 404 | `model_not_found` |
| Virtual model (`auto…`), or a model of the wrong type (an embedding model on chat, a chat model on embeddings) | 400 | `model_not_supported` |
| No deployment has every capability the request needs, or fits the context window | 400 | `no_eligible_model`; the message names each deployment and why it was excluded |
| A legacy completion parameter the chat shim cannot honor | 400 | `unsupported_parameter`; `param` names it |
| `X-Switchyard-Timeout-Ms` not a positive whole number | 400 | `invalid_timeout` |
| A request an adapter cannot translate (for example a content part or tool type the Anthropic Messages API cannot express), when no other deployment in the plan could serve it | 400 | `invalid_value`; `param` names the field |
| Every eligible deployment is unhealthy: breaker open, rate-limit cooldown, or provider at `max_concurrency` | 503 | `no_healthy_upstream`; the message names each deployment and why |
| Provider 4xx (other than 401/402/403), including 429 | the provider's status | the provider's body |
| Provider 401/402/403: the gateway's credential or account is wrong | 502 | `upstream_error` (the body is never echoed) |
| Provider 5xx, connection failure | 502 | `upstream_error` |
| First-token (streams), idle or total timeout, or the request's deadline | 504 | `timeout` |
| Provider failure after the stream started | an SSE `data: {"error":…}` event, then the stream closes without `[DONE]` | `upstream_error` |

When a request was retried or fell back and every attempt failed, the answer is the most informative failure: 504 if the deadline expired, the provider's 429 if every failure was a rate limit, and otherwise, in order of preference, a context-length or not-found error, a 5xx, a timeout, an authentication failure, a connection failure, and last an adapter's local rejection (`unsupported`), which is the answer only when every attempt was rejected so. The `X-Switchyard-Provider` and `X-Switchyard-Upstream-Request-Id` headers are those of the attempt whose failure is the answer.

Each request is logged at INFO as one `request` access-log line. For the chat, completions and embeddings endpoints it also carries the tenant, key id, requested model, the `deployment` of the last attempt, the number of `excluded` deployments, `attempts`, `attempt_chain` (each attempt as `deployment:outcome`, such as `m@a:server_error,m@a:server_error,m@b:ok`, at most ten), `fallback_reason` (the failure kind that caused the first fallback, when there was one), `deadline_ms`, latency, TTFT, gateway overhead and token counts, plus the `alias` and the `dropped_params` (comma-separated) when there are any, never the prompt or the response.

**Legacy completions.** `POST /v1/completions` takes the legacy OpenAI shape (`prompt` a string or an array of strings; token arrays get 400). A deployment with the `completions` capability on an OpenAI-wire provider gets the request natively and losslessly. Any other deployment is served through the chat shim, marked `X-Switchyard-Shim: chat`: the prompt becomes one user message; `max_tokens`, `temperature`, `top_p`, `stop`, `n`, `stream`, `stream_options`, `user`, `seed` and the penalties carry over (other legacy fields, such as `logit_bias`, are not sent); and the answer, streamed or not, comes back as `text_completion` objects with `choices[].text`. The shim cannot honor `echo`, `suffix`, `logprobs`, `best_of` above 1 or more than one prompt: such a request goes to a native deployment if the model has one, and otherwise gets 400 `unsupported_parameter`. Capability filtering and dropping work as for chat. The legacy default of 16 `max_tokens` is not applied by the shim.

```sh
curl localhost:8080/v1/completions -H "Authorization: Bearer $SWITCHYARD_API_KEY" \
  -H "Content-Type: application/json" -d '{"model":"gpt-6-luna","prompt":"Say hello","max_tokens":20}'
```

**Embeddings.** `POST /v1/embeddings` takes `model`, `input` (a string, an array of strings, or token arrays, which are passed through), `encoding_format` (`float` or `base64`), `dimensions` and `user`, and is forwarded losslessly. The model must be an embedding model. Its deployments are tried cheapest input price first, with retries and fallback as for chat, so the provider may change but the model never does. Only OpenAI-wire providers serve embeddings; an Anthropic deployment is excluded with "provider does not serve embeddings".

**Models.** `GET /v1/models` lists every catalog model, chat and embedding, then every alias, all with `owned_by: "switchyard"`. Metrics (`switchyard_requests_total`, which also counts requests rejected for a bad key, `switchyard_upstream_requests_total`, `switchyard_overhead_duration_seconds` and others) are on the admin listener's `/metrics`.

## Reliability

Every request runs on a **plan**: the deployments of the requested model that can serve it, cheapest first, at most `routing.fallback.max_deployments` (default 3). Each fallback slot prefers a provider that is not in the plan yet. A pinned `model@provider` is a plan of one, and fallback never switches to a different model. Deployments that are unhealthy right now are left out of the plan: an open circuit breaker, a rate-limit cooldown, or a provider at its `max_concurrency`. If that leaves none, the answer is 503 `no_healthy_upstream`, naming each deployment and why ("circuit open", "rate-limit cooldown 4s", "provider at max_concurrency 64"). A request that no deployment could honor at all still gets 400 `no_eligible_model`.

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

**Visibility.** Besides the response headers and log fields above, the admin listener's `/metrics` has `switchyard_retries_total{deployment,kind}`, `switchyard_fallbacks_total{from,to,kind}`, `switchyard_circuit_state{deployment}` (0 closed, 1 half-open, 2 open), `switchyard_circuit_transitions_total{deployment,to}`, `switchyard_rate_limit_cooldowns_total{deployment}`, `switchyard_provider_in_flight{provider}`, `switchyard_provider_saturated_total{provider}` and `switchyard_retry_budget_exhausted_total{deployment}`. `switchyard_upstream_requests_total` counts every attempt, by outcome. `GET /admin/v1/providers` on the admin listener reports, for every provider, its `in_flight` count and `max_concurrency`, and for each of its deployments the breaker state and the time of its next transition, `cooldown_until`, the recent request and failure counts, the retry budget used, and the TTFT and tokens-per-second statistics (EWMAs, seeded from the catalog's `latency_hint` until there are 20 samples), with their sample counts. It holds no secrets.

```sh
curl -s localhost:9090/admin/v1/providers | jq '.providers[].deployments[] | {id, breaker: .breaker.state, cooldown_until}'
```

All of this state is per replica and kept in memory.

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
cmd/switchyard/     CLI: serve, config validate, catalog lint, keys generate, version
internal/
  api/openaiapi/    HTTP handlers for /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models
  apierror/         errors in the OpenAI-compatible JSON envelope
  app/              composition root: config snapshot to running service
  auth/             gateway API keys: hashing, store, middleware
  catalog/          models, deployments, prices; resolution, selection, lint
  config/           schema, loading, ${…} expansion, includes, validation, snapshot
  gateway/          request pipeline, plans and the retry/fallback executor, completions shim,
                    metrics, log fields, /admin/v1/providers
  health/           per-replica breakers, cooldowns, retry budgets, concurrency, latency stats
  providers/        adapter interfaces; openai/ (OpenAI wire) and anthropic/ (Messages API)
  schema/           lossless chat, legacy completion and embedding requests; capabilities
  server/           listeners, middleware, health checks, graceful shutdown
  sse/              server-sent events reader and writer
  telemetry/        slog with secret redaction, Prometheus registry
  reqctx/           request ID and request-scoped logger in context
  version/          build metadata
test/
  fakeupstream/     scriptable fake OpenAI-wire server
  fakeanthropic/    scriptable fake Messages API (make record-fixtures records real ones)
  fixtures/         Anthropic fixtures, handcrafted from the docs until recorded
  conformance/      OpenAI SDK conformance suite, direct and through the gateway (OpenAI and
                    Anthropic upstreams), plus the P2 and P3 acceptance cases
  load/             overhead benchmark (make bench-overhead)
  smoke/            opt-in live smoke test (make smoke)
configs/            quickstart.yaml, switchyard.example.yaml, catalog/
deploy/             Dockerfile, compose.yaml (dev dependencies)
docs/               DESIGN.md, PLAN.md
```

Later phases add the packages listed in [DESIGN §19](docs/DESIGN.md#19-project-structure).

## License

TBD.
