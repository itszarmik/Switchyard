# Switchyard developer tasks.
#
# Requires GNU make. Works with the make 3.81 that ships with macOS and with
# the make on Ubuntu CI. Run `make` or `make help` to list the targets.

.DEFAULT_GOAL := help

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
DOCKER        ?= docker

BINARY       := bin/switchyard
CONFIG       ?= configs/quickstart.yaml
IMAGE        ?= switchyard:dev
COMPOSE_FILE := deploy/compose.yaml
COVERAGE     := coverage.out

# Limits checked by docker-check. 40 MB = 40,000,000 bytes.
IMAGE_MAX_BYTES := 40000000
IMAGE_UID       := 65532

# Build metadata. The fallbacks keep builds working outside a git checkout
# and in a repository without commits. Each value can be overridden, for
# example `make build VERSION=v0.1.0`. The second assignment expands the
# value once, so every use in this run sees the same DATE.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION := $(VERSION)
COMMIT  := $(COMMIT)
DATE    := $(DATE)

LDFLAGS := -X switchyard/internal/version.Version=$(VERSION) \
	-X switchyard/internal/version.Commit=$(COMMIT) \
	-X switchyard/internal/version.Date=$(DATE)

# Prefer the `docker compose` plugin and fall back to the standalone
# `docker-compose` binary. Evaluated only by the targets that use it.
COMPOSE ?= $(shell $(DOCKER) compose version >/dev/null 2>&1 && echo "$(DOCKER) compose" || echo docker-compose)

.PHONY: help build test cover fmt fmt-check vet lint vuln tidy-check check run \
	docker docker-check dev-up dev-down dev-logs clean bench-overhead smoke

help: ## List the targets.
	@echo "Usage: make <target>"
	@echo
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build bin/switchyard with version metadata.
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/switchyard

test: ## Run the unit tests with the race detector.
	$(GO) test -race -count=1 ./...

cover: ## Write coverage.out and print a per-function summary.
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=$(COVERAGE) ./...
	$(GO) tool cover -func=$(COVERAGE)

bench-overhead: ## Measure the gateway's added latency (pass flags with BENCH_FLAGS="-mode stream").
	$(GO) run ./test/load $(BENCH_FLAGS)

# --- perf (P7): SLO bench and soak ---
#
# bench-slo is the P7 overhead SLO: an open loop at 1,000 req/s for 60 s
# (80% non-streaming, 20% streaming, caching off; a key rate limit and
# stream cap never reached, and tracing of every request to an in-process
# collector) through the gateway binary in front of a fake upstream with
# 20 ms latency, and directly
# against the same fake. It fails if the gateway's added latency, measured
# per request as the client's latency minus the upstream's own time,
# exceeds 2 ms at p50 or 10 ms at p99. Pass flags with BENCH_SLO_FLAGS, for
# example "-rate 2000 -out slo.md"; see test/load's package doc.
#
# Target load is 1,000 req/s, the SLO rate: the soak's default rate.
#
# soak runs the gateway for DURATION at the target load with 2% injected
# upstream 500s, retries off, a per-key rate limit and stream cap it never
# reaches, and tracing of every request to an in-process collector. It
# checks that goroutines and memory stay flat, that the client's errors
# are exactly the injected ones, and that no span, log line or request was
# dropped or rate limited. The report goes to SOAK_OUT; pass other flags
# with SOAK_FLAGS (see test/soak's package doc), for example
# SOAK_FLAGS="-redis-url redis://127.0.0.1:6380/0" for storage.mode redis
# against the test-integration project's redis-state. The warm-up is a
# third of the run, at least until the memory usage ledger is full (110 s
# at 1,000 req/s), so `make soak DURATION=10m` works at the default rate.
BENCH_SLO_FLAGS ?=
DURATION        ?= 1h
SOAK_OUT        ?= bin/soak-report.md
SOAK_FLAGS      ?=

.PHONY: bench-slo soak
bench-slo: ## Check the overhead SLO at 1,000 req/s open loop (p50 <= 2 ms, p99 <= 10 ms added; BENCH_SLO_FLAGS).
	$(GO) run ./test/load slo $(BENCH_SLO_FLAGS)

