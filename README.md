# otlp-collector-oidc

An OpenTelemetry Collector distribution that authenticates OTLP clients with OIDC.

[![CI](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/ci.yml?query=branch%3Amain)
[![Release](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/release.yml/badge.svg?branch=main)](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/release.yml?query=branch%3Amain)
[![tests](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2Fvcheesbrough%2Fb99dc03519c995041f79014fea22c17b%2Fraw%2Fotlp-collector-oidc-tests.json)](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/ci.yml?query=branch%3Amain)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fgist.githubusercontent.com%2Fvcheesbrough%2Fb99dc03519c995041f79014fea22c17b%2Fraw%2Fotlp-collector-oidc-coverage.json)](https://github.com/vcheesbrough/otlp-collector-oidc/actions/workflows/ci.yml?query=branch%3Amain)
[![release](https://img.shields.io/github/v/tag/vcheesbrough/otlp-collector-oidc?sort=semver&label=release)](https://github.com/vcheesbrough/otlp-collector-oidc/tags)
[![licence](https://img.shields.io/badge/licence-PolyForm%20Noncommercial%201.0.0-blue)](LICENSE)

It accepts OTLP traces, logs and metrics from user-facing clients — browsers, mobile
apps — over gRPC and HTTP on a single TLS port, validates a JWT access token from any
OIDC provider on every request, stamps the provider-attested identity onto the
telemetry, and forwards plain OTLP to any downstream collector or backend. It is
pre-release: see [Status](#status) for what exists today, and the
[design](docs/DESIGN.md) for the whole.

It refuses anything it cannot attribute: no anonymous access, no API keys, no shared
secrets, no cookies — a bearer token or nothing. It never lets a client label its own
identity or deployment, and it never lets client metrics carry identity or invent
unbounded series.

It assumes nothing about your reverse proxy, identity provider, configuration store or
hosting. One image, configured with environment variables, TLS on by default.

## Quick start

```sh
docker run --rm -p 4318:4318 \
  -e OIDC_ISSUER_URL=https://idp.example.com/application/o/telemetry/ \
  -e OIDC_AUDIENCE=your-client-id \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://your-collector:4317 \
  ghcr.io/vcheesbrough/otlp-collector-oidc:edge
```

```sh
curl -k https://localhost:4318/v1/traces \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"hello","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}'
# {"partialSuccess":{}}
```

`$ACCESS_TOKEN` is a JWT access token from that provider carrying the
`telemetry:write` scope; [the token profile](docs/token-profile.md) states exactly
what is accepted. Without one the answer is `401` and names what is wrong —
`{"code":16,"message":"no token"}` — and until the provider's keys have loaded it is
`503` `not ready` with `Retry-After`. `-k` accepts the image's embedded self-signed
certificate. OTLP/gRPC clients use the same port and get the same answers as
`Unauthenticated` and `Unavailable`.

## Configuration

Every setting is an environment variable; the
[configuration reference](docs/configuration.md) lists each with its default. Three
are required: `OIDC_ISSUER_URL`, `OIDC_AUDIENCE` and `OTEL_EXPORTER_OTLP_ENDPOINT`
(`http://` for a plaintext upstream, `https://` for TLS). A missing required variable
or a value that does not parse stops the process before it listens, and the error
names the variable and the value.

The image runs `otlp-collector-oidc run`, which renders the pipeline from the
environment into `/tmp/otlp-collector-oidc.yaml` and starts on it; `LOG_LEVEL=debug`
also logs it. To replace the pipeline entirely, mount a collector configuration and
point `COLLECTOR_CONFIG` at it: nothing is rendered, and the custom components remain
available to it.

## Deploy

**Behind a reverse proxy.** The proxy terminates public TLS and forwards HTTPS to the
container over HTTP/2 (gRPC needs it), trusting the container's certificate, and does
the rate limiting. Route both `/v1/…` and `/opentelemetry.proto.collector…` to the one
port.

**Direct.** Mount a real certificate and key and point `TLS_CERT_FILE` / `TLS_KEY_FILE`
at them. The product does not rate-limit; a direct deployment supplies that itself.

There is no plaintext listener. The embedded certificate is generated per image build
and is public: it encrypts the hop from a proxy, it identifies nothing. The non-root
runtime user (uid 10001) must be able to read a mounted pair; it is re-read every
`TLS_RELOAD_INTERVAL`, so a rotated certificate needs no restart. An upstream or
provider signed by a private CA is trusted by mounting the CA and setting
`SSL_CERT_FILE`. With a read-only root filesystem, mount a writable `/tmp`.

## Observability

- **Health:** `http://127.0.0.1:13133/` answers while the process and its pipelines
  run; the image `HEALTHCHECK` probes it. It never checks the upstream or the identity
  provider, so an outage of either does not restart the container: an upstream outage
  is ridden out by the queue, a provider unreachable at start by answering `503`
  until discovery succeeds.
- **Metrics:** Prometheus on `:8888/metrics` — `otelcol_receiver_accepted_*` and
  `otelcol_receiver_refused_*` per transport (`grpc`, `http`);
  `otelcol_otlpsingleport_requests_refused` for requests refused before the
  pipeline, by `transport` and `reason` (`method`, `media_type`, `body_too_large`,
  `decode`, `unknown_path`, `decompress`);
  `otelcol_oidcclientauth_rejections` for requests the authenticator refused, by
  `reason` (`no_token`, `invalid_token`, `missing_scope`, `missing_claim`,
  `not_ready`); and `otelcol_exporter_send_failed_*` and `otelcol_exporter_queue_size`
  for upstream health.
- **Logs:** stdout, JSON unless `LOG_FORMAT=console`. The first line records whether
  the configuration was rendered or mounted, and the value every variable took. A
  refusal logs one warning per reason per `REJECTION_LOG_INTERVAL`, with `reason` as
  a field and the count it suppressed; never the token.

## Development

Requires Go 1.27, [golangci-lint](https://golangci-lint.run) v2.14 and Docker.

```sh
make build         # bin/otlp-collector-oidc, version from git tags
make lint          # gofumpt/gci diff and the full linter set
make test          # unit tests
make integration   # the built binary, driven from outside over both protocols
make check         # lint, vet, unit tests and the drift checks
make docs          # regenerate docs/configuration.md after changing a variable
make image         # the container image
```

Integration tests are the primary tier: `integration/` runs the binary as a subprocess
and observes it only from outside — its port, a fake upstream, `:8888`, health and
output. In CI, the **Test report** check on each commit and PR lists pass, fail and
skip counts and names every failing test; the run summary adds coverage merged from the
unit and integration tiers, and the `test-reports` artifact holds the JUnit files.

## Status

Pre-release (`0.x`): single-port OTLP/gRPC and OTLP/HTTP over TLS, every request
authenticated with an OIDC access token, configured entirely by environment, forwarding
traces and logs to an OTLP/gRPC upstream over plaintext or TLS. The identity is
validated but not yet stamped onto the telemetry; identity stamping, the metrics
pipeline, the rest of the `OTEL_EXPORTER_OTLP_*` family (HTTP, headers, per-signal
endpoints) and its own logs upstream follow —
[board](https://bored.desync.link/boards/otlp-collector-oidc).

## Licence

[PolyForm Noncommercial 1.0.0](LICENSE) — see [`LICENSE-TIER.md`](LICENSE-TIER.md).
Commercial use requires a separate licence. The built image bundles Apache-2.0
OpenTelemetry Collector components; this licence covers the custom components,
configuration, build and documentation in this repository.
