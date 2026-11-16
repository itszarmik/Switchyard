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

# Integration tests (build tag `integration`) run against the Postgres and
# redis-state services of deploy/compose.yaml, started in a compose project
# of their own so that `down -v` never deletes the dev volumes. They use the
# same host ports as `make dev-up`, so stop the dev dependencies first. The
# credentials are the throwaway dev ones from the compose file. The
# packages run one at a time (-p 1), because the Postgres outage test in
# test/integration stops and restarts the test project's Postgres through
# SWITCHYARD_TEST_COMPOSE.
TEST_COMPOSE_PROJECT ?= switchyard-test
INTEGRATION_PKGS     ?= ./internal/usage/... ./internal/budget/... ./test/integration/...
INTEGRATION_FLAGS    ?= -race -count=1 -p 1 -timeout 20m
TEST_POSTGRES_DSN    := postgres://switchyard:switchyard@127.0.0.1:5433/switchyard?sslmode=disable
TEST_REDIS_URL       := redis://127.0.0.1:6380/0

.PHONY: test-integration
test-integration: ## Run the integration tests against Postgres and Redis from deploy/compose.yaml, then tear them down.
	@compose="$(COMPOSE) -p $(TEST_COMPOSE_PROJECT) -f $(COMPOSE_FILE)"; \
	trap '$$compose down -v' EXIT; \
	$$compose up -d --wait postgres redis-state || exit 1; \
	echo "$(GO) test -tags integration $(INTEGRATION_FLAGS) $(INTEGRATION_PKGS)"; \
	SWITCHYARD_TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)' SWITCHYARD_TEST_REDIS_URL='$(TEST_REDIS_URL)' \
	SWITCHYARD_TEST_COMPOSE="$(COMPOSE) -p $(TEST_COMPOSE_PROJECT) -f $(CURDIR)/$(COMPOSE_FILE)" \
		$(GO) test -tags integration $(INTEGRATION_FLAGS) $(INTEGRATION_PKGS)

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

docker-check: docker ## Build the image, then check it is <= 40 MB and runs as UID 65532.
	@size=$$($(DOCKER) image inspect --format '{{.Size}}' $(IMAGE)) || exit 1; \
	user=$$($(DOCKER) image inspect --format '{{.Config.User}}' $(IMAGE)) || exit 1; \
	echo "$(IMAGE): size $$size bytes (limit $(IMAGE_MAX_BYTES)), user '$$user'"; \
	status=0; \
	if [ "$$size" -gt $(IMAGE_MAX_BYTES) ]; then \
		echo "FAIL: image is larger than $(IMAGE_MAX_BYTES) bytes" >&2; status=1; \
	fi; \
	if [ "$${user%%:*}" != "$(IMAGE_UID)" ]; then \
		echo "FAIL: image must run as UID $(IMAGE_UID), got '$$user'" >&2; status=1; \
	fi; \
	if [ $$status -eq 0 ]; then echo "OK: size and user checks passed"; fi; \
	exit $$status

dev-up: ## Start the dev dependencies (Redis x2, Postgres) and wait until healthy.
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --wait

dev-down: ## Stop the dev dependencies (keeps their volumes).
	$(COMPOSE) -f $(COMPOSE_FILE) down

dev-logs: ## Follow the dev dependency logs.
	$(COMPOSE) -f $(COMPOSE_FILE) logs -f

clean: ## Remove build and coverage output.
	rm -rf bin dist coverage.*
