package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The upstream is configured by the standard OTEL_EXPORTER_OTLP_* variables,
// per signal. Each shape sends traces and logs over both client protocols
// and observes where they arrive and what the exporter sent with them.

// upstreamSecret is a header value that must reach the sink and appear in
// no output, the debug dump of the rendered configuration included.
const upstreamSecret = "Bearer upstream-secret-7f3a" // #nosec G101 -- made up, to prove it is never printed

// deliver sends one traces and one logs export per client protocol, each
// marked with name, and waits until each reaches its sink.
func deliver(t *testing.T, env shipped, name string, tracesSink, logsSink *harness.Sink) {
	t.Helper()
	for _, p := range protocols {
		marker := name + "/" + p.String()
		got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces(marker, 1))
		require.Equal(t, codes.OK, got.code, got.message)
		got = env.clients.export(t.Context(), t, p, harness.SignalLogs, harness.Logs(marker, 1))
		require.Equal(t, codes.OK, got.code, got.message)
		require.Eventually(t, func() bool {
			_, ok := harness.FindTraces(tracesSink.Received().Traces, marker)
			return ok
		}, eventually, 10*time.Millisecond, "traces %s never reached their sink", marker)
		require.Eventually(t, func() bool {
			_, ok := harness.FindLogs(logsSink.Received().Logs, marker)
			return ok
		}, eventually, 10*time.Millisecond, "logs %s never reached their sink", marker)
	}
}

// TestUpstreamPerSignal sends traces to a gRPC upstream and, through the
// LOGS_* overrides, logs to an HTTP one at a path of its own, as traces to
// Tempo and logs to Loki. The per-signal headers replace the base ones.
func TestUpstreamPerSignal(t *testing.T) {
	t.Parallel()
	traces := harness.NewSink(t)
	logs := harness.NewSinkWith(t, harness.SinkOptions{Transport: harness.TransportHTTP})
	env := startShippedWith(t, traces, map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS":       "x-tenant=z,authorization=" + strings.ReplaceAll(upstreamSecret, " ", "%20") + ",x-tenant=a",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": logs.Endpoint() + "/otlp/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":  "X-Scope-OrgID=tenant-b",
		"LOG_LEVEL":                        "debug",
	}, nil)
	deliver(t, env, "per-signal", traces, logs)

	assert.Empty(t, traces.Received().Logs, "logs reached the traces upstream")
	assert.Empty(t, logs.Received().Traces, "traces reached the logs upstream")
	for _, req := range traces.Received().Requests {
		assert.Equal(t, []string{upstreamSecret}, req.Header["authorization"], "base headers on the gRPC upstream")
		assert.Equal(t, []string{"a"}, req.Header["x-tenant"], "a later pair of the same name wins")
	}
	for _, req := range logs.Received().Requests {
		assert.Equal(t, "/otlp/v1/logs", req.Path, "a per-signal HTTP endpoint is used verbatim")
		assert.Equal(t, []string{"tenant-b"}, req.Header["x-scope-orgid"])
		assert.NotContains(t, req.Header, "authorization", "per-signal headers replace the base ones")
		assert.Equal(t, []string{"gzip"}, req.Header["content-encoding"], "gzip by default")
	}
	out := env.collector.Output()
	assert.Contains(t, out, "Rendered configuration", "the debug dump is written")
	assert.NotContains(t, out, "upstream-secret", "a header value was logged")
	assert.Contains(t, out, "headers=authorization,x-tenant", "the startup line names the headers")
}

// TestUpstreamHTTP sends every signal to one HTTP upstream over TLS, trusted
// through OTEL_EXPORTER_OTLP_CERTIFICATE alone: the base endpoint's path is
// kept and /v1/<signal> appended, and nothing is compressed. A retry limit
// under the collector's first backoff still starts.
func TestUpstreamHTTP(t *testing.T) {
	t.Parallel()
	cert := harness.NewCertificate(t)
	sink := harness.NewSinkWith(t, harness.SinkOptions{Transport: harness.TransportHTTP, Cert: &cert})
	require.True(t, strings.HasPrefix(sink.Endpoint(), "https://"))
	env := startShippedWith(t, sink, map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL":    "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT":    sink.Endpoint() + "/otlp",
		"OTEL_EXPORTER_OTLP_CERTIFICATE": cert.CertFile,
		"OTEL_EXPORTER_OTLP_COMPRESSION": "none",
		// Below the collector's first backoff, which is shortened to it.
		"UPSTREAM_RETRY_MAX_ELAPSED": "2s",
	}, nil)
	deliver(t, env, "http", sink, sink)

	paths := map[string]bool{}
	for _, req := range sink.Received().Requests {
		paths[req.Path] = true
		assert.NotContains(t, req.Header, "content-encoding", "OTEL_EXPORTER_OTLP_COMPRESSION=none")
	}
	assert.Equal(t, map[string]bool{"/otlp/v1/traces": true, "/otlp/v1/logs": true}, paths)
}

