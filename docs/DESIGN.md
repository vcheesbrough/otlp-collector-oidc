# otlp-collector-oidc — design

> An OpenTelemetry Collector distribution that authenticates OTLP clients with OIDC.
> This document is the product's design: what it does, what it refuses to do, and why.
> How the project is run (repository, board, branches, releases) is not in here.

## 1. Purpose

Browsers and mobile apps cannot be trusted to say who they are, and a stock OTLP
collector cannot tell them apart. `otlp-collector-oidc` accepts OTLP from
user-facing clients, authenticates every request against **any OIDC provider**, stamps
the provider-attested identity and the deployment-declared attributes onto the
telemetry, bounds what a client can cost, and forwards plain OTLP to **any** downstream
collector or backend.

It assumes nothing about the consumer's reverse proxy, identity provider,
configuration store or hosting. One image; environment variables; TLS on by default.

## 2. Decisions

| Decision | Choice |
| --- | --- |
| Shape | A custom OpenTelemetry Collector distribution built with `ocb`: stock processors and exporters plus **two custom components** in Go — an authenticator extension and a single-port OTLP receiver |
| Language | Go (see [decisions/0001-go-collector-distribution.md](decisions/0001-go-collector-distribution.md)) |
| Signals | Traces, logs **and metrics**; metrics on a separate pipeline that never carries identity |
| Downstream | Any OTLP endpoint, gRPC or HTTP, TLS optional, configured with the standard `OTEL_EXPORTER_OTLP_*` variables |
| Listener | **One** TLS port serving OTLP/gRPC and OTLP/HTTP; a self-signed certificate is embedded so the image runs with nothing mounted |
| Authentication | OIDC only: a JWT access token as a bearer or, optionally, in a cookie. No anonymous access, no API keys, no shared secrets |
| Publishing | Multi-arch image on `ghcr.io/vcheesbrough/otlp-collector-oidc` |
| Licence | PolyForm Noncommercial 1.0.0 (see `LICENSE`, `LICENSE-TIER.md`) |

Two facts verified against collector source shape the contract: an authenticator
refusal is always **401** with the error text as the body (`config/confighttp/server.go`,
`authInterceptor`) — 403 is not reachable; and `max_request_body_size` bounds the
**decompressed** body.

## 3. Architecture

```
any client ──► [ consumer's proxy, or TLS here ] ──► otlp-collector-oidc ──► any OTLP endpoint
 bearer /                                            ├ oidcclientauth   the authenticator extension
 cookie-JWT                                          ├ otlpsingleport   ONE TLS port: gRPC + HTTP /v1/{traces,logs,metrics}
                                                     ├ traces/logs:  memory_limiter · attributes(identity) · transform · filter · batch
                                                     ├ metrics:      memory_limiter · attributes(static only) · transform · filter · deltatocumulative · batch
                                                     ├ otlp exporter  (gRPC or HTTP, TLS or plaintext, queue + retry)
                                                     └ its own logs ──► the same logs upstream, as OTLP; own metrics on :8888
```

### 3.1 TLS is always on, and the image works with nothing mounted

The image build generates a self-signed certificate (`openssl req -x509`, CN
`otlp-collector-oidc`, SANs `otlp-collector-oidc`, `localhost`, `127.0.0.1`) into
`/etc/otlp-collector-oidc/tls/`, and the listener uses it by default. There is no
plaintext listener. A deployer overrides it by mounting their own pair and setting
`TLS_CERT_FILE` / `TLS_KEY_FILE`.

The embedded certificate provides encryption in transit behind a proxy that
re-encrypts to the container. It carries no identity worth trusting — it is per build
and public — so anything facing users directly mounts a real certificate.

### 3.2 Two deployment modes

