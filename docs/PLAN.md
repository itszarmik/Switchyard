# Switchyard: Delivery Plan

| | |
|---|---|
| Status | Design approved. **P0–P3 complete** and committed on `main`. Next: P4. CI and the live smoke tests have not run yet. |
| Version | 0.1, 2026-11-09 |
| Companion | [DESIGN.md](DESIGN.md) |

## Working agreement

- **Phases are delivered one at a time.** At the end of each phase I report what changed, give you commands to verify it yourself, share the test results, and note any decisions I made. Then I stop and wait for your go-ahead.
- **Every phase leaves the repo releasable.** It builds, all tests pass, and the docs are current.
- **Scope changes are written into this file first**, so the plan stays the source of truth.

**Definition of done, for every phase:**

- The acceptance criteria are demonstrated by automated tests where that's feasible, and otherwise by a documented manual check.
- `make check` passes cleanly: build, unit tests, lint and `govulncheck`.
- New behavior comes with its metrics and log fields. New config is documented in the reference and the example config.
- CI never calls a real provider. Live smoke tests are opt-in.
- If a decision changed, the decision log in DESIGN §21 is updated.

## Prerequisites (this machine, set up at the start of Phase 0 on 2026-11-09)

| Tool | Status |
|---|---|
| Go ≥ 1.26 | Go 1.27.2 installed with `brew install go` |
| golangci-lint | 2.14.0 installed with `brew install golangci-lint` |
| govulncheck | Pinned as a Go tool in go.mod; run it with `go tool govulncheck` |
| Docker | CLI 29.8 with the Colima context. `colima start` runs the daemon (Docker 29.5). |
| Docker Compose | The standalone `docker-compose` 5.6.0 is installed with Homebrew, and the Makefile falls back to it. Registering the `docker compose` plugin form is optional: add the `cliPluginsExtraDirs` line that Homebrew prints to `~/.docker/config.json`. |
| Postgres 16 | Installed via Homebrew and usable for local development. Tests and `make dev-up` use containers. |
| Redis 8 | Runs in a container (`redis:8`) |
| Provider API keys | Not set up. Optional; needed only for live smoke tests and fixture recording. |

`testcontainers-go` on Colima needs `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`. Both will be documented in the README.

## Milestones

| Milestone | Reached after | What you can do at that point |
|---|---|---|
| **M1** Transparent proxy | P1 | Point an OpenAI SDK at Switchyard and it behaves like OpenAI. |
| **M2** Multi-provider and resilient | P3 | Use one API across OpenAI, Anthropic, Gemini and open-source models, and survive provider outages. |
| **M3** Cost-aware MVP | P5 | Requests go to the cheapest model that's good enough, with cost and savings reports. |
| **M4** v0.1 release | P7 | Cached, observable and hardened, and it starts with `docker compose up`. |

## Phase overview

| Phase | Theme | Size | Depends on |
|---|---|---|---|
| P0 | Foundations | S | none |
| P1 | Transparent OpenAI-compatible proxy | M | P0 |
| P2 | Catalog and multi-provider adapters | L | P1 |
| P3 | Reliability: retries, fallback, circuit breakers | M | P2 |
| P4 | Cost accounting, ledger and budgets | M | P2 |
| P5 | Cost-aware routing, quality scoring and the rule engine | L | P3, P4 |
| P6 | Exact and semantic caching | M | P4 |
| P7 | Production hardening and v0.1 | M | P5, P6 |
| P8 | Quality feedback loop (the first post-MVP phase) | M | P5 |

Sizes are relative (S < M < L), not calendar estimates.

---

## P0: Foundations

**Goal:** an empty but production-shaped service that builds, loads config, logs, serves health checks, and ships as a container.

**Scope**

