# Switchyard: Cost-Aware Inference Gateway

**Architecture & Design Document**

| | |
|---|---|
| Status | Draft for review. No implementation code has been written. |
| Version | 0.1, 2026-11-09 |
| Companion | [PLAN.md](PLAN.md): phases, milestones and acceptance criteria |
| Working name | *Switchyard*, after the rail yard where cars are sorted onto the right track. The name appears in the binary, the `X-Switchyard-*` headers, `SWITCHYARD_*` env vars and the `sy_` key prefix, so rename it before Phase 0 if you'd prefer another. |

> **Where to focus your review.** The decisions that matter most are the language (§8.1), the routing semantics and the default for model substitution (§9, §9.6), the quality model (§10), the semantic-cache defaults (§11.3) and how savings are defined (§12.4). Open questions are collected in §23.

**Contents:** 1 Summary · 2 Goals, non-goals, assumptions · 3 Terminology · 4 High-level architecture · 5 API surface · 6 Core components · 7 Request data flow · 8 Tech stack · 9 Routing decision logic · 10 Quality scoring · 11 Caching strategy · 12 Cost tracking, budgets & savings · 13 Policies & rule engine · 14 Reliability · 15 Observability · 16 Configuration · 17 Security · 18 Deployment: self-hosted & SaaS · 19 Project structure · 20 Testing strategy · 21 Decision log · 22 Risks · 23 Open questions

---

## 1. Summary

Switchyard is an OpenAI-compatible HTTP gateway that sits between applications and LLM providers. For each request it:

1. works out what the request needs: capabilities, context size, and how hard it looks;
2. picks the **cheapest deployment** (a model on a particular provider) that meets the caller's quality, latency, compliance and budget constraints;
3. serves the response from **cache** when that is safe;
4. **retries and fails over** when a provider misbehaves;
5. records what the request **cost** and what it **would have cost** without the gateway.

Customers switch by changing `base_url` and the API key. Nothing else in their code changes.

**Key decisions** (rationale and alternatives are in §21):

- **Go.** One static binary running as stateless replicas. Python/FastAPI is the runner-up (§8.1).
- **The OpenAI Chat Completions schema is the canonical internal format.** Each provider gets an adapter that translates to and from it.
- **Routing is a transparent constrained optimization, not a black box.** Hard filters are applied first, then quality, cost and latency are estimated, the cheapest eligible deployment is chosen, and a fallback chain is attached. Every decision can be explained, including through `POST /switchyard/v1/route/explain`.
- **Quality v1 combines per-model quality priors with a heuristic estimate of request difficulty.** It sits behind an interface that a learned router can replace later.
- **Model substitution is opt-in.** A concrete model name is honored, and Switchyard picks the cheapest *provider* for that model. Routing across models happens for `model: "auto…"` or when a policy enables `equivalent` substitution. An **observe mode** reports projected savings without changing any response.
- **Redis holds caches and hot counters** in two roles: an evictable *cache* and a non-evictable *state*. **Postgres is the usage ledger.** In-memory implementations serve tests and a zero-dependency quickstart.
- **Exact caching is opt-in per policy. Semantic caching is off by default.** When enabled, it is partitioned by an exact fingerprint of the conversation context, uses a high similarity threshold, and fails open.
- **Retries and fallbacks happen only before the first byte reaches the client.**

## 2. Goals, non-goals, assumptions

**Goals (MVP)**

| # | Goal |
|---|---|
| G1 | Drop-in OpenAI compatibility. Official SDKs work unchanged for chat, streaming, tool calls and JSON mode; only `base_url` and the key differ. |
| G2 | Route across OpenAI, Anthropic, Google, OpenAI-compatible hosts (Together, Fireworks, Groq and others) and self-hosted servers (vLLM, Ollama, TGI). |
| G3 | Exact and semantic caching with strict tenant isolation. |
| G4 | Per-request cost, budgets, and savings reporting. |
| G5 | Policy rules: allow/deny lists, preferences, quality thresholds, budgets and per-request caps. |
| G6 | Logs, metrics and traces good enough to operate the gateway *and* to trust its decisions. |
| G7 | Retries, fallback and circuit breaking. |
| G8 | Configuration-driven: no model, price or provider is hard-coded. |
| G9 | Easy to self-host, and structured so it can become multi-tenant SaaS without a rewrite. |

**Non-goals for the MVP:** the Responses, Assistants, Batch, Realtime, audio, image-generation and fine-tuning endpoints · a web UI (Grafana and JSON APIs instead) · ML routing in the hot path (the interface is ready, §10.7) · billing and invoicing · multi-region active-active · the SaaS control plane (only its seams, §18) · content moderation and guardrails.

**Assumptions**

- A trusted operator writes the config. Tenants don't supply provider URLs in the MVP.
- Provider-reported token usage is the source of truth for cost.
- MVP scale: one region, up to about 2k requests/s per replica, tens of tenants, and up to about 200 deployments in the catalog.
- Request heuristics are tuned for English first.

**Gateway SLOs.** These are measured against a zero-latency fake upstream, so they cover only what Switchyard itself adds.

| SLI | Target |
|---|---|
| Added latency on the non-cached path, without semantic lookup, for requests ≤ 32 KB | p50 ≤ 2 ms · p99 ≤ 10 ms |
| Semantic-cache lookup (embedding plus vector search) | ≤ 50 ms with a local embedder, ≤ 150 ms remote; a timeout counts as a miss |
| Exact-cache hit, end to end | p99 ≤ 5 ms with Redis on the same network |
| Errors caused by the gateway itself | < 0.01 % of requests |
| Availability | If at least one eligible deployment is healthy, the request succeeds. |
| Cost accuracy | Ledger cost = provider-reported usage × the price snapshotted at request time, exact to 1e-9 USD. |

## 3. Terminology

| Term | Meaning |
|---|---|
| **Provider** | An upstream endpoint plus its credentials, for example `openai`, `anthropic`, `together` or `gpu-cluster`. |
| **Model** | A logical model in the catalog, with capabilities and quality priors, for example `llama-3.3-70b`. |
| **Deployment** | A model served by one provider, with its own price, limits and health. Its ID is `model@provider`, as in `llama-3.3-70b@together`. Routing chooses between deployments. |
| **Catalog** | The set of models and deployments. It is config data. |
| **Profile** | A routing objective plus constraints, selected with `model: "auto:<profile>"`. Examples: `cheap`, `balanced`, `quality`, `fast`. |
| **Policy** | Governance attached to a tenant or key: allowed models, budgets, caching, substitution mode and rules. |
| **Principal** | An authenticated caller: its tenant, key, policy and tags. |
| **Plan** | The ordered deployments to try for one request: a primary plus fallbacks. |
| **Commit point** | The moment the first response byte goes to the client. After it, a retry is no longer possible. |
| **Baseline** | The model the request would have used without Switchyard. Savings are measured against it. |

## 4. High-level architecture

```
          Applications (OpenAI SDKs, LangChain, curl, …)
                        │  OpenAI wire format · Authorization: Bearer sy_…
                        ▼
          Load balancer / ingress (TLS)
                        │
┌───────────────────────▼──────────────────────┐         ┌──────────────────────────┐
│  Switchyard replicas · stateless · N ≥ 1     │──OTLP──►│ OpenTelemetry collector  │
│  :8080 public API   ·   :9090 admin/metrics  │◄─scrape─│ Prometheus · Grafana     │
└─────┬────────────┬────────────┬───────────┬──┘         └──────────────────────────┘
      │            │            │           │
      ▼            ▼            ▼           ▼
 Redis cache  Redis state  Postgres    Upstream providers (HTTPS, pooled)
 exact cache  budgets      usage       OpenAI · Anthropic · Google Gemini
 + vectors    rate limits  ledger      Together · Fireworks · Groq · …
 allkeys-lru  noeviction   reports     vLLM · Ollama · TGI (self-hosted)
```

Inside a replica, each request runs through the pipeline in §7, built from the components in §6.

**Design principles**

1. **Stateless data plane.** All shared state lives in Redis or Postgres, so replicas scale horizontally and deploys can roll.
2. **The hot path waits only on what it must:** in-memory auth, the Redis cache, Redis budget admission and the upstream call. Ledger writes, cache writes and statistics updates happen asynchronously.
3. **Every external boundary is an interface:** providers, stores, key and config sources, and the quality estimator. Tests use in-memory fakes, and a SaaS build can swap implementations later.
4. **Decisions are pure functions** of the request and snapshots of the policy, catalog and health state. That makes them testable, explainable and reproducible.
5. **Optimizations fail open; money and security fail closed.** Cache, semantic lookup and latency statistics fail open. Auth, hard budget caps and per-request cost caps fail closed.
6. **Config snapshots are immutable** and are swapped atomically. A request runs start to finish on the snapshot it began with.

## 5. API surface

**Public listener** (default `:8080`, API-key auth)

| Endpoint | Phase | Notes |
|---|---|---|
| `POST /v1/chat/completions` | P1 | Streaming and non-streaming, tools, `response_format`, vision inputs. |
| `GET /v1/models` | P1 | The catalog models and virtual models (`auto:*` and aliases) this key may use. |
| `POST /v1/completions` | P2 | Legacy. Served natively where the deployment supports it, otherwise through a chat shim, which is flagged in the `X-Switchyard-Shim` header. |
| `POST /v1/embeddings` | P2 | Picks the cheapest *provider* for the requested model and never switches models, because vectors from different models aren't interchangeable. |
| `GET /switchyard/v1/usage` | P4 | Cost and savings report for the caller's own tenant. |
| `POST /switchyard/v1/route/explain` | P5 | Dry run. Returns the routing decision and its reasons without calling any upstream. |
| `GET /healthz`, `GET /readyz` | P0 | Liveness, and readiness with dependency checks and the config version. |

Gateway-specific endpoints sit under `/switchyard/v1/…` so they can't collide with future OpenAI paths.

**Admin listener** (default `127.0.0.1:9090`, admin token): `/metrics`, `/admin/v1/usage`, `/admin/v1/providers` (health, breakers, latency stats), `/admin/v1/config` (effective config with secrets redacted), `/admin/v1/reload`, `/admin/v1/cache/purge`.

**How the `model` field is resolved**

