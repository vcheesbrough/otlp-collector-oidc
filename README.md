# otlp-collector-oidc

An [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) distribution
that **authenticates OTLP clients with OIDC**.

Browsers and mobile apps cannot be trusted to say who they are, and a stock collector
cannot tell them apart. This one accepts OTLP — traces, logs and metrics, over gRPC and
HTTP on a single TLS port — validates a JWT access token from **any OIDC provider** on
every request, stamps the provider-attested identity and the deployment-declared
attributes onto the telemetry, bounds what a client can cost, and forwards plain OTLP
to **any** downstream collector or backend. It assumes nothing about your reverse
proxy, identity provider, configuration store or hosting.

> **Status: design stage.** Nothing runs yet. The design is complete and lives in
> [`docs/DESIGN.md`](docs/DESIGN.md); the first code will be the single-port receiver
> spike it calls for.

## Documents

- [`docs/DESIGN.md`](docs/DESIGN.md) — what the product does: architecture, the two
  custom components, the pipeline, the full configuration reference, the token
  profile, acceptance tests, open questions.
- [`docs/decisions/0001-go-collector-distribution.md`](docs/decisions/0001-go-collector-distribution.md)
  — why it is a Go collector distribution rather than a Rust service, an Envoy
  configuration, or an embedded crate.
- [`docs/reference-deployment.md`](docs/reference-deployment.md) — how the maintainer
  deploys it (Traefik, authentik, a shared Grafana Alloy). An example, not a
  requirement.

## Licence

[PolyForm Noncommercial 1.0.0](LICENSE) — see [`LICENSE-TIER.md`](LICENSE-TIER.md).
Commercial use requires a separate licence. The built image bundles Apache-2.0
OpenTelemetry Collector components; this licence covers the custom components,
configuration, build and documentation in this repository.
