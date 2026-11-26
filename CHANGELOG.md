# Changelog

All notable changes to Switchyard. Versions follow [Semantic
Versioning](https://semver.org/); before 1.0, a minor version may change
the configuration or the API.

## v0.4.0 (2026-11-26)

Adds a native Gemini adapter and makes it the default for the shipped
`google` provider, with thought signatures kept across multi-turn tool
calls.

### Providers (P11)

- New provider type `gemini`: Google's native Gemini API
  (`{base_url}/models/{model}:generateContent` and
  `:streamGenerateContent?alt=sse`, `base_url`
  `https://generativelanguage.googleapis.com/v1beta`), with the key only in
  the `x-goog-api-key` header. It reports cached tokens
  (`cached_tokens`, priced at `cached_input`) and thinking tokens
  (`reasoning_tokens`, priced once as output), supports forced and named
  `tool_choice`, `json_object` and `json_schema`, maps `reasoning_effort`
  to `thinkingLevel`, and sends `store: false` on every request.
- Multi-turn tool calls keep Gemini 3 and 4's thought signatures with no
  state in the gateway: the signature travels inside the tool-call id
  (`gm1_…`, only `[A-Za-z0-9_-]`), which clients send back unchanged. Ids
  from other providers or models get Google's skip value. The ids are
  long (about 4/3 of the signature), so the `openai` adapter now shortens
  tool-call ids over OpenAI's 40-character limit, and the `anthropic`
  adapter ids over 64 characters, to a 37-character hash (same in the call
  and its result), and a conversation can move from Gemini to OpenAI or
  Claude. Requests without such ids are forwarded as before.
- Traces name Gemini's provider `gcp.gemini` (`gen_ai.system`,
  `gen_ai.provider.name`), the OpenTelemetry GenAI value.