- **Behind a reverse proxy.** The proxy terminates the public TLS and forwards HTTPS
  to the container, accepting the self-signed certificate or the mounted one, and
  does the rate limiting. Traefik is the documented proxy
  (`docs/proxies/traefik.md`); any other proxy is the consumer's to configure against
  the generic contract stated on that page: forward to the one port over HTTPS,
  trusting the container's certificate; carry HTTP/2 to the backend so gRPC works;
  route both `/v1/…` and `/opentelemetry.proto.collector…` to it; strip any mount
  prefix from the HTTP paths and never from the gRPC ones; rate-limit by source IP.
- **Direct.** A real certificate mounted, the container reachable itself.

**The product does not rate-limit.** In both modes that is the edge's job, by source
IP. It is the one thing a direct deployment must supply itself.

### 3.3 Health means "this process works", never "my dependencies work"

The `health_check` extension listens on loopback plain HTTP (`127.0.0.1:13133`) and the
image `HEALTHCHECK` probes only that: process up, pipelines running. It does **not**
check the upstream OTLP endpoint. Docker's single probe means "restart me"; an
upstream outage would restart every instance in a loop and discard the in-memory
sending queue that exists to ride out exactly that outage. Upstream health is a metric
and an alert instead: `otelcol_exporter_send_failed_*` and `otelcol_exporter_queue_size`
on `:8888`.

For the same reason the authenticator does not crash-loop on OIDC discovery failure:
it starts, retries discovery in the background, answers **503** (retryable, so clients
back off rather than stop) until the JWKS is loaded, and counts it as `not_ready`.

### 3.4 The product's own logs go upstream, as OTLP

The collector's internal telemetry (`service::telemetry::logs`) gets an OTLP log
exporter pointed where client logs go — the effective logs exporter configuration, so
a per-signal `OTEL_EXPORTER_OTLP_LOGS_*` override is honoured. Every line the process
writes, including the authenticator's rate-limited rejection warnings (with `reason`
as a structured field), arrives in the same store as the client telemetry, identified
as itself: `service.name` from `OTEL_SERVICE_NAME` (default `otlp-collector-oidc`),
`service.version` from the build, and whatever `OTEL_RESOURCE_ATTRIBUTES` adds.

`OTEL_*` describes this process; `CLIENT_RESOURCE_ATTRIBUTES` describes the data it
forwards. The export is asynchronous and best-effort — an unreachable upstream never
delays startup or a request — and stdout keeps its copy so `docker logs` works when
upstream is the thing that is down. Its own metrics stay a Prometheus pull; it emits
no traces of its own.

## 4. The two custom components

### 4.1 `otlpsingleport` — the receiver

Built only on public collector and `pdata` APIs; not a fork of the stock receiver.

- **One `confighttp.ServerConfig` → one TLS listener.** It already advertises `h2`
  and `http/1.1` through ALPN (`confighttp/server.go:266`), and supplies TLS, the
  authenticator interceptor, `max_request_body_size`, gzip/zstd decompression, CORS
  and `include_metadata` — all stock, all reused.
- **Dispatch by request, not by port.** HTTP/2 requests whose `Content-Type` starts
  with `application/grpc` go to `grpc.Server.ServeHTTP` (grpc-go's handler-transport
  mode; it uses the request context, so the auth context set by the interceptor
  reaches the gRPC handlers). `POST /v1/{traces,logs,metrics}` with
  `application/x-protobuf` or `application/json` go to HTTP handlers. Anything else:
  404 / 405 / 415.
- **Decoding is `pdata`'s, not ours:** `ptraceotlp.ExportRequest.UnmarshalProto/JSON`
  and the logs/metrics equivalents; responses marshalled in the requested encoding.
  The gRPC services are `p*otlp.RegisterGRPCServer` with three small `Export`
  handlers. Consumer errors map as the stock receiver's do: permanent → `400` /
  `InvalidArgument`, otherwise `503` / `Unavailable` with a retry hint.
- **Own telemetry:** the standard `receiver_accepted_*` / `receiver_refused_*` via
  `receiverhelper`, so dashboards built for stock receivers read this one unchanged.

