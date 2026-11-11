# syntax=docker/dockerfile:1

# Switchyard container image: a static binary on distroless, running as a
# non-root user. Requires BuildKit. Build from the repository root:
#
#   docker build -f deploy/Dockerfile -t switchyard:dev .
#
# or use `make docker`, which also passes VERSION, COMMIT and DATE.

# ---- build ------------------------------------------------------------------
# The build stage runs on the builder's native platform and cross-compiles,
# so multi-platform builds do not need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build

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
FROM gcr.io/distroless/static-debian13:nonroot

ARG VERSION=dev
ARG COMMIT=

LABEL org.opencontainers.image.title="switchyard" \
      org.opencontainers.image.description="Cost-aware, OpenAI-compatible LLM inference gateway" \
      org.opencontainers.image.source="https://github.com/OWNER/switchyard" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=build /out/switchyard /usr/local/bin/switchyard
COPY configs/quickstart.yaml /etc/switchyard/switchyard.yaml

# The admin listener (/metrics, /admin/v1/*) defaults to 127.0.0.1:9090. In a
# container, loopback is unreachable from outside the container's network
# namespace, so Prometheus could not scrape it. Bind all interfaces inside
# the container instead, and keep the port private: never publish 9090 to a
# public interface (use `-p 127.0.0.1:9090:9090` or a private network).
ENV SWITCHYARD_ADMIN_LISTEN=":9090"

# Numeric UID:GID so Kubernetes runAsNonRoot can verify it without /etc/passwd.
USER 65532:65532

# Only the public port. 9090 is deliberately not exposed, so `docker run -P`
# cannot publish the admin listener on every host interface.
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/switchyard"]
CMD ["serve", "--config", "/etc/switchyard/switchyard.yaml"]
