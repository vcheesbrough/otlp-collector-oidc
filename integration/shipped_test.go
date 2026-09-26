package integration

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// eventually is how long a scenario waits for data to cross the pipeline.
const eventually = 20 * time.Second

// shipped is the image's environment shape: the shipped configuration, TLS
// with a test certificate, an OIDC issuer, and an upstream that answers OK.
// Its clients present a valid token.
type shipped struct {
	collector *harness.Collector
	sink      *harness.Sink
	issuer    *harness.Issuer
	clients   clients
}

func startShipped(t *testing.T, env map[string]string) shipped {
	t.Helper()
	return startShippedWith(t, harness.NewSink(t), env, nil)
}

// startShippedWith is startShipped with the upstream given, and, when claims
// is set, the clients' token carrying claims(issuer) instead of the issuer's
// defaults.
func startShippedWith(t *testing.T, sink *harness.Sink, env map[string]string, claims func(*harness.Issuer) harness.Claims) shipped {
	t.Helper()
	cert := harness.NewCertificate(t)
	iss := harness.NewIssuer(t)
	iss.Start(t)
	vars := oidcEnv(iss)
	maps.Copy(vars, map[string]string{
		"TLS_CERT_FILE":               cert.CertFile,
		"TLS_KEY_FILE":                cert.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT": sink.Endpoint(),
		// Short, so data reaches the sink quickly; the default is 5s.
		"BATCH_TIMEOUT": "50ms",
	})
	maps.Copy(vars, env)
	c := harness.Start(t, binary, harness.Options{Env: vars})
	tokenClaims := iss.Claims()
	if claims != nil {
		tokenClaims = claims(iss)
	}
	cl := newClients(t, c.ListenAddr, cert.Pool, iss.Sign(t, jose.RS256, tokenClaims))
	awaitReady(t, cl)
	return shipped{collector: c, sink: sink, issuer: iss, clients: cl}
}

// TestShipped runs every scenario of the shipped shape against one process.
// Scenarios that only read run in parallel; the ones that read counters or
// take the upstream down run after them, one at a time.
func TestShipped(t *testing.T) {
	env := startShipped(t, nil)

	t.Run("parallel", func(t *testing.T) {
		t.Run("dispatch", dispatchScenarios(env))
		t.Run("fidelity", fidelityScenarios(env))
		t.Run("bodies", bodyScenarios(env))
		t.Run("cors off", corsOff(env))
		t.Run("logs", defaultLogs(env))
	})
	t.Run("counters", acceptedCounters(env))
	t.Run("refusals", refusalCounters(env))
	t.Run("one listener", oneListener(env))
	t.Run("version", versionAgrees(env))
	t.Run("upstream down", upstreamDown(env))
}

// dispatchScenarios is the dispatch matrix: which requests reach which handler,
// and what everything else answers.
func dispatchScenarios(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		traces, err := harness.Traces("dispatch", 1).MarshalProto()
		require.NoError(t, err)

		httpCases := []struct {
			name        string
			http1       bool
			method      string
			path        string
			contentType string
			body        []byte
			wantStatus  int
			wantProto   string
		}{
			{name: "POST /v1/traces protobuf over HTTP/2", method: http.MethodPost, path: "/v1/traces", contentType: "application/x-protobuf", body: traces, wantStatus: http.StatusOK, wantProto: "HTTP/2.0"},
			{name: "POST /v1/traces protobuf over HTTP/1.1", http1: true, method: http.MethodPost, path: "/v1/traces", contentType: "application/x-protobuf", body: traces, wantStatus: http.StatusOK, wantProto: "HTTP/1.1"},
			{name: "GET /v1/traces", method: http.MethodGet, path: "/v1/traces", wantStatus: http.StatusMethodNotAllowed},
			{name: "PUT /v1/logs", method: http.MethodPut, path: "/v1/logs", contentType: "application/json", body: []byte("{}"), wantStatus: http.StatusMethodNotAllowed},
			{name: "text/plain", method: http.MethodPost, path: "/v1/traces", contentType: "text/plain", body: []byte("hello"), wantStatus: http.StatusUnsupportedMediaType},
			{name: "no content type", method: http.MethodPost, path: "/v1/logs", body: traces, wantStatus: http.StatusUnsupportedMediaType},
			{name: "unknown path", method: http.MethodPost, path: "/v1/profiles", contentType: "application/x-protobuf", body: traces, wantStatus: http.StatusNotFound},
			{name: "root", method: http.MethodGet, path: "/", wantStatus: http.StatusNotFound},
			{name: "metrics are unwired", method: http.MethodPost, path: "/v1/metrics", contentType: "application/x-protobuf", body: traces, wantStatus: http.StatusNotFound},
			{name: "gRPC content type over HTTP/1.1 is not gRPC", http1: true, method: http.MethodPost, path: "/opentelemetry.proto.collector.trace.v1.TraceService/Export", contentType: "application/grpc", body: traces, wantStatus: http.StatusNotFound},
		}
		for _, tc := range httpCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				client := env.clients.http
				if tc.http1 {
					client = env.clients.http1
				}
				header := http.Header{}
				if tc.contentType != "" {
					header.Set("Content-Type", tc.contentType)
				}
				resp, err := client.Do(t.Context(), tc.method, tc.path, header, tc.body)
				require.NoError(t, err)
				assert.Equal(t, tc.wantStatus, resp.Status, "%s %s (%s): %s", tc.method, tc.path, tc.contentType, resp.Body)
				if tc.wantProto != "" {
					assert.Equal(t, tc.wantProto, resp.Proto)
				}
				if tc.wantStatus == http.StatusMethodNotAllowed {
					assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"))
				}
			})
		}

		grpcCases := []struct {
			name     string
			message  harness.Message
			wantCode codes.Code
		}{
			{name: "gRPC traces reach the gRPC service", message: harness.Traces("dispatch-grpc", 1), wantCode: codes.OK},
			{name: "gRPC logs reach the gRPC service", message: harness.Logs("dispatch-grpc", 1), wantCode: codes.OK},
			{name: "gRPC metrics are unwired", message: harness.Metrics("dispatch-grpc", 1), wantCode: codes.Unimplemented},
		}
		for _, tc := range grpcCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := grpcOutcome(env.clients.grpc.Export(t.Context(), tc.message, harness.CompressionNone))
				assert.Equal(t, tc.wantCode, got.code, got.message)
			})
		}
	}
}

