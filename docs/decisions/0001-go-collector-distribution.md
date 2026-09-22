# 0001 — Go, as a custom OpenTelemetry Collector distribution

**Status:** accepted, 2026-09-22.

## Decision

`otlp-collector-oidc` is built with the OpenTelemetry Collector Builder (`ocb`) as a
custom distribution: stock processors and exporters plus two custom components in Go —
an authenticator extension (`oidcclientauth`) and a single-port OTLP receiver
(`otlpsingleport`).

## Alternatives considered

**Rust — a bespoke service** (axum + tonic + `opentelemetry-proto`). The maintainer's
other products are Rust, so every house pattern — config, auth, observability,
deployment — would transfer. Smallest footprint (~20 MB image), full control of
status codes. Rejected because it owns ~3–5k lines including two genuinely tricky
pieces — OTLP/JSON compatibility with every SDK's encoder, and a correct
delta→cumulative aggregator — and offers no consumer-configurable pipeline. It is a
bespoke proxy, not a collector, and would be maintained as one. **Recorded as the
fallback:** if the single-port receiver cannot be made sound on either the
`ServeHTTP` path or the `cmux` path, the collector framework has stopped paying for
itself.

**Envoy `jwt_authn` in front of an existing collector.** Zero custom code: Envoy does
OIDC (JWKS, cookie extraction, RBAC on claims), rate limiting and a single listener;
the consumer's collector stamps identity from headers. Rejected on one fact: Envoy
never parses OTLP, so every payload rule — bounding `service.name`, the metric name
and key allowlists, the stream cap, timestamp clamping, stamping onto resources —
must live in a collector configured per deployment anyway. The Go distribution *is*
that collector, with authentication inside it.

**An embedded Rust crate** each axum application mounts at `/otlp`, reusing the
app's authentication, configuration, TLS and deployment. Around 1–1.5k lines, and it
would delete most of this product's surface. Rejected on audience alone: it serves
Rust applications the maintainer owns, and the product is for anyone on any stack.
If that audience ever narrows, this is the design to return to.

**Vector, Fluent Bit, Grafana Alloy, the experimental Rust collector
(`otap-dataflow`), proxy forward-auth.** No OIDC, no plugin model, or no direct
(proxy-less) deployment mode.

## Why Go wins

Least code owned where mistakes are expensive. Delta→cumulative, OTTL clamps and
allowlists, queue and retry, memory limiting and internal telemetry are stock and
correct; the custom surface is roughly 1.2k lines of production Go; the consumer can
replace the whole pipeline via `COLLECTOR_CONFIG`; and "a collector distribution" is
a category people already know how to run and dashboard.

Its costs are known and bounded: `grpc.Server.ServeHTTP` is experimental (the
receiver is built and proven first, with a `cmux` fallback that keeps one port);
authenticator refusals are 401-only, which the client rule absorbs; and Go is a second
language in a Rust estate.

## Performance did not decide it

Per request, the dominant cost is JWT signature verification (~30–100 µs for RS256),
identical in every design, followed by collector work that an Envoy-fronted design
would still leave to a collector. Envoy's C++ dispatch is faster than the Go HTTP
path, and grpc-go's `ServeHTTP` is slower than its native transport, but those are
tens of microseconds against a shared budget of hundreds — and two processes idle at
more memory than one. At the load this product will see, the difference is not
measurable. If it ever matters, the lever is a validated-token cache in the extension
(LRU by token hash, TTL bounded by `exp`).