- **Repo.** `git init`, the Go module, the layout from DESIGN §19, and a Makefile with `build`, `test`, `lint`, `check`, `run` and `dev-up` targets. Also a `golangci-lint` config, and GitHub Actions CI that runs build, test, lint and `govulncheck`.
- **Config.** The schema skeleton and strict YAML decoding. Expansion of `${VAR}`, `${VAR:-default}` and `${file:…}`, plus `include:` globs. Validation that reports every error with its path, the immutable `Snapshot`, and `switchyard config validate`.
- **Server.** Public and admin listeners. Middleware for UUIDv7 request IDs, panic recovery, header and body limits, and the access log. `/healthz` and `/readyz` on the public listener, `/metrics` on the admin listener, and graceful shutdown.
- **Telemetry.** JSON `slog` output with the redacting handler, and a Prometheus registry with build info and HTTP metrics.
- **Packaging.** A multi-stage Dockerfile (distroless, static, non-root), and `deploy/compose.yaml` with the dev dependencies: two Redis instances and Postgres.
- **Docs.** A README with a developer quickstart. DESIGN and PLAN committed.

**Acceptance**

- [x] `make check` passes locally. *CI is unverified until the repo is pushed.*
- [x] `switchyard serve -c configs/quickstart.yaml` starts. `/healthz` returns 200, `/readyz` returns 200 with the config version, and `/metrics` is served only on the admin listener.
- [x] A config with three deliberate mistakes (an unknown key, a bad reference and a missing env var) fails `config validate`, which lists all three with their paths and exits non-zero.
- [x] On SIGTERM, readiness starts failing immediately, a slow in-flight request still completes within `shutdown_grace`, and the process exits 0.
- [x] Every log line for a request carries `request_id`, and a test proves configured secrets never appear in log output.
- [x] The container image builds, runs as non-root, and is ≤ 40 MB.

**P0 outcome.** All criteria are met locally. An independent security review and a correctness review found 17 issues. 16 are fixed with regression tests. The 17th, pinning CI actions and base images to digests, is deferred until the repo has a remote and Dependabot. The image is 4.6 MB and runs as UID 65532. Total test coverage is 91%.

## P1: Transparent OpenAI-compatible proxy

**Goal:** an OpenAI SDK pointed at Switchyard behaves exactly as if it were talking to an OpenAI-wire upstream directly.

**Scope**

- **Canonical types.** The `schema` package: lossless Chat Completions request, response and chunk types that keep unknown fields, plus validation and extraction of the capabilities a request needs.
- **Endpoints.** `POST /v1/chat/completions`, both non-streaming and streaming over SSE, and `GET /v1/models`.
- **`openai` adapter.** Serves OpenAI and any OpenAI-compatible upstream, with a separate HTTP transport per provider and timeouts for connect, first token, idle and total.
- **Model mapping.** A minimal mapping from model name to provider in config, which grows into the catalog in P2.
- **Auth.** Tenants and keys defined in config and stored as SHA-256 hashes, plus `switchyard keys generate`. Every gateway error uses the OpenAI error envelope.
- **Streaming.**
  - Commit on the first upstream event and flush after every chunk.
  - Cancel the upstream when the client disconnects.
  - Always request `stream_options.include_usage` upstream, and forward the usage chunk only if the client asked for it.
- **Fake upstream** in `test/fakeupstream`: a scriptable OpenAI-wire server with configurable latency, TTFT, errors and mid-stream drops.
- **Conformance suite** in `test/conformance`, using the official `openai-go` SDK.
- **Observability.** Access-log fields, plus metrics for requests, durations, upstream outcomes, TTFT and gateway overhead.

**Acceptance**

- [x] The conformance suite passes for plain chat, streaming, tool calls (including parallel tool calls while streaming), JSON mode, `n > 1` and stop sequences. The SDK reports gateway errors (400, 401, 404, 429) the same way it reports OpenAI's: same status, `type` and `code`.
- [x] Non-streaming responses from an OpenAI-wire upstream pass through semantically identical, unknown fields included (golden comparison).
- [x] A client disconnect mid-stream cancels the upstream request within 100 ms, verified against the fake.
- [x] Usage is captured for every streaming request, and clients that didn't ask for `include_usage` don't receive the usage chunk.
- [x] Overhead benchmark (`make bench-overhead`): p99 added latency ≤ 5 ms with 100 concurrent non-streaming requests against a zero-latency fake upstream, on a single machine. The numbers are recorded in `test/load/RESULTS.md`. *Restated from concurrency 200 on 2026-11-11. At 200, the load generator, the fake and the gateway saturate one machine's CPU (8–10.5 ms p99 added), while Switchyard's own processing stays ≤ 0.25 ms p99. That run stays in RESULTS.md as a reference number. Off-box measurement at high load is part of P7's 1,000 req/s criterion.*
- [ ] The opt-in live smoke test (`make smoke`) works against one real OpenAI-compatible provider using your key. *Not verified: no provider key on this machine. The test skips cleanly.*