| `model` value | Meaning |
|---|---|
| A catalog ID, e.g. `gpt-5-mini` | That model, on its cheapest healthy deployment. A different model is used only if the policy enables `equivalent` substitution (§9.6). |
| `model@provider`, e.g. `llama-3.3-70b@together` | Pins both the model and the provider. Useful for debugging and benchmarks. |
| `auto` or `auto:<profile>` | Routes across every model the policy allows, under the profile's constraints. Plain `auto` uses the policy's default profile. |
| An alias, e.g. `code-review` | An operator-defined name for any of the above. |
| Anything else | `404 model_not_found`, in OpenAI's error format. |

**Request headers** (all optional). A header can choose among options the policy allows and can tighten its limits. It can never exceed them.

| Header | Effect |
|---|---|
| `X-Switchyard-Profile` | Selects a profile, if the policy allows it. |
| `X-Switchyard-Max-Cost-Usd` | Lowers the per-request cost cap. |
| `X-Switchyard-Min-Quality` | Raises the quality floor. |
| `X-Switchyard-Max-TTFT-Ms`, `X-Switchyard-Timeout-Ms` | Tighten the time-to-first-token target or the deadline. |
| `X-Switchyard-Task`, `X-Switchyard-Difficulty` | Hints for the quality assessor (§10.6). |
| `X-Switchyard-Cache` | Per-request cache control: `off`, `no-store` or `exact-only`. |
| `X-Switchyard-Tags` | Cost-attribution tags such as `team=search,feature=autocomplete`, stored in the ledger. Count and length are bounded. |

**Response headers:** `X-Request-Id` · `X-Switchyard-Model` (the catalog model that served the request) · `X-Switchyard-Provider` · `X-Switchyard-Profile` · `X-Switchyard-Route-Reason` (for example `cheapest_eligible`, `pinned`, `fallback:upstream_503` or `quality_relaxed`) · `X-Switchyard-Cache` (`miss`, `exact` or `semantic`) · `X-Switchyard-Attempts` · `X-Switchyard-Cost-Usd` · `X-Switchyard-Upstream-Request-Id`. The cost header is sent only for non-streaming responses, because a stream's cost isn't known until after its headers have gone out; streaming costs are available from the usage API. The body's `model` field carries the upstream's own model string, so it names what actually answered.

**Errors** use OpenAI's envelope, `{"error": {"message", "type", "param", "code"}}`, with `type` following OpenAI's conventions.

| Situation | Status | `code` |
|---|---|---|
| Missing or invalid gateway key | 401 | `invalid_api_key` |
| Gateway rate limit hit | 429 | `rate_limit_exceeded` |
| Budget exhausted (`on_exhausted: block`) | 429 | `budget_exceeded`, with type `insufficient_quota` as in OpenAI's quota errors and `Retry-After` set to when the window resets |
| No deployment satisfies the hard constraints | 400 | `no_eligible_model`; the message names the constraints that ruled everything out |
| Unknown model | 404 | `model_not_found` |
| Non-retryable upstream 4xx, such as a context too long for every candidate | the upstream's status | the upstream's code where it maps, e.g. `context_length_exceeded` |
| Every attempt failed | 502, 503 if every circuit is open, 504 at the deadline | `upstream_error`, `no_healthy_upstream`, `timeout` |

## 6. Core components

Dependencies point downward:

```
transport        server · api/openaiapi · api/syapi · api/adminapi
application      gateway          per-endpoint pipeline; the only code that knows the order of steps
domain           policy · quality · router · executor · cache · budget · ratelimit · usage · catalog · health
infrastructure   providers/{openai,anthropic} · store/{redis,postgres,memory} · sse · telemetry
shared kernel    schema           canonical OpenAI-shaped types, used by every layer
composition      app              builds every component from a config snapshot; no globals
```

Domain packages own their interfaces, adapters and stores implement them, and only `app` imports the concrete implementations.

| Component | Responsibility |
|---|---|
| `server` | Listeners and middleware: request ID, trace extraction, panic recovery, body and header limits, access log, graceful shutdown. |
| `schema` | Lossless Chat Completions request, response and chunk types. Unknown fields are kept for pass-through. Also validation, and extracting the capabilities a request needs. |
| `api/*` | HTTP handlers, the OpenAI error envelope, and request-header parsing. |
| `gateway` | Orchestrates the pipeline for each endpoint. |
| `auth` | Turns an API key into a `Principal`. Also handles the admin token, with constant-time comparison. |
| `config` | Loads config, expands secret references, validates, builds the immutable `Snapshot`, and hot-reloads. |
| `catalog` | Models, deployments, prices, capabilities, quality priors, aliases, and lookup indices over them. |
| `policy` | Builds the `EffectivePolicy`: the base policy, then the first matching rule, then header tightening. |
| `quality` | The `Assessor` (category, difficulty, signals) and the `Estimator` (a quality estimate per candidate). |
| `router` | Builds the candidate set, applies hard filters, computes estimates, ranks, and produces a `Plan` with an explanation. |
| `executor` | Runs the attempt loop over a plan: timeouts, retries with backoff, the retry budget, breakers, concurrency limits, and the stream commit point. |
| `providers` | One adapter per wire protocol. `openai` covers every OpenAI-compatible upstream; `anthropic` speaks the Messages API. Adapters normalize errors and usage. |
| `sse` | A robust SSE reader and writer, an aggregator that turns chunks into a full response, and replay that turns a full response back into chunks. |
| `cache` | The exact and semantic caches, key derivation, `Embedder` and `VectorIndex`. |
| `budget` | Atomic admission of estimated cost, then settlement with the actual cost. |
| `ratelimit` | Per-key requests-per-minute and concurrent-stream caps, using GCRA in Redis. |
| `usage` | Cost calculation, usage events, the asynchronous batched sink, reporting queries and savings. |
| `health` | Per-deployment latency statistics (TTFT and throughput), the breaker registry, and in-flight counts. |
| `telemetry` | `slog` setup, the Prometheus registry and metrics, and the OpenTelemetry tracer. |

### 6.1 Contract sketch

Illustrative signatures, not final code:

```go
// providers: one implementation per wire protocol.
type Adapter interface {
    Chat(ctx context.Context, d catalog.Deployment, req *schema.ChatRequest) (*schema.ChatResponse, error)
    ChatStream(ctx context.Context, d catalog.Deployment, req *schema.ChatRequest) (Stream, error)
    Embed(ctx context.Context, d catalog.Deployment, req *schema.EmbeddingRequest) (*schema.EmbeddingResponse, error)
}
type Stream interface {
    Next() (*schema.ChatChunk, error) // io.EOF at the end
    Usage() usage.Tokens              // complete once the stream has ended
    Close() error
}
// Every adapter error is a *providers.Error{Kind, Status, RetryAfter, Provider, Message}.

// quality: heuristic now, learned later.
type Assessor interface {
    Assess(req *schema.ChatRequest, h Hints) Assessment // category, difficulty, signals
}
type Estimator interface {
    Estimate(ctx context.Context, a Assessment, candidates []catalog.Deployment) []float64
}

// router
type Router interface {
    Plan(ctx context.Context, in Input) (Plan, error) // ordered attempts plus an explanation
}

// cache
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}
type VectorIndex interface {
    Search(ctx context.Context, partition string, v []float32) (hit Hit, found bool, err error)
    Put(ctx context.Context, partition string, v []float32, entryKey string, ttl time.Duration) error
}

// Seams that let a SaaS build swap implementations without touching the pipeline.
type ConfigSource interface {
    Load(ctx context.Context) (*config.Snapshot, error)
    Watch(ctx context.Context) <-chan struct{}
}
type KeyStore interface {
    Lookup(ctx context.Context, keyHash [32]byte) (auth.Principal, error)
}
type CredentialResolver interface {
    Resolve(ctx context.Context, tenant, provider string) (Credential, error)
}
type UsageSink interface {
    Record(ev usage.Event) // never blocks; batches internally
}
```

### 6.2 Provider adapters

**`openai` adapter.** It serves OpenAI and every OpenAI-compatible upstream: Together, Fireworks, Groq, vLLM, Ollama, TGI, and Gemini through Google's OpenAI-compatible endpoint. These upstreams differ in what they support, such as usage reporting in streams, parallel tool calls, `json_schema`, `seed` and `logprobs`. Each deployment declares those differences as capabilities in the catalog, so they live in data rather than in code branches.

**`anthropic` adapter (Messages API).** Anthropic positions its OpenAI-compatible endpoint for testing and comparison, not production, and that endpoint doesn't expose every feature, so the MVP speaks the native Messages API.

| OpenAI (canonical) | Anthropic Messages API |
|---|---|
| `system` and `developer` messages | The top-level `system` field, concatenated in order |
| Consecutive messages with the same role | Merged into one message's content blocks |
| `max_tokens` / `max_completion_tokens` | `max_tokens`, which Anthropic requires; defaults to the policy cap or the model maximum |
| `stop` | `stop_sequences` |
| `temperature` (0–2), `top_p` | `temperature` (0–1; a value above 1 makes the deployment ineligible), `top_p` |
| `tools[].function{name, description, parameters}` | `tools[]{name, description, input_schema}` |
| `tool_choice`: `auto`, `required`, `none`, `{function}` | `{type: auto}`, `{type: any}`, `{type: none}`, `{type: tool, name}` |
| Assistant `tool_calls[]`, with arguments as a JSON string | `tool_use` blocks, with input as a JSON object |
| `tool` role messages | A user message with `tool_result` blocks, linked by `tool_use_id` |
| `image_url` parts, as a data URL or https URL | `image` blocks with a base64 or URL source |
| `response_format: json_schema` | Native structured output where the catalog says the model supports it; otherwise a single forced tool whose input is returned as the content |
| `user` | `metadata.user_id` |
| `n > 1`, `logprobs`, `seed`, frequency and presence penalties | Unsupported. The deployment becomes ineligible, or the parameter is dropped if `drop_unsupported_params` is set. |
| `finish_reason`: `stop`, `length`, `tool_calls`, `content_filter` | `stop_reason`: `end_turn` or `stop_sequence`, `max_tokens`, `tool_use`, `refusal` |
| `usage.prompt_tokens`, `prompt_tokens_details.cached_tokens`, `completion_tokens` | The sum of `input_tokens`, `cache_read_input_tokens` and `cache_creation_input_tokens`; `cache_read_input_tokens`; `output_tokens` |
| `chat.completion.chunk` deltas for content and `tool_calls[i].function.arguments`, a finish chunk and a usage chunk | `message_start`, `content_block_start/delta/stop` (`text_delta`, `input_json_delta`), `message_delta`, `message_stop`. `ping` is dropped or becomes a keep-alive comment; `error` becomes an error event. |
| Retryable errors | 429 rate limited, 529 overloaded, 5xx |

