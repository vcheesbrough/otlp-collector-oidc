<!-- Generated from internal/render by `make docs`; edit the struct tags there, not this file. -->

# Configuration

Every setting is an environment variable. `otlp-collector-oidc run` (the image's
command) reads them, renders the shipped pipeline and starts the collector on it;
the rendered file is written to `otlp-collector-oidc.yaml` in the temporary directory
(`/tmp` in the image) and logged at `LOG_LEVEL=debug`.

A variable set to the empty string counts as unset. A **required** variable that is
unset, or any value that does not parse, stops the process before it listens, and the
error names the variable and the value.

A provider or upstream whose certificate is signed by a private CA is trusted the
standard Go way: mount the CA and set `SSL_CERT_FILE` or `SSL_CERT_DIR`.

## Identity provider

| Variable | Default | Meaning |
| --- | --- | --- |
| `OIDC_ISSUER_URL` | **required** | Exact `iss` value; discovery reads `<issuer>/.well-known/openid-configuration` |
| `OIDC_AUDIENCE` | **required** | Value `aud` must contain, normally the provider client id |
| `OIDC_DISCOVERY_RETRY` | `30s` | Retry interval while discovery fails; requests get `503` meanwhile |
| `OIDC_JWKS_REFRESH` | `10m` | JWKS re-read interval: the longest a key the provider removes is still trusted |
| `REQUIRED_SCOPE` | `telemetry:write` | Must appear in `scope` (or `scp`) |
| `REQUIRED_CLAIMS` | `sub,preferred_username` | Comma-separated claims that must each be present and non-empty |
| `CLOCK_SKEW` | `60s` | Tolerance on `exp`, `nbf` and `iat` |
| `REJECTION_LOG_INTERVAL` | `60s` | At most one warning per refusal reason per interval |

## Upstream

| Variable | Default | Meaning |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | **required** | Upstream URL. `http://` is plaintext, `https://` is TLS verified against the system roots (or `SSL_CERT_FILE`). With `grpc` it is `scheme://host:port` only; with `http/protobuf` the base endpoint gets `/v1/<signal>` appended and a per-signal one is used verbatim. Not needed when all three per-signal endpoints are set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | `grpc` or `http/protobuf`; selects the exporter component |
| `OTEL_EXPORTER_OTLP_HEADERS` | *(empty)* | `key=value,key2=value2` sent on every export, values percent-decoded: a tenant id, a hosted backend's token. Never logged |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | `10000` | Export timeout in milliseconds |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | `gzip` | `gzip` or `none` |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | *(empty)* | CA file that alone verifies the upstream's certificate, in place of the system roots |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` | *(empty)* | Client certificate (PEM) for mTLS to the upstream; set together with `OTEL_EXPORTER_OTLP_CLIENT_KEY` |
| `OTEL_EXPORTER_OTLP_CLIENT_KEY` | *(empty)* | Its private key (PEM) |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | `true` for plaintext gRPC whatever the scheme; with `http/protobuf` use an `http://` URL instead |
| `UPSTREAM_TLS_INSECURE_SKIP_VERIFY` | `false` | `true` accepts an upstream certificate that does not verify. No SDK equivalent; applies to every TLS upstream |
| `UPSTREAM_QUEUE_SIZE` | `1000` | Export requests held per exporter and pipeline while the upstream is unreachable; beyond it they are dropped and counted. No SDK equivalent |
| `UPSTREAM_RETRY_MAX_ELAPSED` | `60s` | How long a failing export is retried before it is dropped and counted; `0s` retries forever. No SDK equivalent |

Every `OTEL_EXPORTER_OTLP_*` variable above also has `OTEL_EXPORTER_OTLP_TRACES_*`, `OTEL_EXPORTER_OTLP_LOGS_*` and `OTEL_EXPORTER_OTLP_METRICS_*` forms, which take precedence for that signal, as the OpenTelemetry SDK specification defines them. One exporter is rendered per distinct signal configuration, so with no per-signal variable set all signals share one. The `UPSTREAM_*` variables apply to every exporter.

## Listener and TLS

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `0.0.0.0:4318` | The one listener: TLS, OTLP/gRPC and OTLP/HTTP on the same port |
| `TLS_CERT_FILE` | `/etc/otlp-collector-oidc/tls/cert.pem` | Server certificate (PEM); the image's embedded self-signed one unless set |
| `TLS_KEY_FILE` | `/etc/otlp-collector-oidc/tls/key.pem` | Its private key (PEM); set together with `TLS_CERT_FILE` |
| `TLS_RELOAD_INTERVAL` | `1m` | How often the pair is re-read from disk, so a rotated certificate needs no restart |
| `MAX_REQUEST_BODY_BYTES` | `4194304` | Cap on the decompressed request, `413` beyond it; a gRPC message may be 5 bytes less (its frame header), `ResourceExhausted` beyond it |
| `CORS_ALLOWED_ORIGINS` | *(empty)* | Comma-separated origins allowed to send from a browser on another origin; empty turns CORS off |

## Resources and batching

| Variable | Default | Meaning |
| --- | --- | --- |
| `MEMORY_LIMIT_MIB` | `64` | `memory_limiter` hard limit |
| `MEMORY_SPIKE_LIMIT_MIB` | `16` | `memory_limiter` spike limit; less than `MEMORY_LIMIT_MIB` |
| `BATCH_TIMEOUT` | `5s` | Longest a record waits in the batcher |

## Health and own metrics

| Variable | Default | Meaning |
| --- | --- | --- |
| `HEALTH_ADDR` | `127.0.0.1:13133` | Liveness endpoint; never checks the upstream or the provider |
| `SELF_METRICS_ADDR` | `0.0.0.0:8888` | Prometheus endpoint for the collector's own metrics, at `/metrics` |

## Own logs

| Variable | Default | Meaning |
| --- | --- | --- |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; at `debug` the rendered configuration is logged |
| `LOG_FORMAT` | `json` | Stdout encoding, `json` or `console` |

## Replacing the pipeline

| Variable | Default | Meaning |
| --- | --- | --- |
| `COLLECTOR_CONFIG` | *(empty)* | Path of a collector configuration to run instead of the shipped pipeline. Nothing is rendered: every other variable is ignored unless that file reads it with `${env:...}`, except `LOG_LEVEL` and `LOG_FORMAT`, which still govern the run command's own lines. The custom components remain available to it |