**P1 outcome.** Independent security and correctness reviews found 11 issues, and all are fixed with regression tests. They included:
- an unauthenticated slow-body attack that pinned connections (high);
- stalled request bodies and responses;
- upstream keep-alive connections discarded after every stream;
- 401s missing from the request metric.

The access log and completion log are now one line per request (DESIGN §15.1). Total test coverage is 91%.

## P2: Catalog and multi-provider adapters

**Goal:** one API across OpenAI, Anthropic, Gemini, OpenAI-compatible hosts and self-hosted servers, where a request is only ever sent to a deployment that can honor it.

**Scope**

- **Catalog.** Models, deployments, prices, capabilities, context windows, quality priors, tags, latency hints and aliases, with validation and `switchyard catalog lint`. An initial curated catalog whose prices are checked against the providers' pricing pages, with each source link and check date recorded.
- **Anthropic adapter.** Every mapping in DESIGN §6.2, including full streaming translation of text and tool-call argument deltas and of usage, plus error mapping (including 529).
- **Other providers.**
  - Gemini through Google's OpenAI-compatible endpoint.
  - Together, Fireworks, vLLM and Ollama through the `openai` adapter, with capabilities declared per deployment.
- **Routing basics.**
  - Requirement extraction drives deployment filtering, and `drop_unsupported_params` is honored.
  - A pinned model is served by its cheapest deployment. `model@provider` pins the provider too.
- **Endpoints.** `POST /v1/completions`, served natively or through the chat shim, and `POST /v1/embeddings`, which may change provider but never model.
- **Fixtures.** A fixture recorder (`make record-fixtures`, opt-in with your keys). Until it has been run, fixtures are handcrafted from provider documentation.

**Acceptance**

- [x] The P1 conformance suite passes unchanged against the Anthropic adapter, using a fake Anthropic upstream that replays fixtures, streaming tool calls included.
- [x] Golden tests cover every row of the Anthropic mapping table: requests, responses, stream events, errors and usage.
- [x] A request that needs a capability a deployment lacks is never sent there. Examples: `n = 2` on Anthropic, images on a text-only model, temperature 1.5 on a model limited to 1.0. It goes to a capable deployment instead, or fails with a 400 that names the reason.
- [x] A model offered by two providers is served by the cheaper one, and `model@provider` pins the other.
- [x] `/v1/completions` works against a chat-only deployment through the shim, marked by `X-Switchyard-Shim: chat`. Legacy parameters the shim can't honor (`echo`, `suffix`, `logprobs`) return 400.
- [x] `/v1/embeddings` never switches to a different model.
- [x] `catalog lint` flags missing prices, quality values outside [0, 1], unknown providers and duplicate IDs.

**P2 outcome.**

- **Conformance through Anthropic:** the P1 conformance suite runs through the binary against a fake Anthropic upstream. Two skips are named and inherently OpenAI-only (unknown request fields and unknown response fields), and n > 1 is asserted as `no_eligible_model`.
- **Catalog:** 11 models across OpenAI, Anthropic, Gemini (compat endpoint) and Together. Prices were verified on 2026-11-11 against the providers' official pricing pages. The self-hosted entry is commented out until the operator supplies an amortized price.
- **New capabilities:** research found `sampling` and `forced_tools`.
  - Several current models reject temperature/top_p or a forced tool_choice.
  - Missing `sampling` always drops temperature and top_p.
- **Capability definition:** `json_schema` means native structured output.
- **Reviews:** independent security and correctness reviews found 11 issues. All are fixed with regression tests. The main ones:
  - a JSON-escaped provider key slipping past the echo guard (high);
  - base64 images counted as text tokens (high);
  - case-folded JSON keys hiding an image from the vision filter.