**Gemini** goes through Google's OpenAI-compatible endpoint in the MVP. A native adapter is a post-MVP item, built only if that endpoint's gaps start to matter, for example in reporting cached tokens or in safety settings.

## 7. Request data flow

```
 #  stage            what happens                                              exits with
 1  ingress          request id, trace context, header and body limits         413
 2  authenticate     Bearer key → SHA-256 → Principal (in-memory index)        401
 3  rate limit       per-key GCRA + concurrent-stream cap (Redis state)        429
 4  decode           JSON → ChatRequest; validate; required capabilities       400
 5  assess           category, difficulty, signals (local, microseconds)
 6  resolve policy   base ⊕ first matching rule ⊕ header tightening
 7  resolve target   model field → pinned | equivalent | auto:<profile>        404
 8  cache lookup     exact (GET), then semantic (embed + KNN, ≤ 150 ms)        hit → 12
 9  route            candidates → hard filters → estimates → ranked plan       400
10  admit            reserve estimated cost against budgets (atomic)           429
11  execute          attempt loop: adapter call; retry or fall back by error   502 · 503 · 504
12  respond          OpenAI-shaped JSON or SSE stream + X-Switchyard-* headers
13  post-process     async: settle budget · ledger event · cache write ·
                     metrics · latency stats · end span
```

**Streaming specifics**

- Steps 1–10 are the same. In step 11, the executor **waits for the first upstream event before committing**, that is, before sending `200` and the SSE headers. Until then, failures are retried or failed over exactly as for non-streaming requests.
- **After the commit point there are no retries.** An upstream failure is sent as `data: {"error": {…}}`, which the OpenAI SDKs raise as an API error, and the stream closes.
- Each upstream event is translated into a `chat.completion.chunk` and flushed immediately. A tee also feeds the aggregator, which drives usage, cost and the cache write.
- Switchyard **always asks the upstream for usage**; for OpenAI-wire upstreams that means `stream_options.include_usage`. The usage chunk is forwarded only if the client asked for it.
- **A client disconnect cancels the upstream request**, so the customer doesn't pay for tokens nobody will read. Usage is then estimated from the aggregated text and flagged `estimated`, and nothing is cached.
- Optionally, Switchyard sends SSE comment keep-alives (`: ping`) while it waits for a slow first token, for intermediaries with short idle timeouts. This is off by default.
- A **cache hit on a streaming request** is replayed as a minimal valid stream: a role chunk, a content or tool-call chunk, a finish chunk, an optional usage chunk, then `[DONE]`.

**What each upstream outcome does**

| Upstream outcome | Behavior |
|---|---|
| Connection error, 5xx, 529 overloaded, or TTFT timeout | Retry once with jittered backoff, then move to the next candidate. |
| 429 with a `Retry-After` that fits within the deadline | Wait and retry the same deployment; otherwise go to the next candidate. The deployment also cools down briefly. |
| 400 or 422 invalid request | Returned to the client unchanged, with no retry and no fallback; another provider would reject it too. |
| 401 or 403 on *our* provider credential | Move to the next candidate, trip the breaker, and alert, since this is a config problem. |
| 404 model, or context length exceeded | Move to the next candidate, which may have a larger context window. |
| Content filter or refusal | Returned unchanged. Switchyard **never shops a refused request around** to a more permissive provider. |

## 8. Tech stack

### 8.1 Language: Go (recommended) vs Python/FastAPI

| Criterion | Go | Python (FastAPI) |
|---|---|---|
| Hot-path overhead and tail latency | Low and predictable. There's no GIL, so JSON and SSE translation run on all cores. | Good with asyncio and uvloop, but CPU work on the event loop (JSON, SSE translation, hashing) inflates p99. Using several cores means running several processes. |
| Thousands of long-lived streams | A goroutine costs a few KB, and `context` cancellation propagates naturally to upstream calls. | Works with asyncio, but cancellation and cleanup are easier to get wrong. |
| Self-hosting and operations | One static binary in an image of roughly 30 MB, with no runtime to install. | An interpreter plus dependencies, larger images, and worker tuning. |
| Provider SDKs | Not needed. Speaking HTTP directly gives full control of retries, timeouts and streaming. | Official SDKs exist, but a gateway would bypass their retry logic anyway. |
| ML ecosystem (embeddings, classifiers) | Thin, so models are called over HTTP. | Rich, and models can run in-process. |
| MVP velocity | Somewhat slower | Faster |
| Precedent | Performance-focused gateways are mostly Go or Rust, such as Bifrost (Go) and TensorZero (Rust). | LiteLLM, the most widely used open-source gateway, is Python. |

**Decision: Go.** Switchyard sits in the hot path of every LLM call, so customers will judge it on added latency, streaming robustness, memory per connection and how easy it is to run, and those are Go's strengths. Python's real advantage is ML, and the design keeps that door open. Embeddings and learned quality scoring sit behind HTTP-shaped interfaces (`Embedder`, `Estimator`), so a Python sidecar can be added if routing ever needs heavyweight models. The offline evaluation tooling in Phase 8 runs outside the hot path and could even be written in Python.

Python would be the better choice if the team were Python-only, load were modest, and in-process ML routing were the main differentiator. Rust has the best raw performance, but iteration is slower and fewer people can contribute; Go is fast enough when the upstream call takes 100 to 10,000 times longer than the gateway's own work. TypeScript (Node or Bun) suits edge runtimes, but it shares Python's single-event-loop CPU caveat, and deploying it is harder than deploying a static binary.

### 8.2 Libraries and tools

| Concern | Choice | Notes |
|---|---|---|
| Language | Go. go.mod sets the oldest supported release as the minimum (`go 1.26.0`); development and CI use the current stable release (1.27). | Built with `CGO_ENABLED=0` for a static binary. `go vet` rejects standard-library APIs newer than the go.mod minimum. |
| HTTP server | stdlib `net/http` with method and path patterns | No framework needed; middleware are small functions. |
| Upstream HTTP | stdlib client, with one tuned `Transport` per provider | Keep-alive pools, HTTP/2 and per-provider timeouts. |
| JSON | `encoding/json`, plus `json.RawMessage` for pass-through fields | Profile before switching to a faster codec. |
| SSE | A small in-house reader and writer | Fuzz-tested. Handles `event:` lines (used by Anthropic), comments, CRLF and multi-line data. |
| Config | YAML via `go.yaml.in/yaml/v3` with `KnownFields(true)`, our own `${…}` expansion, and hand-written validation | Errors name the exact path, e.g. `models[3].deployments[0].pricing.input: must be ≥ 0`. |
| Cache and state store | Redis 8 via `redis/go-redis/v9` | Redis 8 bundles the query engine used for vector search. Valkey works for the exact cache and counters; for vectors it needs a search module or another `VectorIndex` backend. |
| Ledger | Postgres ≥ 15 via `jackc/pgx/v5`, with `pressly/goose` migrations embedded in the binary | |
| Logging | stdlib `log/slog` with the JSON handler | |
| Metrics | `prometheus/client_golang` | |
| Tracing | OpenTelemetry Go SDK with the OTLP exporter | Vendor-neutral: works with Jaeger, Tempo, Honeycomb, Datadog, Langfuse and others. |
| Circuit breaker | `sony/gobreaker/v2` | Small and proven. |
| Request IDs | UUIDv7 via `google/uuid` | Time-ordered, which keeps ledger indexes compact. |
| Embeddings for the semantic cache | Any embedding deployment in the catalog | OpenAI's embeddings API, or a self-hosted TEI or Ollama for operators who don't want prompts to leave their network. |
| Tests | stdlib `testing`, `go-cmp`, `rapid` for property tests, `miniredis` (which runs Lua), `testcontainers-go`, and the official `openai-go` SDK for conformance | |
| Build and release | Make, `golangci-lint`, `govulncheck`, GoReleaser, a distroless static non-root image, and an SBOM | |

## 9. Routing decision logic

### 9.1 Inputs

- **Request features.** Estimated input tokens: bytes ÷ 4 plus per-message overhead, adjusted by a per-model `token_ratio` from the catalog. Expected output tokens: `min(max_tokens, routing.expected_output_tokens)`, which defaults to 500. The capabilities the request requires: tools, vision, `json_schema`, `n > 1`, logprobs, seed, temperature range, reasoning parameters. The assessment (category and difficulty) and any client hints.
- **Effective policy.** Allow and deny lists, preferences, required tags, profile, substitution mode, cost cap and budget state.
- **Catalog.** Deployments with their prices, context windows, capabilities, quality priors, tags and latency hints.
- **Live state (per replica).** Breaker state, latency EWMAs, and in-flight requests against `max_concurrency`. Remaining budget comes from Redis.

### 9.2 Algorithm

```
1 candidates    pinned       → deployments of the requested model (or the single pinned deployment)
                equivalent   → every allowed model; quality bar = q̂(requested model) − max_quality_drop
                auto:<prof>  → every allowed model; quality bar = profile.min_quality
2 hard filters  never relaxed; every exclusion is recorded with its reason
                · allow/deny, required tags (region, zdr, self_hosted)
                · capabilities and parameter support
                · context_window ≥ (est_input + expected_output) × 1.1
                · breaker not open; in-flight < max_concurrency
                · est_cost ≤ min(max_cost_per_request, remaining budget)
3 estimates     q̂ (§10.4), est_cost, est_ttft, est_duration
4 soft checks   q̂ ≥ quality bar; latency ≤ profile/header targets
                if nothing passes: relax latency first, then quality (take the best available),
                unless the policy says on_unmet: reject; record the relaxation
5 rank          preference tier → objective (cost or latency) → the other metric → config order
6 plan          primary + up to N−1 fallbacks (N = 3 by default); each fallback slot prefers
                a provider that is not already in the plan
```

