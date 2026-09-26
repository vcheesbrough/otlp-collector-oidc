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
| Authentication | OIDC only: a JWT access token as a bearer. No cookies, no anonymous access, no API keys, no shared secrets |
| Publishing | Multi-arch image on `ghcr.io/vcheesbrough/otlp-collector-oidc` |
| Licence | PolyForm Noncommercial 1.0.0 (see `LICENSE`, `LICENSE-TIER.md`) |

Two facts verified against collector source shape the contract. First, the
authenticator never chooses a status: `authInterceptor` (`config/confighttp/server.go`)
hands every `Authenticate` error to the receiver's error handler
(`confighttp.WithErrorHandler`) as **401** with the error text, or writes exactly that
itself when no handler is installed. The status is therefore the receiver's, and this
receiver keeps **401** for every refusal of a token — 403 is deliberately never used —
and overrides it for one error only, not-ready → **503** (§3.3). Second,
`max_request_body_size` bounds the **decompressed** body.

## 3. Architecture

```
any client ──► [ consumer's proxy, or TLS here ] ──► otlp-collector-oidc ──► any OTLP endpoint
 bearer JWT                                          ├ oidcclientauth   the authenticator extension
                                                     ├ otlpsingleport   ONE TLS port: gRPC + HTTP /v1/{traces,logs,metrics}
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
The 503 is the receiver's doing, not the extension's: `Authenticate` returns a
distinguished not-ready error, the interceptor passes it to the receiver's error
handler as it does every other authenticator error, and that handler alone answers
503 with a `Retry-After` for this one error and writes every other status it is given
unchanged (§4.1). Over gRPC the handler answers with the gRPC status itself —
`Unavailable` and `Unauthenticated`, with the same text (§4.1, §10).

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
  handlers. Consumer errors map exactly as the stock receiver's do: an error carrying
  a gRPC status keeps its code — an upstream's permanent `InvalidArgument` reaches the
  client as `400` / `InvalidArgument`, a retryable `Unavailable` as `503` /
  `Unavailable` with the upstream's `RetryInfo` passed through as `Retry-After` — a
  permanent error with no status is the collector's own fault (`500` / `Internal`),
  and anything else is `503` / `Unavailable`. The shipped pipeline batches and queues,
  so in it the client sees only the receiver's own answers; an upstream's answer
  reaches the client only in a pipeline with neither.
- **One cap, both protocols.** `max_request_body_size` bounds the decompressed body
  (`413` over HTTP — the stock receiver answers `400`). Over gRPC, confighttp's cap
  also counts the 5-byte frame header of the raw stream, so the gRPC server's
  `MaxRecvMsgSize` is the cap less 5: a gRPC message may be 5 bytes smaller than an
  HTTP body, and every oversized one is `ResourceExhausted`.
- **One listener, shared.** Every pipeline naming the receiver's configuration gets
  the same instance; a signal with no pipeline has no route, so it answers `404` over
  HTTP and `Unimplemented` over gRPC.
- **The error handler is where any status other than 401 for an authenticator error
  comes from.** `confighttp.WithErrorHandler` is consulted by the auth interceptor
  and the decompressor alike; this receiver's handler answers `503` with `Retry-After`
  for the extension's not-ready error and writes every other status it is handed
  unchanged — `401` for any other authenticator error, the decompressor's own for a
  bad or oversized body. The handler sees only the error text, so the not-ready error
  is a fixed string the extension and the receiver share as a constant
  (`oidcclientauth.ErrNotReady`).
- **Authenticator answers speak the client's protocol.** A gRPC client handed a
  plain-text HTTP `401` or `503` sees only the status — grpc-go reports
  `Unauthenticated` or `Unavailable` with a transport message and drops the body —
  so for a gRPC request the handler answers an authenticator error as a gRPC
  trailers-only response instead: `grpc-status` `Unauthenticated` with the refusal
  text, or `Unavailable` with `not ready` and a `RetryInfo`. Over HTTP a `401`
  carries `WWW-Authenticate: Bearer`, and a `503` `Retry-After` plus the same
  `RetryInfo` in its OTLP status body. The decompressor's answers are unchanged.
  Authenticator refusals are the authenticator's to count, so they never reach
  `otlpsingleport_requests_refused`.
- **Own telemetry:** the standard `receiver_accepted_*` / `receiver_refused_*` via
  `receiverhelper`, so dashboards built for stock receivers read this one unchanged,
  and `otlpsingleport_requests_refused{transport, reason}` for every request refused
  before the pipeline — which the stock receiver does not count. `reason` is a closed
  set: `method`, `media_type`, `body_too_large`, `decode`, `unknown_path`,
  `decompress`. Over gRPC, an RPC counts only if it failed before its handler ran; a
  failure the pipeline returns is `receiver_refused_*`.

**Why the receiver is ours.** The stock OTLP receiver builds a `*grpc.Server` and an
`*http.Server` and calls `net.Listen` twice (`otlpreceiver/otlp.go:117,172`), so it
cannot serve both on one port. The protocols can: ALPN negotiates `h2` / `http/1.1`
on one TLS socket, and `grpc.Server.ServeHTTP` runs gRPC inside an ordinary HTTP/2
server. This receiver is that composition. If upstream ever grows a shared listener,
the stock receiver replaces it with no configuration change.

### 4.2 `oidcclientauth` — the authenticator extension

Implements `extensionauth.Server`.

- **Token source:** `Authorization: Bearer …`, and nothing else. There is no cookie
  form, so the receiver is never a target for an ambient credential: no CSRF position
  to hold, no `SameSite` requirement on the app, and the 415 on non-OTLP content types
  is a plain dispatch rule rather than a defence. A browser client therefore holds an
  access token it can put in a header, and flushes on unload with
  `fetch(…, { keepalive: true })`, which carries headers where `navigator.sendBeacon`
  cannot.
- **Validation:** discovery with `github.com/coreos/go-oidc/v3` at startup, with
  background retry on failure (the not-ready error meanwhile, which the receiver
  answers as 503 — §3.3 — never a crash) until discovery and the first JWKS load have
  both succeeded; a JWKS cache refreshed once on an unknown `kid` (at most once per
  10 s, since the lookup precedes signature verification and any client can present
  a made-up `kid`) and re-read every `OIDC_JWKS_REFRESH`, so a key rotated in needs
  no restart and a key removed stops being trusted within the interval; `aud` must contain `audience`; RS256/384/512 and ES256/384/512;
  required `exp` / `iss` / `aud`. Then `scope` (or an array `scp`) must contain
  `required_scope`. go-oidc's `IDTokenVerifier` is for ID tokens and its
  `RemoteKeySet` neither tells an unknown `kid` from a bad signature nor reports when
  its keys have loaded, so the key set and the access-token rules are the
  extension's own, on `go-jose` (which go-oidc uses).
- **Required claims** (`required_claims`, default `sub`, `preferred_username`) must be
  present and non-empty; otherwise the 401 body is `missing claim: <name>`.
- **Auth context:** every claim in `claim_attributes` (default `sub→user.id`,
  `preferred_username→user.name`, `email→user.email`, `name→user.full_name`; optional
  ones only when present) plus every key of `resource_attributes`, a static map the
  deployer sets so the deployment, not the client, states them.
- **Own telemetry:** `oidcclientauth_rejections{reason}` (on `:8888` as
  `otelcol_oidcclientauth_rejections`, named like the receiver's counter) with a
  bounded reason set (`no_token`, `invalid_token`, `missing_scope`, `missing_claim`,
  `not_ready`); one rate-limited warning per reason, carrying the count it
  suppressed and never the token.

**`resource_attributes` is optional; `deployment.environment.name` and the estate's
path marker (`telemetry_source=client`) are its usual content.**
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
  `transform` lifts the static resource attributes onto `resource.attributes` and
  clamps far-future timestamps; `filter` drops
  `service.name` outside `ALLOWED_SERVICE_NAMES` and far-past spans. The collector
  stamps no fixed marker of its own: the client-origin attribute is one of the
  deployer's `resource_attributes` keys. No attribute count or length caps: the
  decompressed body cap and the edge rate limit bound cost, and span/log attributes
  are not labels, so they cannot cost cardinality. **What `filter` drops is not
  reported to the client.** The receiver has answered `200` by the time a later
  processor drops an item, so OTLP's partial-success response — which the
  `observability` skill's client reference asks for — cannot be produced here. This
  is the recorded deviation: the drops are visible in the filter processor's own
  metrics on `:8888`, and the allowed set is published to client authors up front.
- **Metrics — a separate, stricter chain.** No identity action at all, so `user.*` and
  `session.id` cannot reach a datapoint by construction. `keep_keys` allowlists
  datapoint attribute keys (`ALLOWED_METRIC_ATTRIBUTE_KEYS`); `filter` allowlists
  metric names (`ALLOWED_METRIC_NAMES`); `deltatocumulative` has a hard `max_streams`
  so a client inventing series exhausts a counter, not the container; the resource is
  reduced to `service.name` / `service.version` and the keys of `resource_attributes`.
  Rejections are counted, so a client build that ships an unregistered metric is
  visible.
- `memory_limiter` first in every pipeline; identity is materialised from context
  **before** `batch`, so no `metadata_keys` on the batcher; the OTLP gRPC exporter
  (`otlp_grpc`; `otlp` is its deprecated alias from collector v0.161) with
  `sending_queue` and `retry_on_failure`.
- `health_check` on `:13133`; own metrics on `:8888`.

## 6. Configuration

### 6.1 Configuration is rendered, not substituted

Stock `${env:…}` substitution yields strings only: it cannot turn `k=v,k=v` into a
`headers:` map, a comma list into `keep_keys([...])`, or a protocol choice into a
different exporter component. So the binary has a render step: `otlp-collector-oidc run`
reads the environment, renders the embedded `collector.yaml.tmpl`, validates the
result with the collector's own config loader, and starts the collector on it.
Setting `COLLECTOR_CONFIG` to a mounted file skips rendering entirely; the rendered
file and a mounted one reach the collector through the same file provider, so every
behaviour holds for both. The rendered YAML is written to
`/tmp/otlp-collector-oidc.yaml` (the temporary directory) and logged at debug, so
"what config am I actually running" is one command, and the startup line records the
source and the value every variable took.

Every value is rendered as a quoted scalar with `$` doubled, so no variable can inject
YAML or a `${…}` reference: a value reaches its component as the literal string the
deployer set. An empty variable counts as unset. Every missing or malformed variable
is reported at once, by name and value, before the collector is built; a value that
parses but that a component refuses (a body cap below 6, a negative skew) is the
component's own validation error.

### 6.2 Reference

All values are environment variables. **Required** means the process refuses to start
without it, naming the variable.

**Identity provider**

| Variable | Default | Meaning |
| --- | --- | --- |
| `OIDC_ISSUER_URL` | **required** | Exact `iss` value; discovery at `<issuer>/.well-known/openid-configuration` |
| `OIDC_AUDIENCE` | **required** | Value `aud` must contain — normally the provider's client id |
| `OIDC_DISCOVERY_RETRY` | `30s` | Background retry interval while discovery fails (requests get 503 meanwhile) |
| `OIDC_JWKS_REFRESH` | `10m` | JWKS re-read interval once loaded: the longest a removed key stays trusted. A failed re-read keeps the cache |
| `REQUIRED_SCOPE` | `telemetry:write` | Must appear in `scope` (or `scp`) |
| `REQUIRED_CLAIMS` | `sub,preferred_username` | Each must be present and non-empty |
| `CLAIM_ATTRIBUTES` | `sub=user.id,preferred_username=user.name,email=user.email,name=user.full_name` | Claim → span/log attribute; absent optional claims are skipped |
| `CLIENT_RESOURCE_ATTRIBUTES` | *(empty = client's values kept)* | `key=value,…` stamped onto every **client** resource, overwriting the client's; `deployment.environment.name` and the estate's path marker (`telemetry_source=client`) recommended. Not `OTEL_RESOURCE_ATTRIBUTES`, which describes the collector itself |
| `CLOCK_SKEW` | `60s` | Tolerance on `exp` / `nbf` / `iat` |
| `REJECTION_LOG_INTERVAL` | `60s` | At most one warning per reason per interval |

**Listener and TLS**

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `0.0.0.0:4318` | The one listener: TLS; OTLP/gRPC and OTLP/HTTP on the same port, told apart per request (h2 + `application/grpc` → gRPC; `/v1/…` → HTTP). Same authenticator on both |
| `TLS_CERT_FILE` | `/etc/otlp-collector-oidc/tls/cert.pem` | Embedded self-signed pair unless overridden |
| `TLS_KEY_FILE` | `/etc/otlp-collector-oidc/tls/key.pem` | Set together with `TLS_CERT_FILE` |
| `TLS_RELOAD_INTERVAL` | `1m` | The pair is re-read at the first handshake after this interval, so a rotated certificate needs no restart |
| `MAX_REQUEST_BODY_BYTES` | `4194304` | Cap on the **decompressed** body → 413 |
| `CORS_ALLOWED_ORIGINS` | *(empty = CORS off)* | For browsers on a different origin; comma-separated. Allows `Authorization`, `Content-Type` and `Content-Encoding` |

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
| `LOG_LEVEL` | `info` | Applies to both outputs; at `debug` the rendered configuration is logged |
| `LOG_FORMAT` | `json` | Stdout encoding, `json` or `console` |
| `LOG_OUTPUT` | `both` | `both`, `otlp` or `stdout`. Own logs go to the logs upstream as OTLP unless `stdout`; stdout keeps a copy unless `otlp`. On a platform that also ships container stdout to the same store, pick one |
| `OTEL_SERVICE_NAME` | `otlp-collector-oidc` | This process's own `service.name` on its logs and metrics |
| `OTEL_RESOURCE_ATTRIBUTES` | *(empty)* | This process's own resource attributes, the SDK convention. Not what is stamped on client data — that is `CLIENT_RESOURCE_ATTRIBUTES` |
| `COLLECTOR_CONFIG` | *(empty = render the shipped pipeline)* | Path of a mounted pipeline that replaces the shipped one; nothing is rendered, and the custom components are still available to it |

**Trust roots are not a knob.** A provider or upstream signed by a private CA is
handled the standard Go way: mount the CA and set `SSL_CERT_FILE` or `SSL_CERT_DIR`.
`OTEL_EXPORTER_OTLP_CERTIFICATE` exists only because it is a zero-code passthrough of
the exporter's `tls.ca_file` and scoping trust to upstream alone is a real need.

The configuration reference, `docs/configuration.md`, is generated from one source —
the tags on the variable structs in `internal/render`, which hold each variable's
name, default, required-ness and description once — and CI fails if the template
renders a field that is not a variable, if a variable is not rendered, or if the
reference is stale.

## 7. The token profile — the contract with any OIDC provider

`docs/token-profile.md` states exactly what is accepted, so a third party can
configure their provider without reading Go. It is normative; the extension's tests
are its conformance suite; every rejection's 401 body names the failed rule, so a
misconfigured provider diagnoses itself with one `curl`.

- **What:** a JWS compact-serialised **access token** (RFC 9068 shape). An ID token is
  not accepted (it carries no `scope`); an opaque access token is not accepted.
- **Delivery:** `Authorization: Bearer <jwt>`. Nothing else is read: a token in a
  cookie or a query parameter is `no token`.
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
- **Failure catalogue** (the body text is the contract; `docs/token-profile.md` lists
  every rule's text). Every refusal of a token is **401**: `no token` · `invalid token: <reason>` · `missing scope: telemetry:write` ·
  `missing claim: preferred_username`. One answer is not a refusal and is **503** with
  `Retry-After`: `not ready`, while the JWKS has not yet loaded (§3.3) — the only
  status a client should back off and retry rather than fix.

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

- **Extension unit**, only for what cannot be observed from outside: optional claims
  absent → no attribute; `claim_attributes` and `resource_attributes` land in the
  auth context (an empty map means no resource action); the warning window's edge.
  Everything else — the bearer path, a token presented any other way (cookie, query
  parameter) → `no token`, every rule of the token profile, counter labels bounded,
  one warning per reason — is the integration table.
- **Receiver** (integration, like everything observable from outside):
  dispatch (h2 + `application/grpc` → gRPC; `/v1/*` + protobuf/JSON
  → HTTP; other paths 404, methods 405, content types 415); every signal in every
  encoding, gzip and plain; permanent vs retryable upstream errors → `400` / `503`
  and `InvalidArgument` / `Unavailable`, observed through a pipeline with no batch or
  queue; the error handler: the not-ready error →
  `503` with `Retry-After`, every other authenticator error → `401` with its text, a
  decompressor status passed through unchanged; the auth context is visible inside a
  gRPC handler; `receiver_accepted_*` / `refused_*` counted.
- **Renderer:** golden files per environment shape, each `OTEL_EXPORTER_OTLP_*`
  translation, every golden file loaded by the collector's own validation.
- **Integration** (the built binary as a subprocess, an httptest OIDC issuer,
  in-process gRPC sinks, **HTTPS** with a test certificate, every core scenario over
  both HTTP and gRPC on the same port, and a check that only one port is open):
  OTLP/JSON and gzipped protobuf with a bearer arrive carrying `user.id`,
  `user.name`, the static resource attributes; forged identity and resource values
  overwritten; `service.name`
  outside `ALLOWED_SERVICE_NAMES` dropped, `.*` admits any; far-future start clamped;
  no/invalid token, missing scope, missing claim → 401 with the named body and
  nothing at the sink; a 5 MiB gzip bomb → 413; a `text/plain` body with a valid
  bearer → 415; sink down → client status unchanged and
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

**Resolved** (collector v0.161.0, grpc-go v1.83.2, Go 1.27; proven by the integration
suite, so a version bump re-proves them):

- `grpc.Server.ServeHTTP` inside the `confighttp` server serves unary `Export` for
  every signal, plain and `grpc-encoding: gzip`, byte-for-byte; the `cmux` fallback is
  not needed. The server's `otelhttp` wrapper keeps the `http.Flusher` gRPC requires.
- `confighttp`'s decompressor keys on `Content-Encoding` alone, which gRPC never sets,
  so gRPC frames pass through it untouched. Its body-size interceptor does wrap the
  gRPC request stream, bounding the raw bytes, frame header included; the gRPC
  server's `MaxRecvMsgSize` is therefore the cap less 5, so a message one byte over
  is `ResourceExhausted` compressed or not, rather than a stream read error.
- grpc-go calls no stats handler for an RPC to an unregistered service, so the
  receiver's unknown-service handler answers `Unimplemented` and counts it itself.
- HTTP/1.1 with `Content-Type: application/grpc` is not gRPC: it is dispatched as
  OTLP/HTTP and answers `404` on a gRPC path.
- The interceptor's plain-text `401` and `503` would reach a gRPC client as
  `Unauthenticated` and `Unavailable`, but without the refusal text, which grpc-go
  drops for a non-gRPC answer; so the error handler answers gRPC requests with a
  trailers-only gRPC status carrying it (§4.1). The integration suite asserts the
  exact text over both protocols.
- The receiver's `tls:` block reloads a mounted pair: `configtls`'s `reload_interval`
  re-reads it at the first handshake after the interval, rendered from
  `TLS_RELOAD_INTERVAL`. The non-root runtime user reads a mounted pair that is
  world-readable (the image smoke mounts one and verifies it with `curl --cacert`).
- `confighttp`'s CORS allows `Content-Type` implicitly only when `allowed_headers`
  is empty (rs/cors v1.11), despite its documentation; the renderer lists it.
- `client.FromContext(ctx).Auth` inside the handlers — gRPC through `ServeHTTP` and
  HTTP alike — carries the auth data the interceptor set: grpc-go's handler transport
  derives the stream context from the request's (a receiver unit test until the
  pipeline reads the auth context).

**Open:**

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
- authentik: a provider without `signing_key` issues a non-RS or opaque token; `scope`
  on the access token is space-delimited; `hashed_user_id` `sub` is stable across
  provider edits other than `sub_mode`.