- **Overhead regression check:** borderline on this machine. The c100 run measured 4.8–5.3 ms p99 added at load averages of 6–21, and A/B runs against the P1 binary under the same load measured the same. Re-check on a quiet machine.
- **Follow-ups:**
  - strict tool `input_schema` is not transformed for Anthropic yet;
  - long-context price tiers and cache-write token accounting are Phase 4.

## P3: Reliability (retries, fallback, circuit breakers)

**Goal:** retries and fallbacks absorb provider failures without causing retry storms, and a stream is never corrupted.

**Scope**

- **Failure handling.**
  - The error taxonomy (DESIGN §14.1).
  - Retries: exponential backoff with full jitter, honoring `Retry-After`, within a per-deployment retry budget.
  - Plans with fallback chains that prefer provider diversity.
  - Circuit breakers per deployment, and cooldowns after a 429.
- **Limits.** Deadline propagation, and per-deployment `max_concurrency` with spillover.
- **Streaming.** Optional SSE keep-alives.
- **Visibility.** The `X-Switchyard-Attempts` header and upstream request IDs, metrics for retries, fallbacks and breakers, and `/admin/v1/providers`.

**Acceptance** (all tested against scripted fake upstreams)

- [x] When the primary returns 503, the fallback serves the request and the client sees 200. Headers and logs show the chain.
- [x] A 429 with `Retry-After: 1` is retried after at least 1 s when the deadline allows. Otherwise the next candidate is tried immediately.
- [x] A 400 is never retried or failed over: exactly one upstream call is made.
- [x] Refusals and content-filter responses are never retried on another provider.
- [x] Nothing is retried after the first byte has been sent. If the upstream drops mid-stream, the client's stream ends with an error event that the SDK raises.
- [x] A breaker opens after the configured number of failures, traffic shifts away, and a half-open probe restores it.
- [x] Total request time never exceeds the deadline by more than 50 ms.
- [x] In a full-outage simulation, retries stay within the retry budget, with no amplification beyond the configured ratio.
- [x] When a self-hosted deployment reaches `max_concurrency`, traffic spills over to the next cheapest deployment.

**P3 outcome.**

All nine criteria pass. Each has its own test that runs the real binary against scripted fake upstreams.

Independent security and correctness reviews found 9 issues, and all are fixed with regression tests. The main ones:

- A slow-reading client could trip a deployment's circuit breaker for every tenant. The idle timer now measures only time spent waiting on the upstream.
- Long non-streaming generations hit the first-token timeout and triggered repeated, billed retries. Non-streaming calls are now bounded by the total timeout, and a total timeout is never retried on the same deployment.
- An Anthropic-side translation limit stopped fallback to other providers.
- A panic could leak concurrency slots and half-open probe slots.

The rules that came out of the fixes:

- A half-open probe is released, and the breaker closed, at the first event.
- Genuine upstream failures after the first event still count toward the breaker.
- A total timeout or a slow client after the first event does not count.

Overhead is about +0.1 ms p99 against P2, measured alternating with the P2 build. The noisy machine fails the 5 ms gate for both builds.

## P4: Cost accounting, ledger and budgets

**Goal:** every request has an exact cost on record, and budgets are enforced without overshoot.

**Scope**

- **Cost.** A calculator covering every token class with snapshotted prices, usage normalization across providers, and estimation when usage is missing.
- **Ledger.**
  - Usage events go through an asynchronous batched sink.
  - The Postgres sink has embedded migrations and monthly partitions; there's also a memory sink.
- **Reporting.**
  - The tenant-scoped reporting API and the admin usage API, grouped by day, model, provider, key, tag, profile or cache status.
  - `X-Switchyard-Tags` for cost attribution.
- **Budgets.** Scopes and windows; Lua scripts for admission and settlement; the soft-threshold hook; the `block` and `notify` actions; `fail_mode`.
- **Visibility.** Cost and token metrics, and the `X-Switchyard-Cost-Usd` header.
- **Savings fields.** Recorded from the start. Until P5 adds `auto` and observe mode, the baseline is the requested model.

**Acceptance**