- **Cost estimate:** `est_cost = est_input × input_price + expected_output × output_price`. The estimate ignores provider-side prompt caching, which can only lower the cost; settlement counts it.
- **Latency estimates:** `est_ttft` is the EWMA of observed TTFT, and `est_duration = est_ttft + expected_output ÷ EWMA(tokens per second)`. Both come from the catalog's `latency_hint` until a deployment has at least 20 samples. Statistics are kept per replica, which converges quickly and avoids chatter between replicas.
- **Hard vs soft constraints.** Money, compliance and capability limits are never relaxed. Quality and latency targets fall back to best effort by default, so a request isn't failed just because a heuristic thinks it's hard. Any relaxation is visible in headers, logs and metrics.
- **Determinism.** The same inputs and snapshots always produce the same plan. That's what makes the explain endpoint and the tests trustworthy.

### 9.3 Profiles

A profile is a set of constraints plus an objective. Clients pick one with `model: "auto:<profile>"` or the profile header.

| Profile | `min_quality` | Objective | Typical use |
|---|---|---|---|
| `cheap` | 0.65 | cost | Bulk classification, extraction, drafts |
| `balanced` (default) | 0.80 | cost | General application traffic |
| `quality` | 0.92 | cost | User-facing answers where mistakes are expensive |
| `fast` | 0.80 | latency (TTFT) | Interactive UX |

### 9.4 Preferences and spillover

- A policy's `prefer` list, e.g. `["claude-*", "llama-*"]`, puts matching deployments in earlier tiers, and cost decides within each tier.
- A self-hosted deployment priced at its amortized cost and given a `max_concurrency` yields a **spillover** pattern with no extra code: use our own GPUs until they're saturated, then the cheapest paid provider.

### 9.5 Budget-aware routing

The router knows the remaining budget. Candidates that would exceed it are removed by a hard filter. Once spending passes a budget's `soft_at` threshold, the policy switches to that budget's `soft_profile` (for example `cheap`), and the explanation says so.

### 9.6 Model substitution for concrete model names

| Mode | Behavior | Default |
|---|---|---|
| `pinned` | Only deployments of the requested model are considered. Savings come from provider arbitrage, caching and fallback. | ✓ |
| `equivalent` | Any allowed model qualifies if its estimated quality *for this request* is at least the requested model's estimate minus `max_quality_drop` (default 0.05). This is how a customer who keeps sending `gpt-…` names still gets savings. | opt-in |

There is also **`routing_mode: observe`**, which works with either mode or with `auto`. Switchyard serves exactly what was asked for (the baseline model for `auto`), computes the decision it *would* have made, and records the projected savings. It is the zero-risk way to onboard.

### 9.7 Explainability

Every decision produces an `Explanation`. A summary goes into the response headers and the access log, the full explanation is attached to the trace, and `POST /switchyard/v1/route/explain` returns it without calling an upstream (it takes the same request body as chat completions). Here is the output for the second request in §10.5, with the `code-needs-quality` rule from §16.3 matching:

```json
{
  "policy": "default",
  "matched_rule": "code-needs-quality",
  "profile": "balanced",
  "assessment": { "category": "coding", "difficulty": 0.55, "signals": ["code_block", "stack_trace"] },
  "estimates": { "input_tokens": 1500, "output_tokens": 500 },
  "quality_bar": 0.85,
  "candidates": [
    { "deployment": "small@together",  "q": 0.78, "est_cost_usd": 0.00035, "eligible": false, "excluded_by": "quality 0.78 < 0.85" },
    { "deployment": "medium@openai",   "q": 0.86, "est_cost_usd": 0.00175, "eligible": true,  "rank": 1 },
    { "deployment": "large@anthropic", "q": 0.97, "est_cost_usd": 0.01200, "eligible": true,  "rank": 2 }
  ],
  "plan": ["medium@openai", "large@anthropic"],
  "relaxed": []
}
```

## 10. Quality scoring

### 10.1 What is being estimated

For each request, the question is whether a cheaper model is *good enough*. Version 1 has to be simple, explainable, tunable without code changes, and replaceable.

### 10.2 Model quality priors

Each model has a prior `Q(m, c)` between 0 and 1 for each category `c`.

- **Definition.** `Q(m, c)` is the approximate probability that model `m` gives an acceptable answer to a *hard* request in category `c`. Defining it this way means it can be measured (§10.7).
- **Categories in v1:** `general`, `coding`, `reasoning` (math and logic), `extraction` (structured output and classification), `creative`, `tool_use`. A category with no prior falls back to `general`.
- **Seeding.** The priors are set by hand from public benchmarks and leaderboards, normalized, and labeled as starting estimates in the catalog. Operators can override them, and Phase 8 replaces them with measurements taken on the operator's own prompts.
- A per-deployment `quality_offset` covers hosted variants of a model, such as a quantized version.

### 10.3 Request assessment (heuristic `Assessor`)

| Signal | What it looks for | Effect |
|---|---|---|
| Length | Estimated input tokens, log-scaled | Raises difficulty |
| Code | Fenced code, stack traces, dense code-like tokens | Category `coding`; raises difficulty |
| Math and logic | LaTeX, equations, "prove", "derive" | Category `reasoning`; raises difficulty |
| Reasoning cues | "step by step", "trade-offs", "design", "analyze why" | Raises difficulty |
| Constraints | Number of explicit instructions or bulleted requirements | Raises difficulty |
| Conversation depth | Number of turns | Raises difficulty slightly |
| Tools | Tool definitions, or `tool_choice` set to required | Category `tool_use` |
| Strict schema | `response_format: json_schema` | Category `extraction` |
| Simple-task cues | Translate, classify, extract, "one word", short input | Lowers difficulty |

Difficulty is `d = clamp(Σ wᵢ·fᵢ, 0, 1)`, where each feature `fᵢ` is between 0 and 1 and the weights `wᵢ` live in config. The category is the strongest category signal, or `general` if none fires. The assessment also returns the signals that fired, for use in explanations. It makes no network calls and takes microseconds.

### 10.4 The estimate

```
q̂(m, r) = 1 − d(r) · (1 − Q(m, c(r)))      (plus the deployment's quality_offset, clamped to [0, 1])
```

- For a trivial request (`d` near 0), every model scores close to 1, so the cheapest one wins.
- For the hardest requests (`d` near 1), `q̂` approaches `Q`, so only strong models clear the bar.
- In between, the estimate interpolates linearly. This is crude, but it moves in the right direction, is easy to explain, and can be calibrated.

A deployment is eligible on quality when `q̂ ≥ bar`, where the bar comes from the profile, a rule, a header, or the relative bar used by `equivalent` substitution (§9.6).

### 10.5 Worked example

Coding category, `balanced` profile (bar 0.80), with each request assumed to have 1,500 input and 500 output tokens to keep the arithmetic simple. Prices are illustrative.

| Model | `Q(coding)` | $ per 1M tokens, input · output | Estimated cost |
|---|---|---|---|
| S | 0.60 | 0.10 · 0.40 | $0.00035 |
| M | 0.75 | 0.50 · 2.00 | $0.00175 |
| L | 0.95 | 3.00 · 15.00 | $0.01200 |

| Request | d | q̂ S | q̂ M | q̂ L | Routed to |
|---|---|---|---|---|---|
| "Rename `tmp` to `total` in this 10-line function" | 0.20 | 0.92 ✓ | 0.95 ✓ | 0.99 ✓ | S |
| "Fix the failing test", plus a stack trace and 300 lines of code | 0.55 | 0.78 ✗ | 0.86 ✓ | 0.97 ✓ | M |
| "Design a lock-free MPMC queue and argue that it's correct" | 0.90 | 0.64 ✗ | 0.78 ✗ | 0.96 ✓ | L |

Together, the three requests cost about 61 % less than sending all of them to L ($0.0141 instead of $0.036).

### 10.6 Client hints

`X-Switchyard-Task: coding` and `X-Switchyard-Difficulty` (`low`, `medium`, `high`, or a number from 0 to 1) override the heuristic, since the caller often knows best. Rules can also raise or lower quality bars for specific traffic, for example everything tagged `feature=autocomplete`.

### 10.7 How it improves over time

Each step below plugs into the same interfaces.

1. **Calibrate (Phase 8).** `switchyard eval` runs prompt sets against the candidate models. An LLM judge scores each answer pairwise against a reference model. The output is a per-category `Q` with confidence intervals, written as catalog overrides. The same data fits the heuristic's weights to labeled prompts.
2. **Shadow sampling** (opt-in, sampled, budget-capped). A small percentage of live traffic is also sent to the baseline model in the background and judged offline. This measures routed quality against baseline quality, per profile.
3. **Feedback endpoint.** `POST /switchyard/v1/feedback` with a request ID and a score stores the score in the ledger, where it can be joined with routing decisions.
4. **Learned estimator.** For example, a RouteLLM-style classifier served as an HTTP sidecar that implements `Estimator`.
5. **Cascades** in the style of FrugalGPT: try the cheap model, check the answer, and escalate if needed. Only for non-streaming traffic that can tolerate the extra latency.
6. **LLM-based assessor**, used only when it pays for itself, that is, when the price gap between candidates multiplied by the token count is much larger than the assessor's own cost.

### 10.8 Known limitations

Heuristics can be fooled by phrasing, they're English-centric for now, and the priors are estimates rather than guarantees. That's why the defaults are conservative: substitution is `pinned`, observe mode exists, relaxations show up in metrics, and the explain endpoint is always available, with the Phase 8 feedback loop as the long-term fix. Prompt content affects only the quality estimate, never authorization, allow lists, budgets or caps. Inflating difficulty to reach expensive models is capped by the policy's cost limit.

## 11. Caching strategy

### 11.1 Overview

Lookups go to the exact cache, then the semantic cache, then the upstream. Both caches are scoped to the tenant and configured per policy. Cache hits are served even when the budget is exhausted, because they cost nothing, and a cache failure never fails a request.

| | Exact | Semantic |
|---|---|---|
| Matches when | The canonical request hash is identical | The context is identical and the last user message is similar |
| Default | Opt-in per policy (recommended on) | Off |
| Main risk | Stale answers within the TTL; one fixed sample for temperature > 0 | False positives |
| Cost per lookup | One Redis GET, about 0.3 ms | One embedding plus a KNN search, within a 150 ms budget |

### 11.2 Exact cache

