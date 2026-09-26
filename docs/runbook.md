# Runbook

What each alert in [`alerts/otlp-collector-oidc.yaml`](../alerts/otlp-collector-oidc.yaml)
means and what to do. The [dashboard](../dashboards/otlp-collector-oidc.json) has a
panel for each. Health (`:13133`) never follows the upstream or the provider, so
none of these is fixed by restarting the container unless the entry says so.

## OTLPCollectorOIDCAbsent

**Means:** an environment that reported `target_info` within the last day has not
for ten minutes. Either the process is gone, or its `:8888` is no longer scraped. It
keeps firing for up to a day of outage.

**Check first:** is the container running and healthy (`docker ps`, the image
`HEALTHCHECK`)? If it is, the scrape broke: the target list of whatever scrapes
`:8888`, and whether `SELF_METRICS_ADDR` still points where it scrapes.

**False positive:** an environment deliberately retired. It clears a day later;
silence it until then.

## OTLPCollectorOIDCNotReady

**Means:** clients are answered `503 not ready` (gRPC `Unavailable`): the OIDC
provider's discovery document or keys have not loaded. Every client export is
refused, and clients back off and retry.

**Check first:** the collector's own logs for `OIDC discovery failed`, which names
the error. From the container, is `OIDC_ISSUER_URL/.well-known/openid-configuration`
reachable, and does its `issuer` equal `OIDC_ISSUER_URL` exactly? The collector
retries every `OIDC_DISCOVERY_RETRY` and recovers by itself once the provider
answers; restarting does not help.

**False positive:** a few minutes after a deploy while the provider itself starts.

## OTLPCollectorOIDCTokensRefused

**Means:** tokens are refused for a reason one misconfigured client would not
explain, sustained. The `reason` label says which rule:

- `missing_claim` — the provider stopped emitting a claim in `REQUIRED_CLAIMS`
  (a scope mapping removed or renamed). The warning in the collector's logs names
  the claim.
- `missing_scope` — tokens no longer carry `REQUIRED_SCOPE`: the scope was removed
  from the client, or its mapping changed.
- `invalid_token` — signature, issuer, audience or expiry. After a provider key
  rotation the collector refreshes on an unknown `kid` by itself; a sustained rate
  usually means an `OIDC_AUDIENCE` or issuer mismatch after a provider edit, or
  clients with badly skewed clocks (`CLOCK_SKEW`).

**Check first:** the rate-limited `Refused a request` warnings in the collector's
logs, which carry `reason` and the failing rule, never the token. Mint a token as a
client would and compare it with [the token profile](token-profile.md).

**False positive:** a scanner or a stale client sending garbage. The threshold
(0.1/s for 15 minutes) is meant to sit above that; raise it if a known client is
the cause.

## OTLPCollectorOIDCExportDropped

**Means:** telemetry is being lost on the way upstream, at the `exporter` label:
exports given up after `UPSTREAM_RETRY_MAX_ELAPSED`, or refused because the
sending queue (`UPSTREAM_QUEUE_SIZE`) was full. Clients were told `200`; the data
is gone.

**Check first:** is the upstream up and reachable from the container
(`OTEL_EXPORTER_OTLP_*`, the per-signal overrides)? The collector's logs say
`Exporting failed` with the upstream's error. A TLS error means the CA
(`OTEL_EXPORTER_OTLP_CERTIFICATE`, `SSL_CERT_FILE`) or the client certificate;
`Unauthenticated` from the upstream means `OTEL_EXPORTER_OTLP_HEADERS`. The
**Queue fill** panel near 1 means the upstream is slower than the traffic, or
down. Do not restart the collector: it drops what is queued. Fix the upstream and
the queue drains by itself.

**False positive:** a short upstream restart longer than the retry window.