- [ ] For each provider, fixture requests produce costs equal to hand-computed decimals, including cached input and cache writes.
- [ ] Over a test run, the sum of ledger costs equals the `cost_usd_total` metric to within 1e-6.
- [ ] With 1,000 concurrent requests against a small budget and a fake upstream that reports exactly the estimated usage, final spend never exceeds the limit. When usage is under-estimated, the overshoot is at most the sum of the admitted requests' under-estimates. Rejected requests get `429 budget_exceeded` with `Retry-After`.
- [ ] Stopping Postgres for 60 s under load fails no requests. Queued events land after recovery, and an overflow (forced with a tiny queue in the test) increments the drop counter.
- [ ] Reports return correct aggregates for seeded data. Tenant A can't read tenant B's usage, verified with both API tests and store-layer tests.

## P5: Cost-aware routing, quality scoring and the rule engine

**Goal:** each request goes to the cheapest deployment that meets its requirements, the decision can be explained, and the savings are measured.

**Scope**

- **Policies.** The full policy model from DESIGN §13, first-match rules, and header tightening.
- **Profiles and virtual models.** Profiles (constraints plus an objective), `auto` and `auto:<profile>`, and aliases exposed as virtual models in `/v1/models`.
- **Quality.** The heuristic assessor (signal weights in config) with client hints, the estimator from DESIGN §10.4, and the latency tracker (EWMAs seeded from the catalog).
- **Router.** Filters, estimates, soft checks with relaxation, ranking, and the plan.
- **Modes.**
  - Substitution modes `pinned` and `equivalent`.
  - `routing_mode: observe`.
  - Downgrading to the soft profile when a budget's soft threshold is crossed.
- **Savings and explainability.**
  - Savings against the baseline, broken into routing, cache and overhead.
  - The explain endpoint and the `switchyard route explain` CLI.
  - Route metrics and log fields.

**Acceptance**

- [ ] Table-driven router tests show three things: the chosen deployment satisfies every hard constraint, no cheaper eligible deployment exists in the same preference tier, and every exclusion carries the correct reason.
- [ ] A property test over random catalogs and requests checks the first two invariants.
- [ ] Headers can tighten the policy but never loosen it. Requests for a denied model, a higher cost cap or a disallowed profile are ignored with a warning header, or rejected.
- [ ] On a committed sanity set of about 100 labeled prompts, at least 80 % land in their labeled difficulty band. Test output lists the misclassified prompts for tuning.
- [ ] Mixed-workload demo (`make demo-routing`): with `auto:balanced`, easy prompts go to the cheap tier and hard ones to stronger tiers, and the usage API reports savings against the baseline.
- [ ] In observe mode, 100 % of responses come from the requested model (or the baseline for `auto`), and projected savings are reported separately.
- [ ] Routing adds ≤ 1 ms at p99 with a 200-deployment catalog, measured by a benchmark.
- [ ] `switchyard route explain` (offline) and the explain endpoint agree, given the same input and snapshot.
- [ ] Crossing a budget's soft threshold switches to the soft profile, and the switch is visible in explanations and metrics.

## P6: Exact and semantic caching

**Goal:** repeated and near-repeated requests are served from cache, safely.

**Scope**

- **Exact cache.**
  - Canonical key derivation, with tenant, key and end-user scopes and namespace versions.
  - Stored in Redis with TTLs, a size cap, opt-outs and `deterministic_only`.
- **Streaming and coalescing.** Stream replay, aggregate-then-store for streamed responses, and singleflight coalescing.
- **Semantic cache.**
  - An embedding client that uses the catalog's embedding deployment.
  - The partition fingerprint, and a Redis query-engine index with a versioned name.
  - The similarity threshold and a timeout that fails open.
- **Operations.** Memory implementations, the purge API, and cache savings attribution.
- **Visibility.** Cache metrics including similarity histograms, and embedding cost recorded as overhead in the ledger.

**Acceptance**

- [ ] Key tests: identical requests get the same key, and every output-affecting field changes it, while `stream` and `user` don't. Different tenants never share entries.
- [ ] An exact hit takes ≤ 5 ms at p99 with local Redis, and the ledger shows a cost of 0 with full baseline savings.
- [ ] A streaming client receives a replay that the `openai-go` SDK parses into the same final message as the original response.
- [ ] Semantic cache: a committed paraphrase set hits at the default threshold with the default embedding model, and a committed near-miss set (the same question about different entities) doesn't.
- [ ] With Redis stopped, requests still succeed, cache errors are counted, and the added latency is at most the lookup timeout.
- [ ] 100 identical concurrent non-streaming requests make exactly 1 upstream call.
- [ ] Purging removes all of one tenant's entries (by bumping the namespace version) without touching other tenants.

