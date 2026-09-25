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
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
RUN addgroup -S -g 10001 otel && adduser -S -D -H -u 10001 -G otel otel \
 && install -d -m 0755 /etc/otlp-collector-oidc /etc/otlp-collector-oidc/tls
COPY --from=build /out/otlp-collector-oidc /usr/local/bin/otlp-collector-oidc
COPY --chown=otel:otel --chmod=0400 --from=tls /tls/key.pem /etc/otlp-collector-oidc/tls/key.pem
COPY --chmod=0444 --from=tls /tls/cert.pem /etc/otlp-collector-oidc/tls/cert.pem
COPY --chmod=0444 config/collector.yaml /etc/otlp-collector-oidc/collector.yaml
USER 10001:10001
EXPOSE 4318 8888
# Liveness only: the process and its pipelines, never the upstream.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --start-interval=1s --retries=3 \
  CMD wget -q -O /dev/null "http://${HEALTH_ADDR:-127.0.0.1:13133}/" || exit 1
ENTRYPOINT ["/usr/local/bin/otlp-collector-oidc"]
CMD ["--config", "/etc/otlp-collector-oidc/collector.yaml"]