soak: ## Soak the gateway for DURATION (default 1h) at the 1,000 req/s target load: flat memory and goroutines, errors = injected (SOAK_OUT, SOAK_FLAGS).
	@mkdir -p $(dir $(SOAK_OUT))
	$(GO) run ./test/soak -duration $(DURATION) -out $(SOAK_OUT) $(SOAK_FLAGS)
# --- end perf ---

# --- ops (P7): compose stack, release, config reference, deploy checks ---
#
# compose-up starts the whole stack in deploy/compose.yaml: the gateway,
# both Redis instances and Postgres, plus the optional PROFILES
# (observability, tei). On the first run it writes deploy/.env with
# deploy/init.sh, taking provider keys from the environment. With the
# observability profile, deploy/observability.env turns tracing on. The
# image is built by the docker target, so DOCKER="colima ssh -- docker"
# works here too. compose-down stops the stack and keeps its volumes; add
# COMPOSE_DOWN_FLAGS=-v to delete them.
#
# release-snapshot is a local GoReleaser dry run (.goreleaser.yaml):
# binaries, archives, checksums and SBOMs in dist/, without images (they
# need Buildx; CI builds them on a tag) and without publishing.
#
# docs-config regenerates docs/configuration.md from internal/config/
# types.go; the tools/configdoc test fails while it is stale.
#
# check-deploy validates deploy/: the Prometheus config and alert rules
# and their unit tests (promtool, in the Prometheus image), the Grafana
# dashboard's JSON (jq), that every metric they query exists in the code,
# the compose file with every profile, deploy/config/switchyard.yaml (config
# validate, with placeholder secrets) and deploy/init.sh (sh -n).
PROFILES           ?=
COMPOSE_DOWN_FLAGS ?=
GORELEASER_VERSION := v2.20.2
PROMETHEUS_IMAGE   := prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06
DEPLOY_COMPOSE     = $(COMPOSE) -f $(CURDIR)/$(COMPOSE_FILE) $(foreach p,$(subst $(comma), ,$(PROFILES)),--profile $(p))
comma              := ,

.PHONY: compose-up compose-down release-snapshot docs-config check-deploy
compose-up: build docker ## Start the full stack in deploy/compose.yaml (PROFILES="observability tei"); writes deploy/.env on first run.
	@if [ ! -e deploy/.env ]; then SWITCHYARD_BIN=$(CURDIR)/$(BINARY) deploy/init.sh || exit 1; fi
	$(DEPLOY_COMPOSE) --env-file $(CURDIR)/deploy/.env \
		$(if $(filter observability,$(subst $(comma), ,$(PROFILES))),--env-file $(CURDIR)/deploy/observability.env) \
		up -d --wait

compose-down: ## Stop the stack from compose-up, every profile included (COMPOSE_DOWN_FLAGS=-v deletes the data).
	$(COMPOSE) -f $(CURDIR)/$(COMPOSE_FILE) --profile observability --profile tei down $(COMPOSE_DOWN_FLAGS)

release-snapshot: ## Build a local GoReleaser snapshot in dist/: binaries, archives, checksums, SBOMs (no images, no publishing).
	$(GO) run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean --skip=docker,publish

docs-config: ## Regenerate docs/configuration.md from internal/config/types.go.
	$(GO) run ./tools/configdoc

