package integration

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// smallCap is the body cap of the small-cap shape: small enough to build
// requests on either side of it byte for byte.
const smallCap = 1024

// TestSmallCap proves MAX_REQUEST_BODY_BYTES takes effect, and pins the cap
// at its boundary: an HTTP body may be exactly the cap, a gRPC message the
// cap less its 5-byte frame header, and one byte more is refused on each.
func TestSmallCap(t *testing.T) {
	cert := harness.NewCertificate(t)
	sink := harness.NewSink(t)
	c := harness.Start(t, binary, harness.Options{Env: map[string]string{
		"TLS_CERT_FILE":               cert.CertFile,
		"TLS_KEY_FILE":                cert.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT": sink.Endpoint(),
		"MAX_REQUEST_BODY_BYTES":      strconv.Itoa(smallCap),
	}})
	cl := newClients(t, c.ListenAddr, cert.Pool)

	httpCases := []struct {
		name       string
		size       int
		comp       harness.Compression
		wantStatus int
	}{
		{name: "exactly the cap", size: smallCap, wantStatus: http.StatusOK},
		{name: "one byte over", size: smallCap + 1, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "one byte over, gzip", size: smallCap + 1, comp: harness.CompressionGzip, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range httpCases {
		t.Run("http/"+tc.name, func(t *testing.T) {
			t.Parallel()
			req := tracesOfSize(t, "small-cap/http/"+tc.name, tc.size)
			resp, err := cl.http.Export(t.Context(), harness.SignalTraces, req, harness.EncodingProtobuf, tc.comp)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, resp.Status, "a %d-byte body against a %d-byte cap: %s", tc.size, smallCap, resp.Body)
		})
	}

	grpcCases := []struct {
		name     string
		size     int
		comp     harness.Compression
		wantCode codes.Code
	}{
		{name: "the cap less the frame header", size: smallCap - 5, wantCode: codes.OK},
		{name: "one byte over", size: smallCap - 4, wantCode: codes.ResourceExhausted},
		{name: "one byte over, gzip", size: smallCap - 4, comp: harness.CompressionGzip, wantCode: codes.ResourceExhausted},
	}
	for _, tc := range grpcCases {
		t.Run("grpc/"+tc.name, func(t *testing.T) {
			t.Parallel()
			req := tracesOfSize(t, "small-cap/grpc/"+tc.name, tc.size)
			got := grpcOutcome(cl.grpc.Export(t.Context(), req, tc.comp))
			assert.Equal(t, tc.wantCode, got.code, "a %d-byte message against a %d-byte cap: %s", tc.size, smallCap, got.message)
		})
	}
}

// tracesOfSize is a traces export request whose protobuf encoding is exactly
// size bytes.
func tracesOfSize(t *testing.T, marker string, size int) ptraceotlp.ExportRequest {
	t.Helper()
	req := harness.Traces(marker, 1)
	attrs := req.Traces().ResourceSpans().At(0).Resource().Attributes()
	pad := 0
	for range 8 {
		attrs.PutStr("padding", strings.Repeat("0", pad))
		b, err := req.MarshalProto()
		require.NoError(t, err)
		if len(b) == size {
			return req
		}
		pad += size - len(b)
		require.GreaterOrEqual(t, pad, 0, "%d bytes is smaller than an empty request", size)
	}
	require.FailNow(t, "could not build a request of the exact size", "%d bytes", size)
	return req
}

// TestRefusesToStart proves a configuration the collector cannot serve stops
// it at start, with an error that names the problem, rather than later.
func TestRefusesToStart(t *testing.T) {
	cert := harness.NewCertificate(t)
	base := map[string]string{
		"TLS_CERT_FILE":               cert.CertFile,
		"TLS_KEY_FILE":                cert.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317",
	}
	with := func(k, v string) map[string]string {
		env := map[string]string{}
		for bk, bv := range base {
			env[bk] = bv
		}
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
		return env
	}
	emptyEndpoint := strings.Replace(directConfig, "endpoint: ${env:LISTEN_ADDR}", `endpoint: ""`, 1)
	require.NotEqual(t, directConfig, emptyEndpoint)

	cases := []struct {
		name     string
		opts     harness.Options
		wantText string
	}{
		{name: "body cap of zero", opts: harness.Options{Env: with("MAX_REQUEST_BODY_BYTES", "0")}, wantText: "max_request_body_size must be between 6 and 2147483647"},
		{name: "body cap below the gRPC frame header", opts: harness.Options{Env: with("MAX_REQUEST_BODY_BYTES", "5")}, wantText: "max_request_body_size must be between 6 and 2147483647"},
		{name: "no listen endpoint", opts: harness.Options{ConfigYAML: emptyEndpoint, Env: base}, wantText: "endpoint must be set"},
		{name: "no upstream endpoint", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "")}, wantText: "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := harness.RunToExit(t, binary, tc.opts)
			assert.NotZero(t, got.ExitCode, got.Output)
			assert.Contains(t, got.Output, tc.wantText)
		})
	}
}