- **Key:** `sy:x:v1:{scope}:{nsver}:{sha256(canonical)}`. `canonical` is the sorted-key JSON of every field that affects the output: the resolved target (the model ID, or for `auto` the profile, policy ID and any constraint a header tightened: quality floor, TTFT target and cost cap), messages, tools, `tool_choice`, `response_format`, temperature, `top_p`, `max_tokens`, `stop`, `seed`, `n`, `logprobs`, penalties, reasoning parameters, and any unknown fields passed through. It excludes `stream`, `stream_options`, `user`, `metadata` and Switchyard's own headers.
- **Scope:** the tenant by default, or the key, or the end user (the `user` field), chosen per policy.
- **`nsver`:** a per-tenant namespace version stored in Redis state. Bumping it invalidates everything for that tenant in O(1).
- **Value:** the full `ChatResponse` plus metadata (the serving deployment, the parameters it dropped, and the creation time). Entries are capped at 1 MiB (`cache.max_entry_bytes`). A hit replays the dropped-parameters header the original response carried.
- **Policy check on every hit.** Entries are shared within a scope, but policies differ by key and rule, so a hit (exact, semantic or coalesced) is served only if the entry's deployment passes the request's own effective policy: allow, deny and `require_tags`, and for a pinned concrete model, the same model. Otherwise it counts as a miss (`result="policy"`) and the request is served upstream.
- **Written only for complete, successful responses**, with `finish_reason` of `stop`, `tool_calls` or `length`. Errors, content-filtered responses, partial streams and answers served under a budget-downgraded profile are never cached.
- **Temperature.** Identical requests with temperature above 0 all get the same cached sample. That's right for most applications but wrong for "generate N variations" loops, so `exact.deterministic_only: true` restricts caching to requests with temperature 0 or a seed.
- **Coalescing.** Identical non-streaming misses that arrive at the same time share one upstream call (an in-process singleflight). Coalescing across replicas is post-MVP.
- **Streaming.** Hits are replayed as described in §7. Streamed misses are aggregated and stored once the stream completes.

### 11.3 Semantic cache

- **Eligibility:** semantic caching is enabled in the policy; the last message is a user message no longer than `max_chars` (default 2,000); there are no images or audio; `n` is 1; and the request hasn't opted out.
- **Partition, which must match exactly:** a SHA-256 hash of the scope, resolved target, system prompts, every message before the last user message, tools, `response_format` and generation parameters. Only the last user message is compared semantically, so earlier context can never match "approximately".
- **Vector:** the embedding of the last user message. The index name includes the embedding model and its dimension, so changing the model starts a new index rather than mixing vector spaces.
- **Lookup:** a k = 1 nearest-neighbor search within the partition, using Redis's query engine (HNSW index, cosine distance, a TAG filter on the partition). It's a hit only if similarity ≥ `min_similarity`, which defaults to 0.95.
- **Time budget:** the embedding and search must finish within `lookup_timeout` (default 150 ms). A timeout counts as a miss, and so does any error.
- **Write:** done asynchronously after a successful response, reusing the embedding from the lookup. The TTL defaults to 72 hours.
- **Tuning from data:** Switchyard exports similarity histograms for hits and near misses, so operators can set thresholds from evidence.
- **Overhead:** the embedding's cost is recorded as overhead and subtracted from savings, and it is charged to the request's budgets. The semantic layer is skipped when a blocking budget is already exhausted, and when the request's effective policy excludes the embedding deployment; configuration validation also checks the embedding deployment against every policy that enables the semantic cache, since the last user message is sent to it.
- **Known failure mode:** near-duplicate questions with different answers, such as "What's the capital of France?" and "What's the capital of Germany?". The high threshold, narrow partitions, the short-message limit and the opt-in all guard against it, and a committed near-miss test set must not produce hits at default settings (Phase 6 acceptance).

### 11.4 Provider-side prompt caching

This is complementary: it discounts input tokens rather than reusing responses. The MVP accounts correctly for cached-input pricing (OpenAI's `cached_tokens`, Anthropic's cache read and cache write tokens). Post-MVP, Switchyard could insert Anthropic `cache_control` breakpoints automatically on long, stable prefixes, and route by prefix affinity, keeping a conversation on one deployment so its cache stays warm.

### 11.5 Operations and privacy

- The Redis query engine indexes only logical database 0, so with the semantic cache enabled `cache_url` must name database 0 (validated).
- Run the **cache** Redis with `maxmemory` and `allkeys-lru`, and the **state** Redis with `noeviction`, because budgets and rate limits must never be evicted. A small install can point both roles at one Redis with separate logical databases, but that's documented as unsuitable for production because the eviction policy applies to the whole instance.
- An admin endpoint purges a tenant's cache.
- **The cache is the only place Switchyard stores prompt and response content.** It is opt-in, bounded by TTLs, scoped to the tenant and purgeable, and the Redis instance must be private, with AUTH and TLS. Per-tenant encryption at rest is post-MVP.

## 12. Cost tracking, budgets & savings

### 12.1 Cost calculation

A request's cost is the sum, over each token class, of tokens × the deployment's price for that class. The classes are uncached input, cached input (read), cache write, and output (which includes reasoning tokens). Prices in config are USD per million tokens.

| Source | Reported | Normalized |
|---|---|---|
| OpenAI-wire | `prompt_tokens` (includes cached tokens), `prompt_tokens_details.cached_tokens`, `completion_tokens` | uncached = prompt − cached; cached; output = completion |
| Anthropic | `input_tokens` (excludes cached tokens), `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens` | Mapped one to one |
| Usage missing (stream aborted, or omitted by the provider) | Nothing | Estimated from Switchyard's token estimate and the aggregated output, with `usage_estimated = true` |

- The prices used are copied into each usage event, so later catalog edits never rewrite history.
- **Money representation:** the ledger uses Postgres `NUMERIC`, which is exact decimal. Budget counters on the hot path use integer micro-USD, rounded up, which is conservative and avoids floating-point drift.

### 12.2 Usage ledger

Every request appends one event, including cache hits and failures:

`id (UUIDv7) · ts · tenant · key · endpoint · requested_model · profile · served_deployment · provider · cache_status · status · attempts · fallback_reason · input / cached / cache_write / output tokens · usage_estimated · cost_usd · baseline_model · baseline_cost_usd · overhead_cost_usd · latency_ms · ttft_ms · tags (jsonb) · route_reason · matched_rule · shadow_deployment (observe mode) · config_version`

- The Postgres table is partitioned by month, so retention is cheap (drop old partitions), and indexed on `(tenant, ts)`.
- **Writer:** a bounded in-memory queue feeds batches of 500 events, or whatever has arrived after 1 s, written with `COPY`. If the database is down, the writer buffers and retries with backoff. If the buffer overflows, the oldest events are dropped, `usage_events_dropped_total` increments, and an alert fires; a disk spool is a post-MVP option. Graceful shutdown flushes the queue.
- In the MVP, reports read the event table directly. A daily rollup table can be added when volume calls for it, and the sink interface allows ClickHouse in a SaaS deployment.

### 12.3 Budgets

- **Settings:** scopes are `tenant` and `key`. Windows are `day` and `month`, in UTC. Each budget has a `limit_usd`, an optional `soft_at` threshold with a `soft_profile`, and `on_exhausted` set to `block` or `notify`.
- **Admission** (step 10). A Lua script atomically rejects the request if `spent + estimate > limit`, and otherwise adds the estimate to `spent`. **Settlement** (step 13) adds `actual − estimate`. If a fallback goes to a pricier deployment, the difference is admitted first. If the process crashes between admission and settlement, the estimate stays counted, so the error is always an over-count, never an under-count.
- Overshoot can come only from under-estimates on requests that were already admitted, never from a race between concurrent admissions. To keep under-estimates rare, admission prices the output at `max_tokens` when the request sets it. (The routing estimate uses the smaller expected output, §9.1.)
- **If Redis state is unavailable**, `budgets.fail_mode` decides. The default, `open`, keeps serving with per-request caps still enforced and raises an alert. `closed` rejects requests instead.

### 12.4 Savings, defined honestly

- **Baseline model:** the requested model when the request names a concrete one; otherwise `policy.baseline_model`, which is required whenever `auto` is allowed.
- **`baseline_cost`:** the same token counts priced at the baseline model's cheapest deployment. This is an approximation, since another model would tokenize and answer differently, and every report says so.
- **Routing savings** = `baseline_cost − actual_cost`. This can be negative, for example when a fallback costs more, and it's reported as is.
- **Cache savings** = the `baseline_cost` of the call the cache avoided.
- **Net savings** = gross savings − overhead. Overhead covers embedding calls now, and assessor and shadow calls later.
- **In observe mode**, projected savings = `baseline_cost` − the estimated cost of the deployment Switchyard would have chosen. Projected savings are reported separately and marked unverified until Phase 8's shadow evaluation checks quality.

### 12.5 Reporting API

`GET /switchyard/v1/usage?from=2026-11-01&to=2026-11-09&group_by=model` returns:

```json
{
  "period": { "from": "2026-11-01", "to": "2026-11-09" },
  "totals": {
    "requests": 120345,
    "cost_usd": "412.18",
    "overhead_usd": "3.85",
    "baseline_cost_usd": "1290.55",
    "savings_usd": { "routing": "731.02", "cache": "151.20", "net": "878.37" },
    "cache_hit_rate": 0.21,
    "fallback_rate": 0.004
  },
  "groups": [
    { "model": "llama-3.3-70b", "requests": 70211, "cost_usd": "61.40" }
  ]
}
```

`cost_usd` includes overhead, so `net = baseline − cost`. Money is serialized as decimal strings. Supported groupings are day, model, provider, key, tag, profile and cache status.

## 13. Policies & rule engine

**Policy fields**