- A non-streaming request whose output limit is above 32,768 tokens (its
  `max_tokens`, else the catalog's `max_output_tokens`) is served by
  streaming internally, so Gemini 4 Carbon's full 65,536 works without
  streaming on the client, even when the client sets no limit.
- `MALFORMED_FUNCTION_CALL` and `MALFORMED_RESPONSE` fall back to another
  deployment without a retry and without counting toward the breaker or
  the error-rate alert: they depend on the request. Google billed them, so
  the attempt is charged its exact usage (in the usage event's overhead,
  the budgets and `switchyard_cost_usd_total` for that deployment) on top
  of the deployment that then serves the request. A failed upstream answer
  that was billed now carries its usage (`providers.Error.Billed`,
  `BilledUsage`), which shadow sampling, the judge, `switchyard eval` and
  the semantic cache's embedder also charge.
- `temperature` and `top_p` are removed for Gemini, as for every
  deployment without `sampling`, and the request is served; `n` > 1,
  logprobs, seed and penalties exclude Gemini deployments (or are removed
  with `drop_unsupported_params`). Requests the native API cannot express
  (an https image URL, a trailing assistant message) are never sent: they
  go to another deployment, or get a 400 naming the reason.
  `reasoning_effort` maps to the nearest level every current model
  accepts (`minimal` to low, `xhigh` to high). Google's errors map to the usual kinds: 429, 5xx and 503 retry
  and fall back; 400 and 403 do not; an invalid key is an auth failure;
  a blocked prompt is `finish_reason: content_filter`.
- Catalog: the Gemini models' capabilities are rechecked for the native
  API and now also list `parallel_tools`, `forced_tools`, `json_mode` and
  `prompt_caching`. Prices are unchanged.
- Behaviour change: the example config, the compose stack
  (`deploy/config/switchyard.yaml`) and the Helm chart's bundled config
  now define the `google` provider as `type: gemini` with the native
  `base_url`, with `first_token: 2m` (Gemini 4 thinks before its first
  token) and `total: 10m`, the default `routing.request_timeout`, which caps
  every call. Google advises at least 15 minutes for long Gemini 4 Carbon
  output: raise both if you serve it. On upgrade, compose and Helm
  users with the bundled config talk to the native API with the same
  `GEMINI_API_KEY`; tool-call ids from Gemini change format, so a
  conversation started before the upgrade continues with the skip value on
  its old calls. A config of your own keeps working: `type: openai` with
  `https://generativelanguage.googleapis.com/v1beta/openai` still validates
  and serves, but the shipped catalog now claims capabilities that endpoint
  lacks; give those deployments
  `capabilities: [streaming, stream_usage, tools, vision, json_schema]`
  (configs/catalog/README.md).
- `make record-fixtures` also records Gemini fixtures (needs
  `GEMINI_API_KEY`), and `make smoke` has a live Gemini test with a
  two-turn tool call (`TestSmokeGemini`, skipped without the key). Runbook:
  "Gemini tool calls rejected: missing thought signature".

## v0.3.0 (2026-11-23)

Adds dated catalog prices, so that Google's announced Gemini price changes
(2026-12-01 and 2027-01-01) take effect on time in a running gateway.

### Catalog (P10)

- Dated price changes: `pricing.changes` lists a deployment's announced
  price changes, each from a date (00:00 UTC) or an RFC 3339 timestamp,
  inheriting the prices it does not give. The gateway puts each change in
  force at its time, without a reload or a restart: costs,
  budgets and routing switch together. New metrics
  `switchyard_catalog_price_switches_total{result}` and
  `switchyard_catalog_next_price_change_timestamp_seconds`, and the alert
  `SwitchyardCatalogPriceSwitchFailed` (runbook: "Catalog price change
  failed"). The shadow judge and the semantic cache's embedder charge at
  their deployment's prices in force too.
- Behaviour change: an unpinned `evaluation.judge.model` or
  `semantic_cache.embedding_model` whose cheapest deployment a policy
  refuses now uses the cheapest deployment the policies permit (possibly a
  pricier provider) instead of failing validation; pin `model@provider` to
  choose one. `switchyard eval`'s default judge makes the same choice as
  shadow sampling.
- `switchyard catalog lint` notes each deployment's next price change and
  any change already in force (to fold into the base prices); neither
  fails it.
- The shipped catalog has Google's announced changes: `gemini-4-carbon`
  from 2026-12-01 (4.00 / 20.00 / 0.20) and `gemini-3.8-flash` from
  2027-01-01 (1.50 / 7.50 / 0.15).

## v0.2.0 (2026-11-21)

Adds a Helm chart for running Switchyard on Kubernetes. The gateway itself
is unchanged from v0.1.0.

### Kubernetes (P9)

- A Helm chart, `deploy/helm/switchyard`, for the high-availability tier:
  3 replicas by default, Redis and Postgres you already run, secrets from a
  Secret only, pods that pass Pod Security "restricted", a NetworkPolicy on
  by default, a PodDisruptionBudget, optional autoscaling, ServiceMonitor
  and PrometheusRule, and a `helm test` hook. See
  [docs/kubernetes.md](docs/kubernetes.md).
- A config, catalog or Secret change rolls the pods; the grace period is
  derived from the shutdown settings.
- `make helm-lint`, `make helm-test` (a kind cluster) and
  `make helm-package`; CI runs them, and releases attach the packaged chart.

### Build

- The Dockerfiles pin their BuildKit frontend (`docker/dockerfile:1.29.0`)
  by digest.

## v0.1.0 (2026-11-19)

The first release: a cost-aware, OpenAI-compatible LLM gateway you can run
in production and self-host (milestone M4 of [docs/PLAN.md](docs/PLAN.md)).
It was built in phases; each phase's acceptance criteria and outcome are in
the plan.

### Foundations (P0)

- One static binary, `switchyard`, with `serve`, `config validate`,
  `catalog lint`, `route explain`, `keys generate`, `eval`, `healthcheck`
  and `version`.
- Strict YAML configuration: unknown keys rejected, every error reported
  with its file, line and path, `${VAR}`, `${VAR:-default}` and
  `${file:…}` references, includes, and secrets that never appear in
  errors, logs or CLI output.
- JSON logs through a redacting handler, Prometheus metrics on a separate
  admin listener, `/healthz` and `/readyz`, graceful shutdown.
- A distroless image that runs as UID 65532.

### OpenAI-compatible proxy (P1)

- `POST /v1/chat/completions`, JSON and streaming, and `GET /v1/models`,
  behind gateway API keys (`sy_…`, stored only as SHA-256 hashes).
- The OpenAI SDK conformance suite passes through the gateway as it does
  against the upstream directly.
- Body, header and slow-client limits; one access-log line per request.

### Catalog and providers (P2)

- A model catalog with prices, capabilities, context windows, quality
  priors and aliases; 11 curated models across OpenAI, Anthropic, Gemini's
  OpenAI-compatible endpoint and Together, prices checked on 2026-11-11.
- The Anthropic adapter, translating requests, responses, streams, tool
  calls, errors and usage.
- A request is only ever sent to a deployment that can honor it;
  `model@provider` pins a provider.
- `POST /v1/completions` (natively or through the chat shim) and
  `POST /v1/embeddings`, which never changes model.

### Reliability (P3)

- An error taxonomy, retries with full-jitter backoff within a
  per-deployment retry budget, fallback plans that prefer provider
  diversity, circuit breakers, rate-limit cooldowns and per-provider
  concurrency limits, all within the request's deadline.
- A stream commits only at its first upstream event, so failures before it
  are retried; refusals are never shopped around.

### Cost accounting, ledger and budgets (P4)

- An exact cost for every request, from decimal prices, in
  `X-Switchyard-Cost-Usd` and a usage ledger in memory or Postgres, with
  tenant and admin usage reports.
- Daily and monthly budgets per tenant and key, in memory or Redis,
  admitted before the call and settled after it; `budgets.fail_mode`.
- A Postgres outage loses no requests and, within the queue, no events.

### Cost-aware routing and policies (P5)

- `model: "auto"` and `auto:<profile>`: the cheapest model whose estimated
  quality for the request clears the profile's bar, with an explanation of
  every decision (`/switchyard/v1/route/explain`, `switchyard route
  explain`) and savings measured against a baseline model.
- Policies with governance and quality rules, header tightening, budget
  soft-profile downgrades and an observe mode.

### Exact and semantic caching (P6)

- A per-policy exact response cache and an opt-in semantic cache, scoped to
  the tenant, key or user, purgeable per tenant, in memory or Redis, and
  failing open. Stream hits are replayed; identical concurrent misses
  share one upstream call.

### Production hardening (P7)

- **Rate limits:** per-key request rates (GCRA) and concurrent-stream caps,
  shared by every replica through the state Redis or kept in memory, with
  `rate_limits.fail_mode`; failed authentications, of API keys and admin
  tokens, limited per client address.
- **Tracing:** OpenTelemetry over OTLP/HTTP protobuf with W3C propagation,
  a span per pipeline step and GenAI attributes, never content; trace IDs
  in every log line.
- **Hot reload** on SIGHUP or `POST /admin/v1/reload`: validated, atomic,
  in-flight requests untouched, restart-only fields reported; a key or
  tenant a reload removes is never served, not even by a request that
  authenticated just before it.
- **Admin hardening:** an admin token of at least 32 characters, required
  off loopback; audit lines for reloads and cache purges, never dropped
  for a full log queue; `/healthz` and `/readyz` on the plain-HTTP admin
  listener too, for healthchecks behind TLS; TLS for the public listener.
- **Logs off the hot path:** a bounded asynchronous log writer.
- **Operations kit:** a compose stack (gateway, two Redis instances,
  Postgres, optional Prometheus, Grafana, Jaeger and TEI), alert rules with
  unit tests, a Grafana dashboard, a read-only, non-root image with a
  built-in healthcheck, a quickstart, a generated configuration reference
  and a runbook.
- **Load:** a 1,000 req/s overhead SLO bench and a soak test at that
  target load, both with rate limits and full tracing on
  ([test/load/RESULTS.md](test/load/RESULTS.md)).
- **Release:** GoReleaser archives for linux and darwin on amd64 and
  arm64, checksums, CycloneDX SBOMs and multi-arch images; actions pinned
  by commit and base images by digest.

### Quality feedback loop (P8)

- `switchyard eval run`, `fit`, `report` and `fit-weights`: measure quality
  priors on your own prompts with a pairwise LLM judge, under a cost cap,
  and write them as `quality_overrides`.
- Shadow sampling of live traffic under a daily budget, client feedback
  (`POST /switchyard/v1/feedback`), and verified savings in the usage
  reports.

### Known limitations

- No live provider check has run yet: the smoke test, fixture recording,
  a live judge-quality check and `make semantic-eval` need provider keys.
- The semantic cache and the shadow runner keep the provider adapters they
  started with until a restart; a deployment removed by a reload stays
  listed in `/admin/v1/providers`.
- Failed-authentication limits are per replica.