## P7: Production hardening and v0.1

**Goal:** something you can run in production and hand to people who self-host.

**Scope**

- **Tracing.** OpenTelemetry over OTLP, with W3C propagation and GenAI attributes.
- **Limits.** Per-key rate limits (GCRA) and caps on concurrent streams.
- **Config and admin.** Hot reload, through SIGHUP and the admin endpoint, and admin hardening.
- **Security review** against DESIGN §17: redaction, limits, header allow lists and TLS options.
- **Operations kit.**
  - A Grafana dashboard and Prometheus alert rules.
  - The full `compose.yaml`: gateway, two Redis instances and Postgres, with optional Prometheus, Grafana, Jaeger and TEI.
- **Load testing.** Load tests and a 1-hour soak test.
- **Docs.** Operator docs: a quickstart, the config reference, and a runbook that covers the degraded modes.
- **Release.** GoReleaser producing multi-arch binaries and images, checksums and an SBOM.

**Acceptance**

- [ ] On a fresh machine, following the README, `docker compose up` plus one provider key gives a working gateway in under 5 minutes.
- [ ] A single trace shows the auth, cache, route, upstream-attempt and usage spans for a request.
- [ ] An invalid reload is rejected and the old config keeps serving. A valid reload takes effect without dropping in-flight streams.
- [ ] Over a 1-hour soak at target load, memory and goroutine counts stay flat, and the error rate matches the injected upstream error rate.
- [ ] The overhead SLO holds at 1,000 requests/s on documented hardware: p50 ≤ 2 ms and p99 ≤ 10 ms.
- [ ] Rate limits are enforced across two replicas that share Redis.
- [ ] `govulncheck` is clean, the image runs as non-root with a read-only root filesystem, and an automated test confirms no secrets appear in logs.
- [ ] The v0.1.0 release artifacts are tagged.

## P8: Quality feedback loop (the first post-MVP phase)

**Goal:** replace hand-set quality priors with measurements, and verify that the savings don't cost quality.

**Scope**

- **`switchyard eval`.**
  - Runs prompt sets against candidate models and has an LLM judge compare each answer pairwise with a reference model.
  - Produces a per-category quality estimate with confidence intervals and writes it to a catalog overrides file.
- **Shadow sampling.** An opt-in sampling rate and a budget cap; the calls run asynchronously and are judged offline.
- **Feedback.** `POST /switchyard/v1/feedback`.
- **Tuning and reporting.** Fitting the assessor's weights on labeled data, and a quality report.

**Acceptance**

- [ ] Running eval on a provided prompt set produces a report and an overrides file, and the overrides change routing in a demonstrable way.
- [ ] Shadow sampling never exceeds its budget, and its results join to ledger rows.
- [ ] For categories with enough shadow data, observe-mode projected savings can be shown as verified.

---

## Post-MVP backlog

- **SaaS:**
  - Tenants, keys and policies stored in Postgres, with an admin API.
  - Bring-your-own-key with envelope encryption through a KMS.
  - Per-tenant custom endpoints with SSRF guards.
  - A principal cache with revocation over pub/sub.
  - A control-plane UI, billing export, and a ClickHouse usage sink.
- **Platform:**
  - A Helm chart, a native Gemini adapter, and the Responses API (a stateless subset).
  - Normalizing reasoning parameters across providers.
  - Conversation stickiness and prefix affinity, and automatic Anthropic `cache_control`.
  - Hedged requests for latency profiles, and request coalescing across replicas.
  - Opt-in, sampled, redacted payload logging, and per-tenant cache encryption.
  - Price sync from provider pricing pages.
- **Routing:**
  - CEL rule expressions and policy inheritance (`extends`).
  - A learned estimator sidecar, cascades, and an LLM-based assessor used only when the economics justify it.
  - Exploration traffic to keep latency statistics fresh.
