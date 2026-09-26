# Behind a reverse proxy — and Traefik exactly

The collector is one TLS port for OTLP/gRPC and OTLP/HTTP. A proxy in front of it
terminates public TLS and does the rate limiting; the collector does neither of those
for the public side.

## The contract, for any proxy

1. **Forward to the one port over HTTPS**, trusting the container's certificate.
   With nothing mounted that is the image's embedded self-signed pair, generated per
   build: it encrypts the hop and identifies nothing, so accept it without
   verification, or mount a pair your CA signs (`TLS_CERT_FILE` / `TLS_KEY_FILE`) and
   verify it.
2. **Carry HTTP/2 to the backend.** gRPC needs it; the collector offers `h2` by ALPN.
3. **Route both** `/v1/…` (OTLP/HTTP) and `/opentelemetry.proto.collector…` (OTLP/gRPC
   method paths) to that one port.
4. **Strip a mount prefix from the HTTP paths, never from the gRPC ones.** If
   `/v1/` is taken on the host, mount OTLP/HTTP under a prefix such as `/otlp` and
   strip it; gRPC clients generate their own method paths, which must arrive as they
   are.
5. **Rate-limit by source address** on both. The product never rate-limits; body size
   is capped by the collector (`MAX_REQUEST_BODY_BYTES`), request rate is the edge's.

The collector's answers pass through unchanged: a client without a token gets the
collector's `401 no token` (gRPC `Unauthenticated`), not a proxy page.

## Traefik

Tested with Traefik **3.7.13**; the integration suite's behind-Traefik tier runs the
scenario tables through it, unprefixed and mounted at `/otlp`.

- **One service** on the container's port `4318`, `scheme: https`, with a
  `serversTransport` that accepts the embedded certificate. Traefik defines
  serversTransports in its file provider, not in container labels:

  ```yaml
  http:
    serversTransports:
      otlp-collector-oidc:
        insecureSkipVerify: true   # or rootCAs: [your CA] for a mounted pair
  ```

  Traefik negotiates HTTP/2 with the backend by ALPN; no `h2c` scheme is involved.
- **Where `/v1/` is free**, one router, nothing stripped:

  ```
  Host(`telemetry.example.com`) && (PathPrefix(`/v1/`) || PathPrefix(`/opentelemetry.proto.collector`))
  ```

  The gRPC client is given `https://telemetry.example.com`.
- **Where `/v1/` is taken**, two routers on the same service: `PathPrefix(`/otlp/v1/`)`
  with a `stripPrefix` middleware for `/otlp`, and
  `PathPrefix(`/opentelemetry.proto.collector`)` with none. The HTTP client's endpoint
  is `https://app.example.com/otlp`.
- **A `rateLimit` middleware** on every router (by source address, Traefik's default).

[`examples/compose-behind-traefik/`](../../examples/compose-behind-traefik/) is the
complete label block, with `traefik-dynamic.yml` for the transport and the hardening
the product supports: the image's own non-root user (`10001`, the one that can read the
embedded key), `cap_drop: ALL`, a read-only root with `/tmp` as tmpfs (the rendered
configuration is written there), `no-new-privileges`, `mem_limit`. Keep the collector
out of any deploy health gate: its health says the process works, nothing about the
upstream or the provider.
