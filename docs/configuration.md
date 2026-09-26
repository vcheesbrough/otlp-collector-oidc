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
| `OTEL_EXPORTER_OTLP_ENDPOINT` | **required** | Upstream OTLP/gRPC endpoint as a URL: `http://host:4317` is plaintext, `https://host:4317` is TLS verified against the system roots (or `SSL_CERT_FILE`) |

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
| `COLLECTOR_CONFIG` | *(empty)* | Path of a collector configuration to run instead of the shipped pipeline; every other variable is then ignored unless that file reads it with `${env:...}`. The custom components remain available to it |