// fidelityScenarios sends every wired signal in every encoding, gzip and plain,
// over both protocols, and checks the sink receives exactly what was sent.
func fidelityScenarios(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		type route struct {
			protocol protocol
			encoding harness.Encoding
		}
		routes := []route{
			{protocol: protocolHTTP, encoding: harness.EncodingProtobuf},
			{protocol: protocolHTTP, encoding: harness.EncodingJSON},
			{protocol: protocolGRPC, encoding: harness.EncodingProtobuf},
		}
		for _, s := range []harness.Signal{harness.SignalTraces, harness.SignalLogs} {
			for _, r := range routes {
				for _, comp := range []harness.Compression{harness.CompressionNone, harness.CompressionGzip} {
					name := s.String() + "/" + r.protocol.String() + "/" + r.encoding.String() + "/" + comp.String()
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						marker := "fidelity/" + name
						sent, want := fidelityPayload(t, s, marker)
						send(t, env.clients, r.protocol, s, sent, r.encoding, comp)
						var got []byte
						require.Eventually(t, func() bool {
							var ok bool
							got, ok = receivedProto(t, env.sink.Received(), s, marker)
							return ok
						}, eventually, 10*time.Millisecond, "%s never reached the sink", marker)
						assert.True(t, bytes.Equal(want, got), "sink received a different payload for %s:\nwant %x\ngot  %x", marker, want, got)
					})
				}
			}
		}
	}
}

// fidelityPayload is the request to send for s and the protobuf bytes of the
// resource the sink must receive.
func fidelityPayload(t *testing.T, s harness.Signal, marker string) (harness.Message, []byte) {
	t.Helper()
	switch s {
	case harness.SignalTraces:
		req := harness.Traces(marker, 3)
		want, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(req.Traces())
		require.NoError(t, err)
		return req, want
	case harness.SignalLogs:
		req := harness.Logs(marker, 3)
		want, err := (&plog.ProtoMarshaler{}).MarshalLogs(req.Logs())
		require.NoError(t, err)
		return req, want
	case harness.SignalMetrics:
		require.FailNow(t, "metrics are not wired in the shipped shape")
	}
	return nil, nil
}

func receivedProto(t *testing.T, r harness.Received, s harness.Signal, marker string) ([]byte, bool) {
	t.Helper()
	switch s {
	case harness.SignalTraces:
		td, ok := harness.FindTraces(r.Traces, marker)
		if !ok {
			return nil, false
		}
		b, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
		require.NoError(t, err)
		return b, true
	case harness.SignalLogs:
		ld, ok := harness.FindLogs(r.Logs, marker)
		if !ok {
			return nil, false
		}
		b, err := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
		require.NoError(t, err)
		return b, true
	case harness.SignalMetrics:
		require.FailNow(t, "metrics are not wired in the shipped shape")
	}
	return nil, false
}

