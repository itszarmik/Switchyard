# Changelog

All notable changes to Switchyard. Versions follow [Semantic
Versioning](https://semver.org/); before 1.0, a minor version may change
the configuration or the API.

## v0.1.0 (unreleased)

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
- The CI and release workflows have not run yet. `OWNER` in
  `.goreleaser.yaml`, the image labels and the alert runbook URLs is a
  placeholder for your GitHub owner or registry.
- The semantic cache and the shadow runner keep the provider adapters they
  started with until a restart; a deployment removed by a reload stays
  listed in `/admin/v1/providers`.
- Failed-authentication limits are per replica.
