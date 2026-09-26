# syntax=docker/dockerfile:1

# Build: the collector distribution. components.go is ocb's output, committed
# and drift-checked in CI; the binary is `go build` of cmd/otlp-collector-oidc
# so the version comes from the linker.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev+unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/vcheesbrough/otlp-collector-oidc/internal/build.version=${VERSION}" \
      -o /out/otlp-collector-oidc ./cmd/otlp-collector-oidc
# The licence and notice files of every module the binary links, most of
# them the Apache-2.0 OpenTelemetry Collector components.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH scripts/third-party-notices.sh /out/licenses/third-party

# TLS: a self-signed pair generated at image build, so the image serves TLS
# with nothing mounted. It is per build and public: encryption in transit
# behind a proxy, never an identity worth trusting.
FROM alpine:3.24 AS tls
RUN apk add --no-cache openssl \
 && mkdir -p /tls \
 && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
      -subj "/CN=otlp-collector-oidc" \
      -addext "subjectAltName=DNS:otlp-collector-oidc,DNS:localhost,IP:127.0.0.1" \
      -keyout /tls/key.pem -out /tls/cert.pem

FROM alpine:3.24
ARG VERSION=0.0.0-dev+unknown
ARG REVISION=unknown
LABEL org.opencontainers.image.title="otlp-collector-oidc" \
      org.opencontainers.image.description="OpenTelemetry Collector distribution that authenticates OTLP clients with OIDC" \
      org.opencontainers.image.source="https://github.com/vcheesbrough/otlp-collector-oidc" \
      org.opencontainers.image.url="https://github.com/vcheesbrough/otlp-collector-oidc" \
      org.opencontainers.image.documentation="https://github.com/vcheesbrough/otlp-collector-oidc/blob/main/README.md" \
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
RUN addgroup -S -g 10001 otel && adduser -S -D -H -u 10001 -G otel otel \
 && install -d -m 0755 /etc/otlp-collector-oidc /etc/otlp-collector-oidc/tls
COPY --from=build /out/otlp-collector-oidc /usr/local/bin/otlp-collector-oidc
# This product's licence, and every bundled module's, where images keep them.
COPY LICENSE LICENSE-TIER.md /usr/share/licenses/otlp-collector-oidc/
COPY --from=build /out/licenses/third-party /usr/share/licenses/otlp-collector-oidc/third-party
COPY --chown=otel:otel --chmod=0400 --from=tls /tls/key.pem /etc/otlp-collector-oidc/tls/key.pem
COPY --chmod=0444 --from=tls /tls/cert.pem /etc/otlp-collector-oidc/tls/cert.pem
USER 10001:10001
EXPOSE 4318 8888
# Liveness only: the process and its pipelines, never the upstream.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --start-interval=1s --retries=3 \
  CMD wget -q -O /dev/null "http://${HEALTH_ADDR:-127.0.0.1:13133}/" || exit 1
# run renders the configuration from the environment (docs/configuration.md)
# into /tmp and starts the collector on it.
ENTRYPOINT ["/usr/local/bin/otlp-collector-oidc"]
CMD ["run"]