| Field | Meaning | Default |
|---|---|---|
| `profile`, `allowed_profiles` | The default profile, and the profiles callers may select | `balanced`; all |
| `allow`, `deny` | Glob patterns over deployment IDs (`model@provider`) | Allow all |
| `prefer` | Ordered globs; earlier patterns form higher preference tiers | None |
| `require_tags` | A deployment must carry all of these tags, e.g. `zdr`, `region_eu`, `self_hosted` | None |
| `substitution` | `mode` (`pinned` or `equivalent`) and `max_quality_drop` | `pinned` |
| `routing_mode` | `enforce` or `observe` | `enforce` |
| `baseline_model` | The savings baseline for `auto` requests | Required if `auto` is allowed |
| `max_cost_per_request_usd` | Hard cap on a request's estimated cost | None |
| `on_unmet` | What happens when soft constraints can't be met: `best_effort` or `reject` | `best_effort` |
| `budgets` | A list of `{scope, window, limit_usd, soft_at, soft_profile, on_exhausted}` | None |
| `cache` | `exact{enabled, ttl, scope, deterministic_only}` and `semantic{enabled, min_similarity, ttl, max_chars}` | Both off |
| `limits` | `rpm`, `max_concurrent_streams`, `max_tokens_cap` | None |
| `drop_unsupported_params` | If false, deployments that can't honor a parameter are filtered out. If true, the parameter is dropped instead. | `false` |
| `rules` | An ordered list of `{name, match, set}` | None |

**Rules**

- Rules are evaluated in order, and **the first match wins**, as with firewall rules. That keeps them easy to reason about, and the explanation names the rule that matched.
- **`match`** succeeds only if every condition holds. Conditions: `model` (glob), `profile`, `task`, `difficulty_gte` and `difficulty_lt`, `key`, `key_tags`, `header` (a header name mapped to a glob), `tags`, `stream`, `has_tools`, `has_images`, `est_input_tokens_gte`.
- **`set`** can override any policy field except `rules` and `budgets`, for example `profile`, `min_quality`, `prefer`, `allow`, `deny`, `require_tags`, `cache`, `substitution` or `max_cost_per_request_usd`.
- Rules are written by the operator and trusted, so they may loosen or tighten the policy. Request headers may only tighten it.
- Validation rejects unknown fields, rules placed after an unconditional rule (they could never match), and references to unknown profiles or models.
- Post-MVP: `when:` expressions in CEL for complex conditions, and `extends:` for policy inheritance.

## 14. Reliability: retries, fallback, circuit breaking

### 14.1 Error taxonomy

| Kind | Examples | Retry same deployment | Fall back | Counts toward breaker |
|---|---|---|---|---|
| `transport` | DNS failure, connection refused or reset, TLS error | ✓ | ✓ | ✓ |
| `timeout` | Connect, first token, idle between chunks | ✓ | ✓ | ✓ |
| `server` / `overloaded` | 500, 502, 503, 504, Anthropic's 529 | ✓ | ✓ | ✓ |
| `rate_limited` | 429 | Only if `Retry-After` fits the deadline | ✓ | ✗ (a short cooldown instead) |
| `auth` | 401 or 403 on our provider credential | ✗ | ✓ | ✓, plus an alert |
| `not_found` / `context_length` | Unknown upstream model, context exceeded | ✗ | ✓ | ✗ |
| `invalid_request` | 400, 422 | ✗ | ✗ | ✗ |
| `content_filter` | Refusal, safety block | ✗ | ✗ | ✗ |

### 14.2 Retries

- By default each deployment gets at most one retry (configurable). The backoff is exponential with full jitter, starting at 250 ms and capped at 2 s.
- `Retry-After` is honored when it fits within the remaining deadline. Otherwise Switchyard moves straight to the next candidate.
- **Retry budget:** retries to a deployment may not exceed 20 % of its requests over a sliding 10-second window. This prevents retry storms during an outage.
- There's no retry after the commit point, and none past the deadline.

### 14.3 Fallback plans

A plan holds up to N candidates (default 3), all of which satisfy every hard constraint. Each fallback slot prefers a provider that isn't already in the plan. A failure on one deployment moves on to the next one in the plan.

### 14.4 Circuit breakers

There is one breaker per deployment on each replica. It opens after 5 consecutive failures, or after a failure ratio of at least 50 % across at least 20 requests within 30 s. It stays open for 30 s, then goes half-open and admits up to 3 probe requests; a success closes it again. Only the kinds marked in §14.1 count as failures. Breaker state is exported as a metric and shown on `/admin/v1/providers`.

### 14.5 Timeouts and deadlines

Timeouts are set per provider and can be overridden per model: 5 s to connect, 30 s to the first token (raise this for reasoning models), and 60 s idle between chunks. The total deadline is the smallest of the server maximum, the policy's value and the header's value. The deadline travels in the `context` to every call, Redis calls included.

### 14.6 Concurrency limits and spillover

Each deployment can set `max_concurrency`, enforced per replica. The router skips saturated deployments, so overflow goes to the next cheapest one (§9.4).

### 14.7 Degraded modes

| Dependency down | Behavior | Signal |
|---|---|---|
| Redis cache | Lookups and writes are skipped and treated as misses | `cache_lookups_total{result="error"}`, plus a rate-limited warning log |
| Redis state | Rate limits fail open (configurable); budgets follow `fail_mode`; namespace versions use the last known value | `state_errors_total`, plus an alert |
| Postgres | Events are buffered; on overflow the oldest are dropped. Requests are unaffected. | `usage_queue_depth`, `usage_events_dropped_total` |
| Embedding deployment | The semantic cache is skipped | `cache_lookups_total{layer="semantic",result="error"}` |
| One provider | Its breaker opens and the router routes around it | Breaker gauge, `fallbacks_total` |
| Every candidate | `503 no_healthy_upstream`, with the attempts listed | Errors by code |

### 14.8 Shutdown and hot reload

- **On SIGTERM**, readiness starts failing, the listener stops accepting new connections, and in-flight requests drain for up to `shutdown_grace` (default 60 s, since streams can be long). Anything still running after that is cancelled. The usage queue is flushed, then the process exits.
- **Hot reload** is triggered by SIGHUP or `POST /admin/v1/reload`. Switchyard loads and validates the new snapshot and swaps it in atomically. An invalid config is rejected and the old one keeps serving. In-flight requests finish on the snapshot they started with. Breaker and latency state is keyed by deployment ID, so it survives reloads. Changes to listeners or storage are reported as needing a restart.

## 15. Observability

### 15.1 Logs

Logs are JSON from `slog`, with one access-log line per request carrying these fields:

`ts · level · msg · request_id · trace_id · tenant · key_id (never the key) · endpoint · stream · requested_model · profile · served_deployment · attempts · fallback_reason · cache · status · error_code · latency_ms · ttft_ms · overhead_ms · input_tokens · output_tokens · cost_usd · savings_usd · route_reason · matched_rule`

Full route explanations are logged at debug level, or sampled. **Prompt and response content is never logged.** A redacting handler masks anything that looks like a credential, and tests verify it. Payload logging is post-MVP: opt-in, sampled, and written to a separate sink.

### 15.2 Metrics

All metric names are prefixed `switchyard_`.

| Metric | Type | Labels |
|---|---|---|
| `requests_total` | counter | endpoint, status_class, cache |
| `request_duration_seconds` | histogram | endpoint, stream |
| `overhead_duration_seconds` | histogram | endpoint. This is the gateway's own SLI. |
| `upstream_requests_total` | counter | provider, model, outcome (`ok`, `rate_limited`, `timeout`, `server_error`, `client_error`, `auth`, `transport`) |
| `upstream_ttft_seconds`, `upstream_duration_seconds` | histogram | provider, model |
| `tokens_total` | counter | provider, model, class |
| `cost_usd_total` | counter | provider, model |
| `savings_usd_total` | counter | source (`routing`, `cache`) |
| `cache_lookups_total` | counter | layer (`exact`, `semantic`), result (`hit`, `miss`, `error`, `skipped`, `policy`, `budget`) |
| `cache_writes_total` | counter | layer, result (`ok`, `error`, `dropped`, `too_large`, `not_cacheable`) |
| `cache_similarity` | histogram | outcome (`hit`, `miss`) |
| `cache_lookup_seconds` | histogram | layer |
| `cache_coalesced_total` | counter | |
| `route_decisions_total` | counter | profile, served_model, reason |
| `route_relaxations_total` | counter | constraint |
| `fallbacks_total` | counter | from, to, reason |
| `retries_total` | counter | provider, reason |
| `circuit_state` | gauge | deployment |
| `budget_utilization_ratio` | gauge | scope (tenant label optional) |
| `state_errors_total` | counter | operation |
| `usage_queue_depth` | gauge | none |
| `usage_events_dropped_total` | counter | none |

**Cardinality rules.** The tenant label is configurable: turn it on for self-hosted installs with few tenants and off for SaaS, where per-tenant reporting comes from the ledger. Tags never become labels. Model and provider labels are bounded by the catalog.

### 15.3 Traces

Traces use OpenTelemetry. Switchyard accepts W3C `traceparent` from clients and, if configured, propagates it upstream. Each request produces a span tree: the request span, with children for `auth`, `ratelimit`, `policy`, `cache.exact`, `cache.semantic` (with `embed` and `search`), `route`, `budget.admit`, `upstream.attempt[n]` (provider, model, status, retry reason) and `usage.record`. Attributes follow the OpenTelemetry GenAI semantic conventions where they exist, such as `gen_ai.request.model` and `gen_ai.usage.input_tokens`, so LLM observability tools understand the traces. Sampling is parent-based with a ratio, and the collector does tail-based sampling of errors.

### 15.4 Dashboards and alerts

These ship in `deploy/`. The Grafana dashboard covers traffic, overhead, upstream errors by provider, breaker states, cost and savings, cache hit rate, routing mix by profile, and relaxations.

The Prometheus alert rules fire when:

- a provider's error rate stays above 5 % for 5 minutes,
- a breaker stays open for more than 5 minutes,
- a budget passes 90 %,
- overhead p99 stays above 25 ms for 10 minutes,
- usage events are dropped,
- the cache error rate is elevated, or
- a config reload fails.

### 15.5 Introspection

- `/admin/v1/providers` shows live health, breaker state, latency stats and in-flight count for each deployment.
- `/readyz` reports the config version hash, which reveals config drift between replicas.
- The explain endpoint shows why any request would be routed where it is.

## 16. Configuration

### 16.1 Files and precedence

Settings resolve in this order, with later sources overriding earlier ones:

1. built-in defaults;
2. the YAML file or files;
3. a short list of operational env overrides: `SWITCHYARD_CONFIG`, `SWITCHYARD_LISTEN`, `SWITCHYARD_ADMIN_LISTEN`, `SWITCHYARD_LOG_LEVEL`;
4. CLI flags.

`include:` splits config across files using globs. List sections are concatenated, and a duplicate ID is a validation error. The model catalog normally lives in its own files, because prices change more often than policies do.

