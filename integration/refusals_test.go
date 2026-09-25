package integration

import (
	binenc "encoding/binary"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// refusedMetric counts requests refused before the pipeline, by transport and
// reason.
const refusedMetric = "otelcol_otlpsingleport_requests_refused"

// grpcTracesPath is the traces Export method, for raw gRPC requests.
const grpcTracesPath = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"

// grpcFrame wraps payload in the 5-byte gRPC length prefix.
func grpcFrame(payload []byte) []byte {
	frame := make([]byte, 5, 5+len(payload))
	binenc.BigEndian.PutUint32(frame[1:], uint32(len(payload))) // #nosec G115 -- test payloads are small
	return append(frame, payload...)
}

// refusalCounters checks every refusal before the pipeline is counted once,
// under its transport and reason, over every protocol that can produce it.
func refusalCounters(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		gzipBomb, err := harness.Gzip(make([]byte, 5<<20))
		require.NoError(t, err)
		big := harness.Traces("refusals-big", 1)
		big.Traces().ResourceSpans().At(0).Resource().Attributes().PutStr("padding", strings.Repeat("0", 5<<20))
		pb := http.Header{"Content-Type": {"application/x-protobuf"}}
		grpcHeader := http.Header{"Content-Type": {"application/grpc"}, "Te": {"trailers"}}

		cases := []struct {
			name      string
			transport string
			reason    string
			send      func(t *testing.T)
		}{
			{name: "wrong method", transport: "http", reason: "method", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodGet, "/v1/traces", nil, nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusMethodNotAllowed, resp.Status)
			}},
			{name: "wrong media type", transport: "http", reason: "media_type", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/traces", http.Header{"Content-Type": {"text/plain"}}, []byte("x"))
				require.NoError(t, err)
				require.Equal(t, http.StatusUnsupportedMediaType, resp.Status)
			}},
			{name: "body too large", transport: "http", reason: "body_too_large", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/traces", http.Header{"Content-Type": {"application/x-protobuf"}, "Content-Encoding": {"gzip"}}, gzipBomb)
				require.NoError(t, err)
				require.Equal(t, http.StatusRequestEntityTooLarge, resp.Status)
			}},
			{name: "undecodable body", transport: "http", reason: "decode", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/traces", pb, []byte{0xff, 0xff, 0xff})
				require.NoError(t, err)
				require.Equal(t, http.StatusBadRequest, resp.Status)
			}},
			{name: "unwired signal", transport: "http", reason: "unknown_path", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/metrics", pb, nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusNotFound, resp.Status)
			}},
			{name: "malformed gzip", transport: "http", reason: "decompress", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/v1/traces", http.Header{"Content-Type": {"application/x-protobuf"}, "Content-Encoding": {"gzip"}}, []byte("not gzip"))
				require.NoError(t, err)
				require.Equal(t, http.StatusBadRequest, resp.Status)
			}},
			{name: "unwired signal", transport: "grpc", reason: "unknown_path", send: func(t *testing.T) {
				t.Helper()
				require.Error(t, env.clients.grpc.Export(t.Context(), harness.Metrics("refusals", 1), harness.CompressionNone))
			}},
			{name: "message too large", transport: "grpc", reason: "body_too_large", send: func(t *testing.T) {
				t.Helper()
				require.Error(t, env.clients.grpc.Export(t.Context(), big, harness.CompressionGzip))
			}},
			{name: "undecodable message", transport: "grpc", reason: "decode", send: func(t *testing.T) {
				t.Helper()
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, grpcTracesPath, grpcHeader, grpcFrame([]byte{0xff, 0xff, 0xff}))
				require.NoError(t, err)
				require.NotEqual(t, "0", resp.Trailer.Get("Grpc-Status"), "an undecodable message must fail")
				require.NotEmpty(t, resp.Trailer.Get("Grpc-Status"))
			}},
			{name: "unsupported content encoding", transport: "grpc", reason: "decompress", send: func(t *testing.T) {
				t.Helper()
				h := grpcHeader.Clone()
				h.Set("Content-Encoding", "br")
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, grpcTracesPath, h, grpcFrame(nil))
				require.NoError(t, err)
				require.Equal(t, http.StatusBadRequest, resp.Status)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.transport+"/"+tc.name, func(t *testing.T) {
				labels := map[string]string{"receiver": "otlpsingleport", "transport": tc.transport, "reason": tc.reason}
				before := scrape(t, env.collector)
				tc.send(t)
				after := scrape(t, env.collector)
				assert.InDelta(t, 1, after.Sum(refusedMetric, labels)-before.Sum(refusedMetric, labels), 0,
					"%s%v", refusedMetric, labels)
				// Exactly one refusal in total: nothing else was counted with it.
				all := map[string]string{"receiver": "otlpsingleport"}
				assert.InDelta(t, 1, after.Sum(refusedMetric, all)-before.Sum(refusedMetric, all), 0,
					"one request, one refusal")
			})
		}

		t.Run("an accepted request is not a refusal", func(t *testing.T) {
			all := map[string]string{"receiver": "otlpsingleport"}
			before := scrape(t, env.collector)
			for _, p := range protocols {
				got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("refusals-ok", 1))
				require.Equal(t, "OK", got.code.String(), got.message)
			}
			after := scrape(t, env.collector)
			assert.InDelta(t, 0, after.Sum(refusedMetric, all)-before.Sum(refusedMetric, all), 0)
		})
	}
}
