# syntax=docker/dockerfile:1

# Switchyard container image: a static binary on distroless, running as a
# non-root user. Nothing writes to the filesystem at runtime, so the image
# runs with a read-only root filesystem (docker run --read-only, compose
# read_only: true, Kubernetes readOnlyRootFilesystem). Requires BuildKit.
# Build from the repository root:
#
#   docker build -f deploy/Dockerfile -t switchyard:dev .
#
# or use `make docker`, which also passes VERSION, COMMIT and DATE.
#
# Base images are pinned by the digest of their multi-arch index, so a
# re-pushed tag cannot change the image; the tag stays for readers. To bump
# one, resolve the new tag's index digest, with
#
#   docker buildx imagetools inspect golang:1.27.N     (the "Digest:" line)
#
# and replace tag and digest together, in both Dockerfiles for distroless.

# ---- build ------------------------------------------------------------------
# The build stage runs on the builder's native platform and cross-compiles,
# so multi-platform builds do not need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Only what the binary needs, so edits elsewhere keep this layer cached.
COPY go.mod go.sum ./
COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=dev
ARG COMMIT=
ARG DATE=

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w \
        -X switchyard/internal/version.Version=${VERSION} \
        -X switchyard/internal/version.Commit=${COMMIT} \
        -X switchyard/internal/version.Date=${DATE}" \
      -o /out/switchyard ./cmd/switchyard

# ---- runtime ----------------------------------------------------------------
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

ARG VERSION=dev
ARG COMMIT=

LABEL org.opencontainers.image.title="switchyard" \
      org.opencontainers.image.description="Cost-aware, OpenAI-compatible LLM inference gateway" \
      org.opencontainers.image.source="https://github.com/OWNER/switchyard" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=build /out/switchyard /usr/local/bin/switchyard
COPY configs/quickstart.yaml /etc/switchyard/switchyard.yaml

# The admin listener (/metrics, /healthz, /readyz, /admin/v1/*) keeps its default,
# 127.0.0.1:9090: loopback inside the container, so the image starts with
# the baked-in quickstart config and no admin token, and nothing outside the
# container reaches it. To scrape /metrics from another container, bind it
# to all interfaces (SWITCHYARD_ADMIN_LISTEN=":9090", or server.admin_listen)
# and set server.admin_token, which validation then requires; never publish
# 9090 on a public interface. deploy/compose.yaml does this.

# Numeric UID:GID so Kubernetes runAsNonRoot can verify it without /etc/passwd.
USER 65532:65532

# Only the public port. 9090 is deliberately not exposed, so `docker run -P`
# cannot publish the admin listener on every host interface.
EXPOSE 8080

# Docker and compose use this; Kubernetes ignores it and should probe
# /healthz (liveness) and /readyz (readiness) over HTTP instead. The image
# has no shell or curl, so the binary probes itself: `switchyard
# healthcheck` GETs http://127.0.0.1:8080/healthz and exits 0 on 200.
# With server.tls on, the public listener's certificate does not name
# 127.0.0.1: probe the plain-HTTP admin listener instead, which serves the
# same /healthz and /readyz, by overriding the healthcheck with
# ["/usr/local/bin/switchyard", "healthcheck", "--url", "http://127.0.0.1:9090/readyz"].
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --start-interval=1s --retries=3 \
  CMD ["/usr/local/bin/switchyard", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/switchyard"]
CMD ["serve", "--config", "/etc/switchyard/switchyard.yaml"]
