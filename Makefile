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