// send exports m and requires success.
func send(t *testing.T, c clients, p protocol, s harness.Signal, m harness.Message, e harness.Encoding, comp harness.Compression) {
	t.Helper()
	switch p {
	case protocolHTTP:
		resp, err := c.http.Export(t.Context(), s, m, e, comp)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.Status, "%s", resp.Body)
		require.Equal(t, e.ContentType(), resp.Header.Get("Content-Type"), "the response is in the request's encoding")
	case protocolGRPC:
		require.NoError(t, c.grpc.Export(t.Context(), m, comp))
	}
}

// bodyScenarios covers bodies the listener refuses before any handler decodes
// them: the decompressor's own answers, and the decompressed size cap on both
// protocols.
func bodyScenarios(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		const fiveMiB = 5 << 20
		bomb, err := harness.Gzip(make([]byte, fiveMiB))
		require.NoError(t, err)

		httpCases := []struct {
			name            string
			contentEncoding string
			body            []byte
			wantStatus      int
			wantCode        codes.Code
			wantMessage     string
		}{
			{name: "malformed gzip", contentEncoding: "gzip", body: []byte("this is not gzip"), wantStatus: http.StatusBadRequest, wantCode: codes.InvalidArgument, wantMessage: "gzip: invalid header"},
			{name: "unsupported content encoding", contentEncoding: "br", body: []byte("x"), wantStatus: http.StatusBadRequest, wantCode: codes.InvalidArgument, wantMessage: "unsupported Content-Encoding: br"},
			{name: "5 MiB gzip bomb", contentEncoding: "gzip", body: bomb, wantStatus: http.StatusRequestEntityTooLarge, wantCode: codes.ResourceExhausted, wantMessage: "request body too large"},
			{name: "5 MiB plain body", body: make([]byte, fiveMiB), wantStatus: http.StatusRequestEntityTooLarge, wantCode: codes.ResourceExhausted, wantMessage: "request body too large"},
			{name: "not protobuf", body: []byte{0xff, 0xff, 0xff}, wantStatus: http.StatusBadRequest, wantCode: codes.InvalidArgument},
		}
		for _, tc := range httpCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				header := http.Header{"Content-Type": {"application/x-protobuf"}}
				if tc.contentEncoding != "" {
					header.Set("Content-Encoding", tc.contentEncoding)
				}
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/traces", header, tc.body)
				require.NoError(t, err)
				got := httpOutcome(t, resp)
				assert.Equal(t, tc.wantStatus, got.httpStatus, got.message)
				assert.Equal(t, tc.wantCode, got.code, got.message)
				assert.Contains(t, got.message, tc.wantMessage)
			})
		}

		// Over gRPC the same cap bounds the decompressed message.
		big := harness.Traces("bodies-grpc-bomb", 1)
		big.Traces().ResourceSpans().At(0).Resource().Attributes().PutStr("padding", strings.Repeat("0", fiveMiB))
		for _, comp := range []harness.Compression{harness.CompressionGzip, harness.CompressionNone} {
			t.Run("5 MiB gRPC message "+comp.String(), func(t *testing.T) {
				t.Parallel()
				got := grpcOutcome(env.clients.grpc.Export(t.Context(), big, comp))
				assert.Equal(t, codes.ResourceExhausted, got.code, got.message)
			})
		}

		for _, p := range protocols {
			t.Run("empty request is accepted/"+p.String(), func(t *testing.T) {
				t.Parallel()
				got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("empty", 0))
				assert.Equal(t, codes.OK, got.code, got.message)
			})
		}
	}
}