check-deploy: ## Validate deploy/: alert rules and their tests, dashboard, metric names, compose file, compose config, init.sh.
	$(DOCKER) run --rm -v $(CURDIR)/deploy/prometheus:/etc/prometheus:ro --entrypoint promtool \
		$(PROMETHEUS_IMAGE) check config /etc/prometheus/prometheus.yml
	$(DOCKER) run --rm -v $(CURDIR)/deploy/prometheus:/etc/prometheus:ro -w /etc/prometheus --entrypoint promtool \
		$(PROMETHEUS_IMAGE) test rules alerts_test.yml
	jq -e '(.panels | length > 0) and ([.panels[] | select(.type != "row") | .targets | length > 0] | all) and .uid == "switchyard"' \
		deploy/grafana/dashboards/switchyard.json >/dev/null
	@echo "dashboard: OK"
	sh deploy/check-metrics.sh
	$(COMPOSE) -f $(CURDIR)/$(COMPOSE_FILE) --profile observability --profile tei --env-file /dev/null config --quiet
	@echo "compose: OK"
	@tmp=$$(mktemp -d) || exit 1; trap 'rm -rf "$$tmp"' EXIT; \
	mkdir "$$tmp/config" && cp deploy/config/switchyard.yaml "$$tmp/config/" && ln -s $(CURDIR)/configs/catalog "$$tmp/catalog" && \
	SWITCHYARD_ADMIN_TOKEN=placeholder-admin-token-for-config-validate SWITCHYARD_API_KEY_HASH=sha256:$$(printf '%064d' 0) \
	DATABASE_URL=postgres://placeholder@postgres/switchyard OPENAI_API_KEY=placeholder \
		$(GO) run ./cmd/switchyard config validate -c "$$tmp/config/switchyard.yaml"
	sh -n deploy/init.sh && sh -n deploy/check-metrics.sh
	@echo "check-deploy: OK"
# --- end ops ---

# --- helm (P9): chart lint and render tests, kind install test ---
#
# helm-lint checks the chart in deploy/helm/switchyard without a cluster:
# helm lint --strict with each example values file (ci/*-values.yaml); the
# render tests in test/helm (helm template: secrets only in Secrets, the
# rendered config loads, pod hardening, the derived termination grace,
# the checksum that rolls the pods, the values the chart refuses) and the
# check that the chart's bundled catalog and alert rules are identical to
# configs/catalog and deploy/prometheus/alerts.yml; then kubeconform
# -strict over each example's render for every Kubernetes version in
# HELM_KUBE_VERSIONS (the chart's minimum and the releases supported
# upstream), when kubeconform is on PATH (CI installs it). Each render goes
# to a file first, so a failing helm template fails the check (make's
# /bin/sh may have no pipefail). Its schemas are fetched from the two
# repositories below, pinned to a commit, and cached in KUBECONFORM_CACHE.
#
# helm-test is the end-to-end test on kind: it builds the gateway image
# (the docker target, so DOCKER="colima ssh -- docker" works here too) and
# the fake provider's (test/helm/Dockerfile.fakeupstream), creates the kind
# cluster KIND_CLUSTER from test/helm/testdata/kind.yaml (KIND_REUSE=1 uses
# an existing one instead), loads both images, runs TestKind in test/helm
# (see its package doc for what it checks), and deletes the cluster at the
# end, pass or fail, unless KIND_KEEP=1 or KIND_REUSE=1 (a reused cluster
# is never deleted). The gateway is reached through a
# NodePort published on the host at HELM_E2E_URL. The cluster's
# credentials go to HELM_E2E_KUBECONFIG, never to your ~/.kube/config, so
# the target leaves your current kubectl context alone.
#
# helm-package writes the chart to bin/chart/switchyard-CHART_VERSION.tgz,
# with CHART_VERSION, which must be a semantic version, as both its version
# and its appVersion (so the image tag defaults to vCHART_VERSION). The
# release workflow runs it with the tag's version in its read-only check
# job and hands the file to the release job, whose GoReleaser attaches it.
HELM               ?= helm
KIND               ?= kind
KUBECONFORM        ?= kubeconform
CHART_DIR          := deploy/helm/switchyard
HELM_KUBE_VERSIONS ?= 1.30.0 1.35.0 1.36.0 1.37.0
KUBECONFORM_CACHE  ?= bin/kubeconform-cache
KUBECONFORM_SCHEMAS := \
	-schema-location 'https://raw.githubusercontent.com/yannh/kubernetes-json-schema/88b0235585da4efb0e4bc6cc00baccf0e327e54d/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json' \
	-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/784ec895547a36e548e6810b41f27baade3a0890/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