**Why the receiver is ours.** The stock OTLP receiver builds a `*grpc.Server` and an
`*http.Server` and calls `net.Listen` twice (`otlpreceiver/otlp.go:117,172`), so it
cannot serve both on one port. The protocols can: ALPN negotiates `h2` / `http/1.1`
on one TLS socket, and `grpc.Server.ServeHTTP` runs gRPC inside an ordinary HTTP/2
server. This receiver is that composition. If upstream ever grows a shared listener,
the stock receiver replaces it with no configuration change.

### 4.2 `oidcclientauth` — the authenticator extension

Implements `extensionauth.Server`.

- **Token source:** `Authorization: Bearer …`, else — when `cookie_name` is set — a
  JWT carried in a cookie of that name. The cookie path exists for browsers: an app
  that keeps the access token in an `HttpOnly` cookie never exposes it to JavaScript,
  and the unload flush via `navigator.sendBeacon` cannot carry a header at all. The
  CSRF position: the app sets `SameSite=Lax` or stricter, and the receiver rejects
  every content type but OTLP's two with 415, so a cross-origin page can send nothing
  the collector would accept without a preflight that CORS-off refuses.
- **Validation** with `github.com/coreos/go-oidc/v3`: discovery at startup with
  background retry on failure (503 `not_ready` meanwhile, never a crash); JWKS cache
  with rotation; `aud` must contain `audience`; RS256/384/512 and ES256/384/512;
  required `exp` / `iss` / `aud`. Then `scope` (or an array `scp`) must contain
  `required_scope`.
- **Required claims** (`required_claims`, default `sub`, `preferred_username`) must be
  present and non-empty; otherwise the 401 body is `missing claim: <name>`.
- **Auth context:** every claim in `claim_attributes` (default `sub→user.id`,
  `preferred_username→user.name`, `email→user.email`, `name→user.full_name`; optional
  ones only when present) plus every key of `resource_attributes`, a static map the
  deployer sets so the deployment, not the client, states them.
- **Own telemetry:** `oidcclientauth_rejections_total{reason}` with a bounded reason
  set (`no_token`, `invalid_token`, `missing_scope`, `missing_claim`, `not_ready`);
  one rate-limited warning per reason.

**`resource_attributes` is optional; `deployment.environment` is its usual content.**
Every key given is **overwritten** on every signal — `attributes` upserts it from
`auth.<key>` onto the span/log/datapoint, `transform` copies it to
`resource.attributes` and deletes the temporary copy — so a dev client cannot label
spans `prod` once the deployment has said which it is. A key not given is left to the
client, like every other resource attribute. Two are deliberately never in this map:
`service.name` is the client's (bounded by `ALLOWED_SERVICE_NAMES`) and
`service.version` is the client build's; the deployment knows neither.

## 5. The pipeline

All stock components, rendered from an embedded template at startup (§6).

- **Traces and logs.** `attributes` upserts identity from `from_context: auth.<key>`;
  `transform` lifts the static resource attributes onto `resource.attributes`, sets
  `telemetry.source=client` (logs also `log_source=otlp`) and clamps far-future
  timestamps; `filter` drops `service.name` outside `ALLOWED_SERVICE_NAMES` and
  far-past spans. No attribute count or length caps: the decompressed body cap and the
  edge rate limit bound cost, and span/log attributes are not labels, so they cannot
  cost cardinality.
- **Metrics — a separate, stricter chain.** No identity action at all, so `user.*` and
  `session.id` cannot reach a datapoint by construction. `keep_keys` allowlists
  datapoint attribute keys (`ALLOWED_METRIC_ATTRIBUTE_KEYS`); `filter` allowlists
  metric names (`ALLOWED_METRIC_NAMES`); `deltatocumulative` has a hard `max_streams`
  so a client inventing series exhausts a counter, not the container; the resource is
  reduced to `service.name` / `service.version` / `deployment.environment` /
  `telemetry.source`. Rejections are counted, so a client build that ships an
  unregistered metric is visible.