### 16.2 Secrets

Secrets are never written literally into config. They are referenced as `${VAR}` (a required env var), `${VAR:-default}`, or `${file:/run/secrets/name}` (for Docker and Kubernetes secrets). Validation warns when a literal value looks like a credential, for example one starting with `sk-`. Vault and KMS resolvers can come later, behind `CredentialResolver`.

### 16.3 Main config example

```yaml
# switchyard.yaml
version: 1

server:
  listen: ":8080"
  admin_listen: "127.0.0.1:9090"      # /metrics and /admin/v1/*; keep private
  max_request_bytes: 10MiB
  shutdown_grace: 60s

storage:
  mode: redis                          # memory (quickstart and dev only) | redis
  redis:
    cache_url: ${REDIS_CACHE_URL:-redis://localhost:6379/0}   # evictable: allkeys-lru
    state_url: ${REDIS_STATE_URL:-redis://localhost:6380/0}   # budgets and limits: noeviction
  ledger:
    driver: postgres                   # memory | postgres
    dsn: ${DATABASE_URL}

include:
  - catalog/*.yaml                     # models, deployments, prices, quality priors

providers:
  - id: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
  - id: anthropic
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    forward_headers: [anthropic-beta]  # client headers are not forwarded unless listed
  - id: google
    type: openai                       # Gemini via Google's OpenAI-compatible endpoint (MVP)
    base_url: https://generativelanguage.googleapis.com/v1beta/openai
    api_key: ${GEMINI_API_KEY}
  - id: together
    type: openai
    base_url: https://api.together.xyz/v1
    api_key: ${TOGETHER_API_KEY}
  - id: gpu-cluster                    # self-hosted vLLM
    type: openai
    base_url: http://vllm.internal:8000/v1
    tags: [self_hosted, zdr]
    max_concurrency: 64                # per replica; when saturated, traffic spills over
    timeouts: { connect: 2s, first_token: 20s }

routing:
  default_profile: balanced
  expected_output_tokens: 500
  profiles:
    cheap:    { min_quality: 0.65, objective: cost }
    balanced: { min_quality: 0.80, objective: cost }
    quality:  { min_quality: 0.92, objective: cost }
    fast:     { min_quality: 0.80, objective: latency }
  assessor:
    type: heuristic                    # signal weights can be tuned here
  fallback: { max_deployments: 3, retries_per_deployment: 1 }

aliases:                               # virtual model names for applications
  code-review: auto:quality
  autocomplete: auto:fast

semantic_cache:
  embedding_model: text-embedding-3-small@openai   # or a self-hosted TEI or Ollama deployment
  lookup_timeout: 150ms

policies:
  - id: default
    profile: balanced
    baseline_model: claude-sonnet-5-5  # "what you'd have used without Switchyard"
    substitution: { mode: pinned }     # pinned | equivalent (with max_quality_drop)
    routing_mode: enforce              # enforce | observe
    max_cost_per_request_usd: 0.50
    budgets:
      - { scope: tenant, window: month, limit_usd: 2000, soft_at: 0.8, soft_profile: cheap, on_exhausted: block }
      - { scope: key,    window: day,   limit_usd: 100,  on_exhausted: notify }
    cache:
      exact:    { enabled: true, ttl: 24h, scope: tenant }
      semantic: { enabled: false, min_similarity: 0.95, ttl: 72h, max_chars: 2000 }
    rules:                             # first match wins
      - name: staging-is-cheap
        match: { header: { x-env: staging } }
        set:   { profile: cheap }
      - name: eu-keys-stay-in-eu
        match: { key_tags: [eu] }
        set:   { require_tags: [region_eu] }
      - name: code-needs-quality
        match: { task: coding, difficulty_gte: 0.5 }
        set:   { min_quality: 0.85 }

tenants:
  - id: acme
    policy: default
    keys:
      - id: acme-backend
        hash: sha256:4be1c0…           # from `switchyard keys generate`; the key itself is never stored
        tags: [prod]
```

### 16.4 Catalog example

```yaml
# catalog/models.yaml
# Illustrative entries. Real IDs, prices and priors are curated and dated in Phase 2.
# Prices are USD per 1M tokens. Quality priors are 0..1 starting estimates (§10.2).
models:
  - id: llama-3.3-70b
    context_window: 131072
    max_output_tokens: 8192
    capabilities: [streaming, tools, json_mode]
    quality: { general: 0.70, coding: 0.62, reasoning: 0.58, extraction: 0.75 }
    token_ratio: 1.05                  # relative to the bytes/4 estimate
    deployments:
      - provider: gpu-cluster
        upstream_model: meta-llama/Llama-3.3-70B-Instruct
        pricing: { input: 0.10, output: 0.10 }            # amortized GPU cost
      - provider: together
        upstream_model: meta-llama/Llama-3.3-70B-Instruct-Turbo
        pricing: { input: 0.88, output: 0.88 }
        quality_offset: -0.02                             # quantized variant

  - id: claude-sonnet-5-5
    context_window: 200000
    capabilities: [streaming, tools, parallel_tools, vision, json_schema, prompt_caching]
    quality: { general: 0.93, coding: 0.95, reasoning: 0.92, extraction: 0.94, tool_use: 0.95 }
    deployments:
      - provider: anthropic
        upstream_model: claude-sonnet-5-5
        pricing: { input: 3.00, output: 15.00, cached_input: 0.30, cache_write: 3.75 }
        latency_hint: { ttft_ms: 900, tokens_per_sec: 70 }

  - id: text-embedding-3-small
    type: embedding
    dimensions: 1536
    deployments:
      - provider: openai
        upstream_model: text-embedding-3-small
        pricing: { input: 0.02 }
```

### 16.5 Validation

Validation fails fast but reports every error, not just the first. It checks that:

- there are no unknown keys;
- every reference resolves: deployments to providers, policies to profiles and models, aliases to targets, and the embedding model whenever any policy enables the semantic cache;
- numbers are in range: prices ≥ 0, qualities within [0, 1], thresholds within (0, 1];
- key hashes are well formed and unique;
- `baseline_model` is set wherever `auto` is allowed;
- Redis is configured whenever a feature needs it.

`switchyard config validate` runs the same checks, so CI can run them too.

### 16.6 Reload

See §14.8.

## 17. Security considerations

| Threat | Controls |
|---|---|
| **Provider credentials leak.** These are the most valuable asset: a leaked key is direct spend. | Secrets come only from env vars or secret files, with KMS or Vault later. They are never logged: a redacting log handler enforces this and tests scan the log output. The admin config endpoint never exposes them. Outbound traffic goes only to configured base URLs. |
| **Gateway keys are stolen or guessed.** | Keys are 32 random bytes, base64url-encoded, with an `sy_` prefix that secret scanners can recognize. They're stored only as SHA-256 hashes. Because the keys are 256-bit random rather than human-chosen passwords, a fast hash is safe and keeps auth in microseconds. Comparison is constant-time. Keys can be revoked by config, or later in a database, and repeated auth failures are rate-limited. |
| **Data leaks across tenants.** | The tenant ID is part of every cache key, budget key and ledger row. Reporting queries are filtered by tenant in the store layer, not in the handlers, and isolation tests cover this. |
| **Denial of wallet.** An attacker or a bug runs up cost. | Per-request cost caps, budgets, a `max_tokens` cap, requests-per-minute and concurrent-stream limits, and allow lists. Inflating a request's apparent difficulty can't push its cost past the policy's cap. |
| **Denial of service and resource exhaustion.** | Body limit (10 MiB) and header limits, a JSON depth limit, `ReadHeaderTimeout`, idle timeouts, per-write deadlines on streams, and per-deployment concurrency caps. |
| **SSRF.** | Switchyard never fetches URLs supplied by a client. Image URLs are passed through, or the deployment is filtered out. Provider URLs come only from operator config. When SaaS later lets tenants add custom endpoints, those will need allow lists and an egress policy. |
| **Sensitive data at rest.** | Content is never logged. The ledger holds metadata only. Cached content is opt-in, bounded by TTLs, scoped to the tenant and purgeable. Redis and Postgres must be private, with AUTH and TLS. |
| **Admin surface exposed.** | The admin API listens on a separate port bound to localhost by default and requires an admin token. Reloads and purges are written to an audit log. |
| **Client headers smuggled upstream.** | Client headers aren't forwarded except through a per-provider allow list (e.g. `anthropic-beta`). The client's `Authorization` header is never forwarded. |
| **Policy evasion through prompt content.** | Content affects only the quality estimate, never authorization, allow lists, budgets or caps. |
| **Refusal shopping.** | Refusals and content-filter responses are never retried on another provider (§7). |
| **Compliance.** | Data residency and zero-data-retention requirements are enforced as hard filters through deployment tags (`region_eu`, `zdr`). |
| **Supply chain.** | Few dependencies with pinned versions, `govulncheck` in CI, an SBOM, and a distroless image that runs as non-root on a read-only filesystem. |

## 18. Deployment: self-hosted & SaaS

The same binary serves every deployment model.

### 18.1 Self-hosted tiers

| Tier | Topology | State |
|---|---|---|
| Quickstart | A single binary or container with `storage.mode: memory` and one provider key | In-process; lost on restart |
| Small production | `docker compose`: 1–2 replicas, Redis for cache and state, Postgres | Shared |
| High availability | Kubernetes (the Helm chart is post-MVP): at least 3 replicas, autoscaling on CPU and in-flight requests, separate Redis instances for cache and state, managed Postgres, an OTel collector | Shared |

### 18.2 SaaS evolution

The SaaS build swaps implementations behind the seams that already exist, with no rewrite:

| Seam | Self-hosted MVP | SaaS |
|---|---|---|
| `ConfigSource` | YAML files | A control-plane API or database, cached as a snapshot and pushed or polled |
| `KeyStore` | Key hashes in config | Postgres, an in-process TTL cache of principals, and revocation over pub/sub |
| `CredentialResolver` | Env vars and secret files | Per-tenant bring-your-own-key, with envelope encryption through a KMS |
| `UsageSink` | Postgres | Kafka or ClickHouse for analytics and billing export |
| `VectorIndex` | Redis | Redis, or a dedicated vector database per region |
| Isolation | The operator is the trust boundary | Per-tenant quotas, noisy-neighbor limits, optionally dedicated data planes |

