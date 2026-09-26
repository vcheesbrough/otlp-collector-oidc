# Reference deployment — the maintainer's own estate

> This describes how `otlp-collector-oidc` is deployed in the maintainer's homelab. It
> is an **example**, not a requirement: the product knows nothing of Traefik, authentik,
> sovereign-config or Grafana Alloy. Read it for a worked, real deployment; substitute
> your own equivalents freely.

## Topology

```
browser / phone ──► Traefik (TLS, rate limit by IP)
                       │  Host(<product>) && PathPrefix(/v1/) || PathPrefix(/opentelemetry.proto.collector)
                       ▼
                    otlp-collector-oidc  (one instance per product environment, ghcr image unmodified)
                       │  OTEL_EXPORTER_OTLP_ENDPOINT=http://monitor-alloy:4317
                       ▼
                    monitor-alloy  (the shared Grafana Alloy: OTLP in → Tempo, Loki, Prometheus)
```

One instance per **product environment** — `v-note` production, `v-note` dev, `bored`
production — each on its product's hostname, so `deployment.environment.name` and
the product are fixed by which deployment answered.

## Identity provider

authentik, one OAuth2/OpenID provider per product with a `telemetry:write` scope
mapping, applied from the blueprint in
[`docs/providers/authentik/`](providers/authentik/otlp-collector-oidc.yaml) with one
instance per product environment; see [the guide](providers/authentik.md). The collector's `OIDC_AUDIENCE` is that
provider's client id; `OIDC_ISSUER_URL` is the application slug URL.

## Configuration delivery

The public image is used unmodified. Its variables arrive through the estate's
existing deploy-time render: each product's CI deploy step runs inside the
`sovereign-config-cli` image and wraps the deploy script in
`sovereign-config render /<product>/devops/<env>/compose -- …`, so the store's values
land in the compose service's `environment:` block and the container receives plain
environment variables. The internet-facing container holds no store credential.

- Rendered from the store: `OIDC_ISSUER_URL`, `OIDC_AUDIENCE`, `ALLOWED_SERVICE_NAMES`,
  the metric allowlists.
- Compose literals (environment-fixed, not secret):
  - `OTEL_EXPORTER_OTLP_ENDPOINT: http://monitor-alloy:4317`
  - `CLIENT_RESOURCE_ATTRIBUTES: deployment.environment.name=<env>,telemetry_source=client`
  - `OTEL_RESOURCE_ATTRIBUTES: deployment.environment.name=<env>,telemetry_source=otlp` —
    the homelab's path label, one key on every signal: `docker` and `file` where
    Alloy scrapes, `otlp` on a server's own push, `client` on what this collector
    forwards. The collector's own logs are a server push.
  - `LOG_OUTPUT: otlp` — Alloy already ships every container's stdout to Loki, and
    `both` would store each line twice.
- The embedded certificate is used as-is: Traefik re-encrypts to the container and
  accepts it via a `serversTransport` with `insecureSkipVerify`, exactly as it does
  for the product's own application container.

## Traefik

One service (`port=4318`, `scheme=https`, the self-signed `serversTransport`). Where
the product's own routes leave `/v1/` free (they live under `/api`), one router:

```
Host(`<product>`) && (PathPrefix(`/v1/`) || PathPrefix(`/opentelemetry.proto.collector`))
```

with no prefix stripping; the gRPC client is given `https://<product>` and generates
its own method paths. Where `/v1/` is taken, the HTTP router mounts `/otlp` with
`stripprefix` and the gRPC rule stays separate — still one service. `rate-limit@docker`
on every router. The full label block is
[`examples/compose-behind-traefik/`](../examples/compose-behind-traefik/); the contract
and the reasons are [the proxy guide](proxies/traefik.md).

Observability labels (`observability.service.name`,
`observability.deployment.environment`, `observability.metrics.*` for the `:8888`
scrape), the hardening block (the image's own user `10001` — the one that can read the
embedded key — `cap_drop: ALL`, `read_only` with a `/tmp` tmpfs,
`no-new-privileges`, `mem_limit`), and **never** in the deploy health gate.

## Downstream: the shared Alloy

`monitor-alloy`'s OTLP receiver historically forwarded traces only. For this product
it also outputs **logs** to Loki's OTLP endpoint (`otelcol.exporter.otlphttp "loki"`)
and **metrics** to the existing Prometheus remote-write
(`otelcol.exporter.prometheus`). That change lives in the homelab's monitoring
configuration, with `OBSERVABILITY.md` there recording the contract: every stream
carries `telemetry_source`, which OTLP pushes set themselves; build identity stays
out of metric labels.

## Stopping it

There is no kill switch. Stopping the product environment's collector container
refuses every client of that product's environment at once; clients drop their events
and carry on, other products and other environments are untouched, and server-side
telemetry — which exports to `monitor-alloy` directly — never passes through this
path at all.