// acceptedCounters checks receiver_accepted_* on :8888 move by exactly
// what was sent, per transport, and receiver_refused_* do not move.
func acceptedCounters(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		before := scrape(t, env.collector)

		sends := []struct {
			protocol protocol
			signal   harness.Signal
			message  harness.Message
		}{
			{protocol: protocolHTTP, signal: harness.SignalTraces, message: harness.Traces("counters", 3)},
			{protocol: protocolGRPC, signal: harness.SignalTraces, message: harness.Traces("counters", 2)},
			{protocol: protocolHTTP, signal: harness.SignalLogs, message: harness.Logs("counters", 4)},
			{protocol: protocolGRPC, signal: harness.SignalLogs, message: harness.Logs("counters", 1)},
		}
		for _, s := range sends {
			got := env.clients.export(t.Context(), t, s.protocol, s.signal, s.message)
			require.Equal(t, codes.OK, got.code, got.message)
		}

		after := scrape(t, env.collector)
		want := []struct {
			metric    string
			transport string
			delta     float64
		}{
			{metric: "otelcol_receiver_accepted_spans", transport: "http", delta: 3},
			{metric: "otelcol_receiver_accepted_spans", transport: "grpc", delta: 2},
			{metric: "otelcol_receiver_accepted_log_records", transport: "http", delta: 4},
			{metric: "otelcol_receiver_accepted_log_records", transport: "grpc", delta: 1},
			{metric: "otelcol_receiver_refused_spans", transport: "http", delta: 0},
			{metric: "otelcol_receiver_refused_spans", transport: "grpc", delta: 0},
			{metric: "otelcol_receiver_refused_log_records", transport: "http", delta: 0},
			{metric: "otelcol_receiver_refused_log_records", transport: "grpc", delta: 0},
		}
		for _, w := range want {
			labels := map[string]string{"receiver": "otlpsingleport", "transport": w.transport}
			assert.InDelta(t, w.delta, after.Sum(w.metric, labels)-before.Sum(w.metric, labels), 0,
				"%s%v moved by the wrong amount", w.metric, labels)
		}
	}
}

// oneListener checks the process listens on exactly its OTLP port, its
// health port and its metrics port.
func oneListener(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		listen, health, metrics, err := env.collector.Ports()
		require.NoError(t, err)
		ports, err := harness.ListeningPorts(env.collector.PID())
		if err != nil {
			t.Skip(err)
		}
		assert.ElementsMatch(t, []int{listen, health, metrics}, ports,
			"listening ports: want OTLP %d, health %d, metrics %d", listen, health, metrics)
	}
}

// versionAgrees checks the version subcommand and target_info's service.version
// both report the version the binary was linked with, and that a build of an
// untagged commit reports the -dev+<sha> form.
func versionAgrees(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		out, err := exec.CommandContext(t.Context(), binary.Path, "version").Output() // #nosec G204 -- the binary under test
		require.NoError(t, err)
		assert.Equal(t, binary.Version, strings.TrimSpace(string(out)))

		info, ok := scrape(t, env.collector).Find("target_info")
		require.True(t, ok, "no target_info on :8888")
		assert.Equal(t, binary.Version, info.Labels["service_version"])
		assert.Equal(t, "otlp-collector-oidc", info.Labels["service_name"])

		exact := exec.CommandContext(t.Context(), "git", "describe", "--tags", "--exact-match", "--match", "v[0-9]*.[0-9]*.[0-9]*", "HEAD")
		exact.Dir = binary.Root
		if exact.Run() == nil {
			assert.Regexp(t, `^\d+\.\d+\.\d+(-dev\+[0-9a-f]{12}\.dirty)?$`, binary.Version)
			return
		}
		sha, err := exec.CommandContext(t.Context(), "git", "-C", binary.Root, "rev-parse", "--short=12", "HEAD").Output() // #nosec G204 -- the repository under test
		require.NoError(t, err)
		assert.Regexp(t, `^\d+\.\d+\.0-dev\+`+regexp.QuoteMeta(strings.TrimSpace(string(sha)))+`(\.dirty)?$`, binary.Version)
	}
}

// upstreamDown takes the upstream away: the collector keeps accepting,
// stays healthy while its exporter fails, and delivers the queued data once
// the upstream is back.
func upstreamDown(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		env.sink.SetBehaviour(t, harness.BehaviourDown)
		t.Cleanup(func() { env.sink.SetBehaviour(t, harness.BehaviourOK) })

		for _, p := range protocols {
			got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("upstream-down/"+p.String(), 1))
			require.Equal(t, codes.OK, got.code, "a client is not told about an upstream outage: %s", got.message)
		}
		require.Eventually(t, func() bool {
			return strings.Contains(env.collector.Output(), "Exporting failed")
		}, eventually, 10*time.Millisecond, "the exporter never tried the dead upstream")
		assert.True(t, env.collector.Healthy(t.Context()), "health must not follow the upstream")

		env.sink.SetBehaviour(t, harness.BehaviourOK)
		for _, p := range protocols {
			marker := "upstream-down/" + p.String()
			require.Eventually(t, func() bool {
				_, ok := harness.FindTraces(env.sink.Received().Traces, marker)
				return ok
			}, 30*time.Second, 50*time.Millisecond, "%s was not delivered after the upstream returned", marker)
		}
	}
}

func scrape(t *testing.T, c *harness.Collector) harness.Samples {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s, err := harness.Scrape(ctx, c.MetricsAddr)
	require.NoError(t, err)
	return s
}