Architecturally, SaaS adds a **control plane** (tenants, keys, policies, billing, UI) next to the existing **data plane**, which stays the stateless gateway described here. Data planes can be deployed per region to meet data-residency needs.

## 19. Project structure

```
switchyard/
├── cmd/switchyard/            # CLI: serve · config validate · keys generate · route explain
├── internal/
│   ├── app/                   # composition root: config snapshot → wired components
│   ├── server/                # listeners, middleware, graceful shutdown
│   ├── api/
│   │   ├── openaiapi/         # /v1/* handlers, OpenAI error envelope
│   │   ├── syapi/             # /switchyard/v1/* (usage, explain, feedback)
│   │   └── adminapi/          # /admin/v1/*
│   ├── schema/                # canonical OpenAI-shaped types, validation, capability extraction
│   ├── gateway/               # pipeline orchestration per endpoint
│   ├── auth/                  # keys, principals, admin auth
│   ├── config/                # schema, loading, ${…} expansion, validation, reload
│   ├── catalog/               # models, deployments, pricing, capabilities, aliases
│   ├── policy/                # policies, rules, matching, header tightening
│   ├── quality/               # Assessor, Estimator, heuristic implementation
│   ├── router/                # filters, estimates, ranking, plans, explanations
│   ├── executor/              # attempts, retries, fallback, breakers, deadlines
│   ├── providers/             # Adapter interface, error kinds, registry
│   │   ├── openai/            # OpenAI and every OpenAI-compatible upstream
│   │   └── anthropic/         # Messages API, including SSE translation
│   ├── sse/                   # reader, writer, aggregator, replay
│   ├── cache/                 # exact, semantic, key derivation, embedder client
│   ├── budget/
│   ├── ratelimit/
│   ├── usage/                 # cost calculation, events, sinks, reports, savings
│   ├── health/                # latency stats, breaker registry, in-flight counts
│   ├── store/
│   │   ├── redisstore/        # including embedded Lua scripts
│   │   ├── pgstore/           # including embedded migrations/
│   │   └── memstore/          # dev/test implementations of every store
│   └── telemetry/             # slog, Prometheus, OpenTelemetry
├── configs/
│   ├── switchyard.example.yaml
│   ├── quickstart.yaml        # memory mode, one provider
│   └── catalog/               # curated model catalog
├── deploy/
│   ├── Dockerfile
│   ├── compose.yaml           # gateway, Redis ×2, Postgres (+ optional observability stack, TEI)
│   ├── grafana/
│   └── prometheus/            # scrape config, alert rules
├── test/
│   ├── fakeupstream/          # scriptable fake OpenAI and Anthropic servers (faults, latency, SSE)
│   ├── fixtures/              # recorded or handcrafted provider payloads and streams
│   ├── conformance/           # openai-go SDK suite run against the gateway
│   └── load/                  # overhead and soak harness
├── docs/
│   ├── DESIGN.md
│   ├── PLAN.md
│   └── adr/                   # one file per decision, added when a decision is revisited
├── Makefile
└── go.mod
```

**Rules**

- Everything lives under `internal/`, so the MVP makes no commitments to a public Go API.
- Domain packages depend on interfaces. Only `app` imports concrete adapters and stores.
- `gateway` is the only package that knows the order of the pipeline's steps.
- There are no package-level mutable globals. The metrics registry, clock and random source are injected, so tests are deterministic.

## 20. Testing strategy

| Layer | What | How |
|---|---|---|
| Unit | Router, policy, quality, cost calculation, cache keys, error classification | Table-driven tests, plus property tests with `rapid` |
| Translation | Adapter requests, responses and streams | Golden files from recorded or handcrafted fixtures; fuzzing of the SSE parser and decoders |
| Component | Executor, budgets, rate limits | Scripted fake upstreams (`httptest`) with fault injection; `miniredis` with Lua |
| Integration | Redis 8 vector search, Postgres ledger | `testcontainers-go`, behind an `integration` build tag |
| Conformance | OpenAI compatibility | The official `openai-go` SDK run against the gateway, with a fake upstream for each adapter |
| Load and soak | Overhead, leaks | A Go load harness with a mock upstream, pprof, and a 1-hour soak |
| Live smoke (manual) | Real providers | `make smoke` with real keys, opt-in only and never in CI |

**Critical paths that must have tests:**

1. OpenAI wire fidelity, including streaming and tool calls.
2. Translation correctness for each adapter.
3. Commit-point semantics: no retry after the first byte, and a client disconnect cancels the upstream.
4. Error classification, and the retry and fallback decisions that follow from it.
5. Router invariants: the hard constraints always hold, the cheapest option within a preference tier wins, and explanations are accurate.
6. Budget admission under concurrency.
7. Cost calculation for each provider, including cached tokens.
8. Cache key derivation, tenant isolation, and the semantic near-miss set.
9. Auth: key verification, admin isolation, and no secrets in logs.

CI runs unit and component tests on every push, and integration and conformance tests on every pull request. Load tests run nightly or on demand.

## 21. Decision log

| # | Decision | Alternatives considered | Why |
|---|---|---|---|
| D1 | Go for the data plane | Python/FastAPI, Rust, TypeScript | See §8.1. |
| D2 | OpenAI Chat Completions as the canonical internal format | A neutral internal schema; pairwise translators | Most clients and many upstreams already speak it, so OpenAI-wire upstreams need no translation. It also means N adapters instead of N² translators. |
| D3 | Routing as deterministic constrained optimization | A weighted score; an ML router in the hot path | It matches the stated goal (the cheapest model that meets the requirements), and it's explainable and testable. ML can still plug in through `Estimator`. |
| D4 | Quality from priors combined with heuristic difficulty (§10.4) | Static tiers only; an LLM classifier on every request | Needs no training data, adds only microseconds, and can be calibrated later. |
| D5 | Substitution is opt-in (`pinned` by default), with an observe mode | Substitute by default | Silent model swaps destroy trust. Observe mode shows the money first. |
| D6 | Redis in cache and state roles, a Postgres ledger, and in-memory implementations | Redis only; SQLite; ClickHouse | Redis suits hot counters and caches, while money needs a durable SQL ledger. The memory implementations serve tests and the quickstart. |
| D7 | Semantic cache off by default, partitioned exactly, failing open | On by default; embedding the whole conversation | False positives are the main risk, and exact partitioning removes most of them. |
| D8 | No retries after the commit point | Buffering the first N tokens before committing | Buffering adds latency to every stream, and retrying after bytes have reached the client corrupts the output. |
| D9 | Upstream calls use the same stream mode as the client | Always stream upstream and aggregate | Keeps non-streaming responses byte-faithful. The aggregator still exists for caching. |
| D10 | Budgets via atomic admission of the estimate, then settlement, in integer micro-USD | Check-then-act; reservations with expiry | No overshoot from concurrency; a crash errs toward over-counting; one key per window. |
| D11 | YAML with `${…}` secret references, strict decoding and atomic reload | TOML; env vars only; viper or koanf | YAML fits ops tooling such as Helm and Kubernetes, strict decoding catches typos, and the approach needs few dependencies. |
| D12 | A native Anthropic adapter; Gemini through Google's OpenAI-compatible endpoint for the MVP | Native adapters for every provider; the compatibility endpoints for all of them | Anthropic positions its compatibility endpoint for testing rather than production, and it lacks some features. Google's endpoint is good enough to start with. |
| D13 | First-match declarative rules now, CEL later | CEL now; a custom DSL | It's the simplest thing that covers the stated needs, and CEL fits the same rule shape later. |
| D14 | Gateway endpoints under `/switchyard/v1`, with admin on a separate listener | Adding them under `/v1`; a single listener | Avoids collisions with future OpenAI paths and keeps admin from being exposed by accident. |
| D15 | No payload logging; the cache is the only content store, and it's opt-in | Logging prompts for debugging | Privacy by default. Storing content is an explicit operator choice. |
| D16 | Refusals and content filters are never retried on another provider | Treating them as retryable | Prevents policy evasion by refusal shopping. |

## 22. Risks & mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Quality is misjudged and hard requests go to weak models | Visible quality regressions and lost trust | `pinned` default, observe mode, conservative bars, client hints, the explain endpoint, relaxation metrics, and Phase 8 evaluations with shadow sampling |
| Gaps in translation, especially tools and streaming edge cases | Broken clients | A conformance suite using the official SDK against every adapter, golden fixtures, and filtering on capabilities instead of lossy mapping |
| Semantic cache false positives | Wrong answers | Off by default, exact partitions, a high threshold, near-miss tests, and similarity telemetry |
| Stale prices in the catalog | Wrong costs and savings | Prices snapshotted into each event, the catalog version in reports, a price-review checklist, and price sync after the MVP |
| Overstated savings | Lost credibility | Conservative definitions, overhead subtracted, the approximation stated in every report, and projected savings kept separate from realized savings |
| The gateway becomes a single point of failure | Every LLM feature goes down at once | Stateless replicas, optimizations that fail open, the degraded-mode table, readiness gates, and load tests |
| Provider rate limits and quotas | Storms of 429s | Per-deployment concurrency caps, 429 cooldowns, the retry budget, and provider diversity in plans |
| Scope creep (Responses API, a UI, and so on) | The MVP never ships | Explicit non-goals and a post-MVP backlog |

## 23. Open questions

1. **Language.** Are you happy with Go? Python is viable; switching would change §8 and the project layout, not the architecture.
2. **Name.** Keep "Switchyard"? From Phase 0 on, it determines the binary, header and env var names.
3. **Substitution default.** The proposal is `pinned`, with savings coming from `auto:*` models, provider arbitrage, caching and observe-mode previews. Should any tenant type default to `equivalent`?
4. **Ledger.** The proposal is Postgres in production and in-memory for development. Do you also need a zero-dependency durable option, such as SQLite, for single-node self-hosting?
5. **Providers.** Which upstreams and self-hosted servers matter most to you first? Do you have API keys for live smoke tests and for recording fixtures?
6. **Scale horizon.** What request rate and how many tenants do you expect within 12 months? The answer sets metric-cardinality defaults and when ClickHouse and control-plane work should start.
7. **License.** Apache-2.0 helps adoption; AGPL or BSL protects a SaaS business. This doesn't block Phase 0.