// TestUpstreamTLSVariables covers the remaining TLS knobs in one process:
// traces go over mTLS to an upstream that requires a client certificate,
// and logs, through LOGS_INSECURE, in plaintext to an https:// endpoint. The
// base endpoint is unset: with the traces and logs endpoints set, it is not
// required, since metrics has no pipeline to need one.
func TestUpstreamTLSVariables(t *testing.T) {
	t.Parallel()
	server, client := harness.NewCertificate(t), harness.NewCertificate(t)
	traces := harness.NewSinkWith(t, harness.SinkOptions{Cert: &server, ClientCAs: client.Pool})
	logs := harness.NewSink(t)
	env := startShippedWith(t, traces, map[string]string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE":        server.CertFile,
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": client.CertFile,
		"OTEL_EXPORTER_OTLP_CLIENT_KEY":         client.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT":           "",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":    traces.Endpoint(),
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":      strings.Replace(logs.Endpoint(), "http://", "https://", 1),
		"OTEL_EXPORTER_OTLP_LOGS_INSECURE":      "true",
	}, nil)
	deliver(t, env, "tls-variables", traces, logs)
}

// TestUpstreamInheritedInsecure sets OTEL_EXPORTER_OTLP_INSECURE=true for
// the base: traces over gRPC go plaintext to an https:// endpoint, while logs
// over http/protobuf, to which it does not apply, still speak TLS.
func TestUpstreamInheritedInsecure(t *testing.T) {
	t.Parallel()
	traces := harness.NewSink(t)
	cert := harness.NewCertificate(t)
	logs := harness.NewSinkWith(t, harness.SinkOptions{Transport: harness.TransportHTTP, Cert: &cert})
	env := startShippedWith(t, traces, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         strings.Replace(traces.Endpoint(), "http://", "https://", 1),
		"OTEL_EXPORTER_OTLP_INSECURE":         "true",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL":    "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":    logs.Endpoint() + "/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE": cert.CertFile,
	}, nil)
	deliver(t, env, "inherited-insecure", traces, logs)
}

// TestUpstreamSkipVerify trusts nothing about the upstream's certificate and
// still delivers, because UPSTREAM_TLS_INSECURE_SKIP_VERIFY says so.
func TestUpstreamSkipVerify(t *testing.T) {
	t.Parallel()
	sink := harness.NewTLSSink(t, harness.NewCertificate(t))
	env := startShippedWith(t, sink, map[string]string{"UPSTREAM_TLS_INSECURE_SKIP_VERIFY": "true"}, nil)
	deliver(t, env, "skip-verify", sink, sink)
}

// TestUpstreamQueueFull keeps the upstream down with a one-request queue:
// every client still gets OK and the process stays healthy, while the
// exporter refuses what its queue cannot hold and, once
// UPSTREAM_RETRY_MAX_ELAPSED is spent, drops what it was retrying. Both are
// counted on :8888, per exporter.
func TestUpstreamQueueFull(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	sink.SetBehaviour(t, harness.BehaviourDown)
	// Longer than the first retry's longest backoff (5s ± 50%), so an export
	// is retried at least once and holds its consumer meanwhile.
	env := startShippedWith(t, sink, map[string]string{
		"UPSTREAM_QUEUE_SIZE":        "1",
		"UPSTREAM_RETRY_MAX_ELAPSED": "10s",
	}, nil)

	exporter := map[string]string{"exporter": "otlp_grpc"}
	// Exports until the queue overflows; each client must still see OK.
	deadline := time.Now().Add(eventually)
	for n := 0; scrape(t, env.collector).Sum("otelcol_exporter_enqueue_failed_spans", exporter) == 0; {
		require.True(t, time.Now().Before(deadline), "the queue never overflowed after %d exports", n)
		for _, p := range protocols {
			n++
			got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("queue-full", 1))
			require.Equal(t, codes.OK, got.code, "a client is not told the queue is full: %s", got.message)
		}
	}
	assert.True(t, env.collector.Healthy(t.Context()), "health must not follow the upstream")

	samples := scrape(t, env.collector)
	// One queue per pipeline the exporter serves; traces' is the one filled.
	assert.InDelta(t, 1.0, samples.Sum("otelcol_exporter_queue_capacity", map[string]string{"exporter": "otlp_grpc", "data_type": "traces"}), 0, "UPSTREAM_QUEUE_SIZE")
	_, ok := samples.Find("otelcol_exporter_queue_size")
	assert.True(t, ok, "the queue size is exposed")
	require.Eventually(t, func() bool {
		return scrape(t, env.collector).Sum("otelcol_exporter_send_failed_spans", exporter) > 0
	}, 30*time.Second, 100*time.Millisecond, "a retried export was never given up")
}