- `memory_limiter` first in every pipeline; identity is materialised from context
  **before** `batch`, so no `metadata_keys` on the batcher; `otlp` exporter with
  `sending_queue` and `retry_on_failure`.
- `health_check` on `:13133`; own metrics on `:8888`.

## 6. Configuration

### 6.1 Configuration is rendered, not substituted

Stock `${env:…}` substitution yields strings only: it cannot turn `k=v,k=v` into a
`headers:` map, a comma list into `keep_keys([...])`, or a protocol choice into a
different exporter component. So the binary has a render step: `otlp-collector-oidc run`
reads the environment, renders the embedded `collector.yaml.tmpl`, validates the
result with the collector's own config loader, and starts the collector on it.
Setting `COLLECTOR_CONFIG` to a mounted file skips rendering entirely. The rendered
YAML is written to `/tmp` and logged at debug, so "what config am I actually running"
is one command.

### 6.2 Reference

All values are environment variables. **Required** means the process refuses to start
without it, naming the variable.

**Identity provider**

| Variable | Default | Meaning |
| --- | --- | --- |
| `OIDC_ISSUER_URL` | **required** | Exact `iss` value; discovery at `<issuer>/.well-known/openid-configuration` |
| `OIDC_AUDIENCE` | **required** | Value `aud` must contain — normally the provider's client id |
| `OIDC_DISCOVERY_RETRY` | `30s` | Background retry interval while discovery fails (requests get 503 meanwhile) |
| `REQUIRED_SCOPE` | `telemetry:write` | Must appear in `scope` (or `scp`) |
| `REQUIRED_CLAIMS` | `sub,preferred_username` | Each must be present and non-empty |
| `CLAIM_ATTRIBUTES` | `sub=user.id,preferred_username=user.name,email=user.email,name=user.full_name` | Claim → span/log attribute; absent optional claims are skipped |
| `CLIENT_RESOURCE_ATTRIBUTES` | *(empty = client's values kept)* | `key=value,…` stamped onto every **client** resource, overwriting the client's; `deployment.environment` recommended. Not `OTEL_RESOURCE_ATTRIBUTES`, which describes the collector itself |
| `COOKIE_NAME` | *(empty = off)* | Also accept the JWT from a cookie of this name |
| `CLOCK_SKEW` | `60s` | Tolerance on `exp` / `nbf` |
| `REJECTION_LOG_INTERVAL` | `60s` | At most one warning per reason per interval |

**Listener and TLS**

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `0.0.0.0:4318` | The one listener: TLS; OTLP/gRPC and OTLP/HTTP on the same port, told apart per request (h2 + `application/grpc` → gRPC; `/v1/…` → HTTP). Same authenticator on both; the cookie form is HTTP-only by nature |
| `TLS_CERT_FILE` | `/etc/otlp-collector-oidc/tls/cert.pem` | Embedded self-signed pair unless overridden |
| `TLS_KEY_FILE` | `/etc/otlp-collector-oidc/tls/key.pem` | |
| `MAX_REQUEST_BODY_BYTES` | `4194304` | Cap on the **decompressed** body → 413 |
| `CORS_ALLOWED_ORIGINS` | *(empty = CORS off)* | For browsers on a different origin; comma-separated |

**Payload bounds (spans and logs)**

| Variable | Default | Meaning |
| --- | --- | --- |
| `ALLOWED_SERVICE_NAMES` | **required** | Regex; records whose resource `service.name` does not match are dropped and counted. `service.name` becomes a stream label downstream, so a client inventing names is a cardinality cost — a deployer who wants any name says so explicitly with `.*` |
| `MAX_FUTURE_SKEW` | `5m` | Timestamps further ahead are clamped to now |
| `MAX_PAST_AGE` | `48h` | Spans older than this are dropped (backend ingestion windows) |

**Metrics (separate pipeline, never carries identity)**

| Variable | Default | Meaning |
| --- | --- | --- |
| `ALLOWED_METRIC_NAMES` | *(empty = every client metric dropped)* | Regex allowlist of metric names |
| `ALLOWED_METRIC_ATTRIBUTE_KEYS` | *(empty = every datapoint attribute stripped)* | Comma-separated allowlist of label keys |
| `MAX_METRIC_STREAMS` | `2000` | `deltatocumulative` hard cap; the excess is dropped, not stored |
| `DELTA_MAX_STALE` | `10m` | Forget a stream not seen for this long |

**Resources and batching**

| Variable | Default | Meaning |
| --- | --- | --- |
| `MEMORY_LIMIT_MIB` | `64` | `memory_limiter` hard limit |
| `MEMORY_SPIKE_LIMIT_MIB` | `16` | |
| `BATCH_TIMEOUT` | `5s` | |

**Upstream — the standard `OTEL_EXPORTER_OTLP_*` variables, SDK semantics**

| Variable | Default | Meaning |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | **required** | URL. `http://host:4317` → plaintext, `https://…` → TLS (rendered to `endpoint` + `tls.insecure`) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | `grpc` or `http/protobuf` — selects the exporter component |
| `OTEL_EXPORTER_OTLP_HEADERS` | *(empty)* | `key=value,key2=value2` → `headers:` map — tenant ids, a hosted backend's token |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | `10000` | Milliseconds, per the SDK spec |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | `gzip` | `gzip` or `none` |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | *(system roots)* | CA file → `tls.ca_file` |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` / `_CLIENT_KEY` | *(empty)* | mTLS to upstream |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Plaintext regardless of scheme |
| `UPSTREAM_TLS_INSECURE_SKIP_VERIFY` | `false` | *(no SDK equivalent)* accept an unverifiable upstream certificate |
| `UPSTREAM_QUEUE_SIZE` | `1000` | *(no SDK equivalent)* batches held while upstream is unreachable; beyond it, drop and count |
| `UPSTREAM_RETRY_MAX_ELAPSED` | `60s` | *(no SDK equivalent)* |

**Per-signal overrides, exactly as the SDK spec defines them.** Each of the above has
`OTEL_EXPORTER_OTLP_TRACES_*`, `OTEL_EXPORTER_OTLP_LOGS_*` and
`OTEL_EXPORTER_OTLP_METRICS_*` forms that take precedence for that signal. The
renderer emits one exporter per distinct signal configuration and wires each pipeline
to its own; with no per-signal variable set, all three share one. Per the spec, a
per-signal HTTP endpoint is used verbatim (no `/v1/<signal>` appended), while the base
endpoint gets the signal path appended. Traces can therefore go to Tempo directly and
logs to Loki's OTLP endpoint with no collector between.

**Self-observability and escape hatch**

| Variable | Default | Meaning |
| --- | --- | --- |
| `HEALTH_ADDR` | `127.0.0.1:13133` | Liveness only; never checks upstream |
| `SELF_METRICS_ADDR` | `0.0.0.0:8888` | Prometheus scrape of the collector's own metrics |
| `LOG_LEVEL` | `info` | Applies to both outputs |
| `LOG_FORMAT` | `json` | Stdout encoding, `json` or `console` |
| `LOG_OUTPUT` | `both` | `both`, `otlp` or `stdout`. Own logs go to the logs upstream as OTLP unless `stdout`; stdout keeps a copy unless `otlp`. On a platform that also ships container stdout to the same store, pick one |
| `OTEL_SERVICE_NAME` | `otlp-collector-oidc` | This process's own `service.name` on its logs and metrics |
| `OTEL_RESOURCE_ATTRIBUTES` | *(empty)* | This process's own resource attributes, the SDK convention. Not what is stamped on client data — that is `CLIENT_RESOURCE_ATTRIBUTES` |
| `COLLECTOR_CONFIG` | `/etc/otlp-collector-oidc/collector.yaml` | Mount your own pipeline to replace the shipped one; the custom components are still available to it |

**Trust roots are not a knob.** A provider or upstream signed by a private CA is
handled the standard Go way: mount the CA and set `SSL_CERT_FILE` or `SSL_CERT_DIR`.
`OTEL_EXPORTER_OTLP_CERTIFICATE` exists only because it is a zero-code passthrough of
the exporter's `tls.ca_file` and scoping trust to upstream alone is a real need.

The configuration reference is generated from one source (the config structs and the
annotated template), and CI fails if a variable is used in one and missing from the
other.

## 7. The token profile — the contract with any OIDC provider

`docs/token-profile.md` states exactly what is accepted, so a third party can
configure their provider without reading Go. It is normative; the extension's tests
are its conformance suite; every rejection's 401 body names the failed rule, so a
misconfigured provider diagnoses itself with one `curl`.

- **What:** a JWS compact-serialised **access token** (RFC 9068 shape). An ID token is
  not accepted (it carries no `scope`); an opaque access token is not accepted.
- **Delivery:** `Authorization: Bearer <jwt>`, or — when `cookie_name` is set — a
  cookie of that name whose value is the JWT itself.
- **Header:** `alg` ∈ {RS256, RS384, RS512, ES256, ES384, ES512}; `kid` must match a
  key in the JWKS published via discovery. `none`, HS*, and an unknown `kid` (after one
  JWKS refresh) are rejected.
- **Registered claims:** `iss` — exact string equality with `issuer_url`; `aud` —
  string or array, must contain `audience`; `exp` — required, with `clock_skew`
  tolerance; `nbf` / `iat` honoured when present.
- **Authorisation:** `scope` — a space-delimited string that must contain
  `required_scope`; an array-valued `scp` is accepted as equivalent.
- **Identity, required:** `sub` and `preferred_username`, non-empty → `user.id`,
  `user.name`. **Optional:** `email` → `user.email`, `name` → `user.full_name`. Any
  other claim is ignored.
- **Failure catalogue** (all 401; the body text is the contract): `no token` ·
  `invalid token: <reason>` · `missing scope: telemetry:write` ·
  `missing claim: preferred_username` · `not ready`.

## 8. Provider and proxy guides

`docs/providers/authentik.md` is the reference provider guide and ships a blueprint so
the setup is reproducible: a `telemetry:write` scope mapping; a public OAuth2/OpenID
provider with `authorization_code` + `refresh_token` declared explicitly, a signing
key (what makes the access token a signed JWT), `sub_mode: hashed_user_id` never
changed afterwards, and the `openid` / `profile` / `email` / `telemetry:write`
mappings; an application whose slug fixes the issuer URL; a group with a policy
binding as the "who may send" switch; the collector settings that follow; and a
verification recipe reading the 401 bodies. Other providers get a page each as they
are needed.

`docs/proxies/traefik.md` states the generic proxy contract first (§3.2), then Traefik
exactly: one service on the container's port with a `serversTransport` that accepts
its certificate; a router for `/v1/…` (with `stripprefix` when mounted under a path)
and one for `/opentelemetry.proto.collector` (never stripped); rate limiting on both.

## 9. Acceptance tests

- **Extension unit:** bearer and cookie paths; each required claim missing → the named
  message; optional claims absent → no attribute; `claim_attributes` and
  `resource_attributes` land in the auth context (an empty map means no resource
  action); counter labels bounded; warnings rate-limited.
- **Receiver unit:** dispatch (h2 + `application/grpc` → gRPC; `/v1/*` + protobuf/JSON
  → HTTP; other paths 404, methods 405, content types 415); every signal in every
  encoding, gzip and plain; permanent vs retryable consumer errors → `400` / `503`
  and `InvalidArgument` / `Unavailable`; the auth context is visible inside a gRPC
  handler; `receiver_accepted_*` / `refused_*` counted.
- **Renderer:** golden files per environment shape, each `OTEL_EXPORTER_OTLP_*`
  translation, every golden file loaded by the collector's own validation.
- **Integration** (the built binary as a subprocess, an httptest OIDC issuer,
  in-process gRPC sinks, **HTTPS** with a test certificate, every core scenario over
  both HTTP and gRPC on the same port, and a check that only one port is open):
  OTLP/JSON and gzipped protobuf with bearer and with cookie arrive carrying `user.id`,
  `user.name`, the static resource attributes, `telemetry.source=client`,
  `log_source=otlp`; forged identity and resource values overwritten; `service.name`
  outside `ALLOWED_SERVICE_NAMES` dropped, `.*` admits any; far-future start clamped;
  no/invalid token, missing scope, missing claim → 401 with the named body and
  nothing at the sink; a 5 MiB gzip bomb → 413; a `text/plain` body with a valid
  cookie → 415 (the CSRF position, pinned); sink down → client status unchanged and
  the container stays healthy; issuer unreachable at start → process runs, health
  passes, 503 `not_ready`, recovery without restart. Metrics: an allowlisted metric
  arrives with no `user.*` / `session.*` even when sent; unknown name dropped;
  unknown key removed, datapoint kept; delta counter arrives cumulative; stream
  `max_streams+1` dropped; a table test proves no metrics processor references an
  `auth.` identity key. Per-signal routing: a `LOGS` endpoint override sends logs to a
  second sink while traces reach the first. Own logs: a rejection's warning arrives
  at the logs sink as an OTLP record identified as `otlp-collector-oidc`, following
  the `LOGS` override; with the sink down the process still starts and stdout still
  carries the line.
- **Image:** metadata and health checks; `docker run` with nothing mounted → 401 over
  the embedded certificate; a mounted pair → `curl --cacert` verifies the chain.
- **Behind Traefik** (a real Traefik container): an OTLP/HTTP export and an
  OTLP/gRPC export both reach the sink through the proxy.

## 10. Open questions — verify while building, do not assume

- `grpc.Server.ServeHTTP` is marked experimental in grpc-go: confirm on the pinned
  version that unary `Export` calls, `grpc-encoding: gzip` and `Unauthenticated`
  propagation behave, and that `confighttp`'s decompressor (keyed on
  `Content-Encoding`) and body-size interceptor leave gRPC frames untouched. Fallback:
  a `cmux` on our own listener in front of a stock-style `grpc.Server` — still one
  port.
- `client.FromContext(ctx).Auth` inside the gRPC handlers carries the extension's
  auth data when reached via `ServeHTTP`.
- The `attributes` processor skips an action whose `from_context` key is absent
  (needed for optional claims); otherwise export empty defaults and delete them in
  `transform`.
- OTTL `Now() + Duration(...)` arithmetic in the pinned version.
- The pinned collector supports an OTLP exporter under
  `service::telemetry::logs::processors`; its failures go to the SDK error handler,
  not back into the log pipeline; an unreachable upstream at startup delays nothing.
- The renderer sets `service::telemetry::resource` explicitly; confirm the collector
  does not also read `OTEL_RESOURCE_ATTRIBUTES` and double it, and that
  `OTEL_EXPORTER_OTLP_*` is not picked up for self-traces.
- `deltatocumulative` covers histograms and exponential histograms, not only sums.
- The receiver's `tls:` block reloads a mounted certificate (`reload_interval`); if
  not, document a restart. The non-root runtime user can read the mounted pair.
- authentik: a provider without `signing_key` issues a non-RS or opaque token; `scope`
  on the access token is space-delimited; `hashed_user_id` `sub` is stable across
  provider edits other than `sub_mode`.