KIND_CLUSTER       ?= switchyard-helm
KIND_REUSE         ?=
KIND_KEEP          ?=
FAKE_IMAGE         := switchyard-fakeupstream:dev
HELM_E2E_URL       ?= http://127.0.0.1:28080
HELM_E2E_KUBECONFIG ?= $(CURDIR)/bin/helm-test.kubeconfig
HELM_E2E_FLAGS     ?= -v -count=1 -timeout 30m
CHART_VERSION      ?= $(shell awk '$$1 == "version:" { print $$2; exit }' $(CHART_DIR)/Chart.yaml)
# helm-package reads CHART_VERSION from its environment, quoted, rather
# than pasted into its shell line: a release derives it from the tag name.
CHART_VERSION      := $(CHART_VERSION)
export CHART_VERSION
SEMVER_RE          := [0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?

.PHONY: helm-lint helm-test helm-package
helm-lint: ## Lint the Helm chart: helm lint --strict, render tests, bundled-copy drift, kubeconform (if installed).
	@for f in $(CHART_DIR)/ci/*-values.yaml; do \
		echo "$(HELM) lint --strict $(CHART_DIR) -f $$f"; \
		$(HELM) lint --strict $(CHART_DIR) -f "$$f" || exit 1; \
	done
	$(GO) test -count=1 -run 'Render|Bundled' ./test/helm
	@if ! command -v $(KUBECONFORM) >/dev/null 2>&1; then \
		echo "kubeconform: skipped: kubeconform not installed"; exit 0; \
	fi; \
	mkdir -p $(KUBECONFORM_CACHE) || exit 1; \
	render=$$(mktemp) || exit 1; \
	trap 'rm -f "$$render"' EXIT; \
	for f in $(CHART_DIR)/ci/*-values.yaml; do \
		for v in $(HELM_KUBE_VERSIONS); do \
			echo "kubeconform: $$f, Kubernetes $$v"; \
			$(HELM) template sy $(CHART_DIR) -f "$$f" --kube-version "$$v" >"$$render" || exit 1; \
			$(KUBECONFORM) -strict -summary -kubernetes-version "$$v" -cache $(KUBECONFORM_CACHE) $(KUBECONFORM_SCHEMAS) "$$render" || exit 1; \
		done; \
	done

helm-test: docker ## Install the chart on a kind cluster and test it end to end (KIND_CLUSTER, KIND_REUSE=1, KIND_KEEP=1).
	DOCKER_BUILDKIT=1 $(DOCKER) build -f test/helm/Dockerfile.fakeupstream -t $(FAKE_IMAGE) .
	@mkdir -p $(dir $(HELM_E2E_KUBECONFIG)); \
	kubeconfig='$(HELM_E2E_KUBECONFIG)'; \
	if [ -z "$(KIND_REUSE)" ]; then \
		echo "$(KIND) create cluster --name $(KIND_CLUSTER)"; \
		$(KIND) create cluster --name $(KIND_CLUSTER) --config test/helm/testdata/kind.yaml --kubeconfig "$$kubeconfig" --wait 2m || exit 1; \
	else \
		$(KIND) export kubeconfig --name $(KIND_CLUSTER) --kubeconfig "$$kubeconfig" || exit 1; \
	fi; \
	if [ -z "$(KIND_REUSE)$(KIND_KEEP)" ]; then trap '$(KIND) delete cluster --name $(KIND_CLUSTER) --kubeconfig "$$kubeconfig"' EXIT; fi; \
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(IMAGE) $(FAKE_IMAGE) || exit 1; \
	echo "$(GO) test $(HELM_E2E_FLAGS) -run TestKind ./test/helm"; \
	KUBECONFIG="$$kubeconfig" SWITCHYARD_HELM_E2E=1 HELM_E2E_CONTEXT=kind-$(KIND_CLUSTER) HELM_E2E_URL=$(HELM_E2E_URL) \
		HELM_E2E_IMAGE=$(IMAGE) $(GO) test $(HELM_E2E_FLAGS) -run TestKind ./test/helm

helm-package: ## Package the chart as bin/chart/switchyard-CHART_VERSION.tgz (default: Chart.yaml's version).
	@printf '%s\n' "$$CHART_VERSION" | grep -Eqx '$(SEMVER_RE)' || \
		{ echo "helm-package: CHART_VERSION is not a semantic version such as 0.2.0" >&2; exit 1; }
	@mkdir -p bin/chart
	$(HELM) package $(CHART_DIR) --version "$$CHART_VERSION" --app-version "$$CHART_VERSION" --destination bin/chart
# --- end helm ---

.PHONY: demo-routing
demo-routing: ## Run the mixed-workload routing demo against fake upstreams (pass flags with DEMO_FLAGS).
	$(GO) run ./test/demo $(DEMO_FLAGS)

.PHONY: demo-eval
demo-eval: ## Run the quality evaluation demo (eval run, fit, overrides changing routing) against fake upstreams.
	$(GO) run ./test/demoeval $(DEMO_FLAGS)

# eval-live runs `switchyard eval run` against real providers. It is opt-in
# and spends real money, so it needs every variable below and skips without
# them; MAX_COST_USD caps this invocation's spend. JUDGE names the judge
# (--judge; default the config's evaluation.judge.model). Run it with
# DRY_RUN=1 first to see the estimated cost; DRY_RUN=0 (or unset) is a real
# run. The provider keys come from the environment variables the config
# references: without them the run fails at config load, before any call,
# and spends nothing (a dry run needs no keys).
EVAL_PROMPTS ?= test/eval/prompts/starter.jsonl
EVAL_OUT     ?= results.jsonl
JUDGE        ?=
DRY_RUN      ?=

.PHONY: eval-live
eval-live: build ## Run eval against real providers (needs EVAL_CONFIG, EVAL_CANDIDATES, EVAL_REFERENCE, MAX_COST_USD and the config's keys; JUDGE=model, DRY_RUN=1 to estimate).
	@if [ -z "$(EVAL_CONFIG)" ] || [ -z "$(EVAL_CANDIDATES)" ] || [ -z "$(EVAL_REFERENCE)" ] || [ -z "$(MAX_COST_USD)" ]; then \
		echo "eval-live: skipped; set EVAL_CONFIG, EVAL_CANDIDATES, EVAL_REFERENCE and MAX_COST_USD (and the config's provider keys) to run it"; \
		exit 0; \
	fi; \
	$(BINARY) eval run -c "$(EVAL_CONFIG)" --prompts "$(EVAL_PROMPTS)" --candidates "$(EVAL_CANDIDATES)" \
		--reference "$(EVAL_REFERENCE)" --max-cost-usd "$(MAX_COST_USD)" --out "$(EVAL_OUT)" \
		$(if $(strip $(JUDGE)),--judge "$(JUDGE)") $(if $(filter-out 0 false no,$(strip $(DRY_RUN))),--dry-run)

# Integration tests (build tag `integration`) run against the Postgres,
# redis-state and redis-cache services of deploy/compose.yaml, started in a
# compose project of their own so that `down -v` never deletes the dev
# volumes. They use the same host ports as `make dev-up`, so stop the dev
# dependencies first. The credentials are the throwaway dev ones from the
# compose file. The packages run one at a time (-p 1), because the outage
# tests in test/integration stop and restart the test project's Postgres
# and cache Redis through SWITCHYARD_TEST_COMPOSE.
TEST_COMPOSE_PROJECT ?= switchyard-test
INTEGRATION_PKGS     ?= ./internal/usage/... ./internal/budget/... ./internal/cache/... ./internal/ratelimit/... ./test/integration/...
INTEGRATION_FLAGS    ?= -race -count=1 -p 1 -timeout 20m
TEST_POSTGRES_DSN    := postgres://switchyard:switchyard@127.0.0.1:5433/switchyard?sslmode=disable
TEST_REDIS_URL       := redis://127.0.0.1:6380/0
TEST_CACHE_REDIS_URL := redis://127.0.0.1:6379/0
TEST_COMPOSE         := $(COMPOSE) -p $(TEST_COMPOSE_PROJECT) -f $(CURDIR)/$(COMPOSE_FILE)

.PHONY: test-integration
test-integration: ## Run the integration tests against Postgres and Redis from deploy/compose.yaml, then tear them down.
	@compose="$(TEST_COMPOSE)"; \
	trap '$$compose down -v' EXIT; \
	$$compose up -d --wait postgres redis-state redis-cache || exit 1; \
	echo "$(GO) test -tags integration $(INTEGRATION_FLAGS) $(INTEGRATION_PKGS)"; \
	SWITCHYARD_TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)' SWITCHYARD_TEST_REDIS_URL='$(TEST_REDIS_URL)' \
	SWITCHYARD_TEST_CACHE_REDIS_URL='$(TEST_CACHE_REDIS_URL)' SWITCHYARD_TEST_COMPOSE="$(TEST_COMPOSE)" \
		$(GO) test -tags integration $(INTEGRATION_FLAGS) $(INTEGRATION_PKGS)

# bench-cache measures exact-cache hits through the gateway binary with
# storage.mode redis, against the cache and state Redis of the
# test-integration compose project (same ports), which it starts and tears
# down: one entry is warmed, then 20,000 hits are fired at it at each of
# concurrency 1, 8 and 50 (change with BENCH_CACHE_FLAGS). It reports p50,
# p90 and p99 of the client-observed latency per level and fails if p99 at
# concurrency 1 exceeds 5 ms: that measures one hit's latency; 8 and 50 are
# information only, since on a shared machine a closed loop at higher
# concurrency measures queueing and CPU contention rather than the hit.
BENCH_CACHE_FLAGS ?=

.PHONY: bench-cache
bench-cache: ## Measure exact-cache hit latency through the gateway with Redis (fails if p99 > 5 ms at concurrency 1).
	@compose="$(TEST_COMPOSE)"; \
	trap '$$compose down -v' EXIT; \
	$$compose up -d --wait redis-state redis-cache || exit 1; \
	$(GO) run ./test/cachebench -cache-url '$(TEST_CACHE_REDIS_URL)' -state-url '$(TEST_REDIS_URL)' $(BENCH_CACHE_FLAGS)

# semantic-eval runs the semantic cache's paraphrase and near-miss sets
# (test/cache) through a real embedding model: SWITCHYARD_SEMANTIC_CONFIG
# names a config whose semantic_cache.embedding_model is that deployment,
# with its provider key in the environment. It spends a few hundred small
# embedding calls, and skips without the variable.
.PHONY: semantic-eval
semantic-eval: ## Evaluate the semantic cache's test sets against a real embedding model (needs SWITCHYARD_SEMANTIC_CONFIG; skips without it).
	$(GO) test -tags semanticlive -count=1 -v -run TestSemanticLive ./test/cache/

smoke: ## Run the live smoke test against a real provider (needs OPENAI_API_KEY; SMOKE_MODEL optional).
	$(GO) test -tags smoke -count=1 -v ./test/smoke/...

.PHONY: record-fixtures
record-fixtures: ## Record Anthropic API fixtures (needs ANTHROPIC_API_KEY; skips without it).
	$(GO) test -tags record -count=1 -v -run TestRecordFixtures ./test/fakeanthropic/...

fmt: ## Format the code (gofmt, goimports).
	$(GOLANGCI_LINT) fmt

fmt-check: ## Fail if formatting would change anything.
	$(GOLANGCI_LINT) fmt --diff

vet: ## Run go vet.
	$(GO) vet ./...

lint: ## Run golangci-lint.
	$(GOLANGCI_LINT) run

vuln: ## Scan for known vulnerabilities with govulncheck.
	$(GO) tool govulncheck ./...

tidy-check: ## Fail if go.mod or go.sum are not tidy.
	$(GO) mod tidy -diff

check: fmt-check vet lint build test vuln tidy-check ## Run every pre-merge check (no Docker needed).

run: build ## Build, then serve configs/quickstart.yaml.
	$(BINARY) serve --config $(CONFIG)

# The Dockerfile uses BuildKit cache mounts, so this needs the buildx plugin;
# without it, docker build stops with a message saying buildx is missing.
docker: ## Build the container image switchyard:dev.
	DOCKER_BUILDKIT=1 $(DOCKER) build \
		-f deploy/Dockerfile \
		-t $(IMAGE) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		.

# docker-check measures the image's uncompressed size: the sum of its
# layers' sizes as `docker history` reports them, which is the same with
# the classic and the containerd image store (`docker image inspect`
# .Size is the compressed content size under containerd, and the
# unpacked size under the classic store). It then runs the image hardened,
# as compose and Kubernetes should (read-only root filesystem, no
# capabilities, no privilege escalation), with the quickstart config baked
# into the image, and waits up to 30 s for /healthz and /readyz to answer
# 200, probed inside the container by `switchyard healthcheck`.
docker-check: docker ## Build the image, check it is <= 40 MB uncompressed and runs as UID 65532, then run it read-only and probe /healthz.
	@size=$$($(DOCKER) history --human=false --format '{{.Size}}' $(IMAGE) | awk '{ s += $$1 } END { printf "%d", s }') || exit 1; \
	[ -n "$$size" ] && [ "$$size" -gt 0 ] || { echo "FAIL: could not measure the image size" >&2; exit 1; }; \
	user=$$($(DOCKER) image inspect --format '{{.Config.User}}' $(IMAGE)) || exit 1; \
	echo "$(IMAGE): uncompressed size $$size bytes (limit $(IMAGE_MAX_BYTES)), user '$$user'"; \
	status=0; \
	if [ "$$size" -gt $(IMAGE_MAX_BYTES) ]; then \
		echo "FAIL: image is larger than $(IMAGE_MAX_BYTES) bytes" >&2; status=1; \
	fi; \
	if [ "$${user%%:*}" != "$(IMAGE_UID)" ]; then \
		echo "FAIL: image must run as UID $(IMAGE_UID), got '$$user'" >&2; status=1; \
	fi; \
	if [ $$status -eq 0 ]; then echo "OK: size and user checks passed"; fi; \
	exit $$status
	@cid=$$($(DOCKER) run -d --read-only --cap-drop=ALL --security-opt=no-new-privileges $(IMAGE)) || exit 1; \
	trap '$(DOCKER) rm -f '"$$cid"' >/dev/null' EXIT; \
	ok=0; i=0; \
	while [ $$i -lt 60 ]; do \
		if $(DOCKER) exec $$cid /usr/local/bin/switchyard healthcheck --url http://127.0.0.1:8080/healthz 2>/dev/null && \
			$(DOCKER) exec $$cid /usr/local/bin/switchyard healthcheck --url http://127.0.0.1:8080/readyz 2>/dev/null; then ok=1; break; fi; \
		if [ "$$($(DOCKER) inspect --format '{{.State.Running}}' $$cid)" != true ]; then break; fi; \
		sleep 0.5; i=$$((i + 1)); \
	done; \
	if [ $$ok -ne 1 ]; then \
		echo "FAIL: the image did not serve /healthz and /readyz with --read-only --cap-drop=ALL --security-opt=no-new-privileges" >&2; \
		$(DOCKER) logs $$cid >&2; exit 1; \
	fi; \
	echo "OK: runs with --read-only --cap-drop=ALL --security-opt=no-new-privileges; /healthz and /readyz answer 200"

dev-up: ## Start the dev dependencies (Redis x2, Postgres) and wait until healthy.
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait redis-cache redis-state postgres

# dev-down stops and removes only the services dev-up starts: the compose
# project is the full stack's too, and `down` would remove its gateway.
dev-down: ## Stop the dev dependencies (keeps their volumes and any running gateway).
	$(COMPOSE) -f $(COMPOSE_FILE) rm --stop --force redis-cache redis-state postgres

dev-logs: ## Follow the dev dependency logs.
	$(COMPOSE) -f $(COMPOSE_FILE) logs -f

clean: ## Remove build and coverage output.
	rm -rf bin dist coverage.*
