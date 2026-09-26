package integration

import (
	"maps"
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
	cl := startShipped(t, map[string]string{"MAX_REQUEST_BODY_BYTES": strconv.Itoa(smallCap)}).clients

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
// it before it listens, with an error on stderr that names the problem: the
// variable and its value when the environment is at fault, the setting when
// a component refuses a value that parsed.
func TestRefusesToStart(t *testing.T) {
	cert := harness.NewCertificate(t)
	base := map[string]string{
		"TLS_CERT_FILE":               cert.CertFile,
		"TLS_KEY_FILE":                cert.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317",
		"OIDC_ISSUER_URL":             "http://127.0.0.1:1",
		"OIDC_AUDIENCE":               harness.Audience,
		"ALLOWED_SERVICE_NAMES":       ".*",
	}
	// with is base with k set to v, or without k when v is empty.
	with := func(k, v string) map[string]string {
		env := maps.Clone(base)
		if v == "" {
			delete(env, k)
		} else {
			env[k] = v
		}
		return env
	}
	// withEach is base with each key, value pair of kv set.
	withEach := func(kv ...string) map[string]string {
		env := maps.Clone(base)
		for i := 0; i+1 < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	// set is base with k set to v, even to the empty string.
	set := func(k, v string) map[string]string {
		env := maps.Clone(base)
		env[k] = v
		return env
	}
	emptyEndpoint := strings.Replace(directConfig, "endpoint: ${env:LISTEN_ADDR}", `endpoint: ""`, 1)
	require.NotEqual(t, directConfig, emptyEndpoint)

	cases := []struct {
		name     string
		opts     harness.Options
		wantText string
		alsoText string
		// absentText must appear nowhere in the output: a secret.
		absentText string
	}{
		// Required variables, unset or empty.
		{name: "no issuer", opts: harness.Options{Env: with("OIDC_ISSUER_URL", "")}, wantText: "OIDC_ISSUER_URL is required"},
		{name: "empty issuer", opts: harness.Options{Env: set("OIDC_ISSUER_URL", "")}, wantText: "OIDC_ISSUER_URL is required"},
		{name: "no audience", opts: harness.Options{Env: with("OIDC_AUDIENCE", "")}, wantText: "OIDC_AUDIENCE is required"},
		{name: "no allowed service names", opts: harness.Options{Env: with("ALLOWED_SERVICE_NAMES", "")}, wantText: "ALLOWED_SERVICE_NAMES is required"},
		{name: "allowed service names not a regex", opts: harness.Options{Env: with("ALLOWED_SERVICE_NAMES", "app-(web")}, wantText: `invalid ALLOWED_SERVICE_NAMES "app-(web": must be a regular expression: missing closing )`},
		{name: "allowed service names escaping the anchors", opts: harness.Options{Env: with("ALLOWED_SERVICE_NAMES", "a)|(b")}, wantText: `invalid ALLOWED_SERVICE_NAMES "a)|(b": must be a regular expression: unexpected )`},
		{name: "allowed service names with a newline", opts: harness.Options{Env: with("ALLOWED_SERVICE_NAMES", "app\nother")}, wantText: "invalid ALLOWED_SERVICE_NAMES", alsoText: "must not contain control characters"},
		{name: "allowed metric names not a regex", opts: harness.Options{Env: with("ALLOWED_METRIC_NAMES", "app.(x")}, wantText: `invalid ALLOWED_METRIC_NAMES "app.(x": must be a regular expression: missing closing )`},
		{name: "no metric streams", opts: harness.Options{Env: with("MAX_METRIC_STREAMS", "0")}, wantText: "MAX_METRIC_STREAMS (0) must be at least 1"},
		{name: "zero delta staleness", opts: harness.Options{Env: with("DELTA_MAX_STALE", "0s")}, wantText: "DELTA_MAX_STALE (0s) must be positive"},
		{name: "negative future skew", opts: harness.Options{Env: with("MAX_FUTURE_SKEW", "-1m")}, wantText: "MAX_FUTURE_SKEW (-1m0s) must not be negative"},
		{name: "zero past age", opts: harness.Options{Env: with("MAX_PAST_AGE", "0s")}, wantText: "MAX_PAST_AGE (0s) must be positive"},
		{name: "no upstream endpoint", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "")}, wantText: "OTEL_EXPORTER_OTLP_ENDPOINT is required"},
		// Values that do not parse.
		{name: "bad duration", opts: harness.Options{Env: with("OIDC_DISCOVERY_RETRY", "soon")}, wantText: `invalid OIDC_DISCOVERY_RETRY "soon": must be a duration`},
		{name: "bad listen address", opts: harness.Options{Env: with("LISTEN_ADDR", "4318")}, wantText: `invalid LISTEN_ADDR "4318": must be host:port`},
		{name: "bad health port", opts: harness.Options{Env: with("HEALTH_ADDR", "127.0.0.1:http")}, wantText: `invalid HEALTH_ADDR "127.0.0.1:http": port must be a number`},
		{name: "bad MiB", opts: harness.Options{Env: with("MEMORY_LIMIT_MIB", "64MiB")}, wantText: `invalid MEMORY_LIMIT_MIB "64MiB": must be a whole number of MiB`},
		{name: "bad body cap", opts: harness.Options{Env: with("MAX_REQUEST_BODY_BYTES", "4M")}, wantText: `invalid MAX_REQUEST_BODY_BYTES "4M": must be a whole number`},
		{name: "upstream without a scheme", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")}, wantText: `invalid OTEL_EXPORTER_OTLP_ENDPOINT "collector:4317": must be an http:// or https:// URL`},
		{name: "upstream without a port", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector")}, wantText: `invalid OTEL_EXPORTER_OTLP_ENDPOINT "http://collector": must name a port`},
		{name: "upstream with a path", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "https://gw.example.com:4317/otlp")}, wantText: "must be scheme://host:port only"},
		{name: "upstream with credentials", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "https://user:secret@gw.example.com:4317")}, wantText: `"https://user:xxxxx@gw.example.com:4317": must be scheme://host:port only`, absentText: "secret"}, // #nosec G101 -- a made-up password, to prove it is never printed
		{name: "upstream with a query", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_ENDPOINT", "https://gw.example.com:4317?tenant=a")}, wantText: "must be scheme://host:port only"},
		{name: "bad upstream protocol", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")}, wantText: `invalid OTEL_EXPORTER_OTLP_PROTOCOL "http/json": must be grpc or http/protobuf`},
		{name: "timeout as a duration", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "10s")}, wantText: `invalid OTEL_EXPORTER_OTLP_LOGS_TIMEOUT "10s": must be a whole number of milliseconds`},
		{name: "bad compression", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_COMPRESSION", "zstd")}, wantText: `invalid OTEL_EXPORTER_OTLP_COMPRESSION "zstd": must be gzip or none`},
		{name: "bad insecure", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_INSECURE", "yes")}, wantText: `invalid OTEL_EXPORTER_OTLP_INSECURE "yes": must be true or false`},
		{name: "bad queue size", opts: harness.Options{Env: with("UPSTREAM_QUEUE_SIZE", "0")}, wantText: `invalid UPSTREAM_QUEUE_SIZE "0": must be a whole number, at least 1`},
		{name: "bad retry limit", opts: harness.Options{Env: with("UPSTREAM_RETRY_MAX_ELAPSED", "-1s")}, wantText: `invalid UPSTREAM_RETRY_MAX_ELAPSED "-1s": must be a duration`},
		{name: "headers without a value", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_HEADERS", "x-ok=1,Authorization: Bearer hunter2")}, wantText: "invalid OTEL_EXPORTER_OTLP_HEADERS: pair 2 is not key=value", absentText: "hunter2"}, // #nosec G101 -- made up, to prove it is never printed
		{name: "per-signal gRPC upstream with a path", opts: harness.Options{Env: set("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://gw.example.com:4317/v1/traces")}, wantText: "invalid OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", alsoText: "must be scheme://host:port only"},
		{name: "header value with a newline", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20hunter2%0A")}, wantText: "invalid OTEL_EXPORTER_OTLP_HEADERS: pair 1 has a value with a control or non-ASCII character", absentText: "hunter2"}, // #nosec G101 -- made up, to prove it is never printed
		{name: "http upstream with credentials", opts: harness.Options{Env: map[string]string{
			"OIDC_ISSUER_URL":             "http://127.0.0.1:1",
			"OIDC_AUDIENCE":               harness.Audience,
			"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "https://user:secret@gw.example.com/otlp",
		}}, wantText: `"https://user:xxxxx@gw.example.com/otlp": must be a URL with no credentials, query or fragment`, absentText: "secret"}, // #nosec G101 -- a made-up password, to prove it is never printed
		// Rules across the upstream variables.
		{name: "client certificate without its key", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "/run/client.pem")}, wantText: "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE and OTEL_EXPORTER_OTLP_CLIENT_KEY must be set together"},
		{name: "CA for a plaintext upstream", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_CERTIFICATE", "/run/ca.pem")}, wantText: "OTEL_EXPORTER_OTLP_CERTIFICATE is set, but every upstream is plaintext"},
		{name: "per-signal CA for a plaintext signal", opts: harness.Options{Env: with("OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", "/run/ca.pem")}, wantText: "OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE is set, but the logs upstream is plaintext"},
		{name: "per-signal insecure over https with http/protobuf", opts: harness.Options{Env: withEach("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "https://loki.example.com/otlp/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_INSECURE", "true")}, wantText: "OTEL_EXPORTER_OTLP_LOGS_INSECURE=true applies to grpc only"},
		{name: "insecure with no grpc upstream", opts: harness.Options{Env: withEach("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf", "OTEL_EXPORTER_OTLP_INSECURE", "true")}, wantText: "OTEL_EXPORTER_OTLP_INSECURE=true, but no upstream uses grpc"},
		{name: "claim mapped to the client's service.name", opts: harness.Options{Env: with("CLAIM_ATTRIBUTES", "sub=service.name")}, wantText: "CLAIM_ATTRIBUTES: service.name is the client's, not a claim's"},
		{name: "claim mapped to the client's service.version", opts: harness.Options{Env: with("CLAIM_ATTRIBUTES", "sub=service.version")}, wantText: "CLAIM_ATTRIBUTES: service.version is the client's, not a claim's"},
		{name: "no claim mapped", opts: harness.Options{Env: with("CLAIM_ATTRIBUTES", ",")}, wantText: "CLAIM_ATTRIBUTES must map at least one claim"},
		{name: "client resource on a claim's attribute", opts: harness.Options{Env: with("CLIENT_RESOURCE_ATTRIBUTES", "user.id=x")}, wantText: "CLIENT_RESOURCE_ATTRIBUTES: user.id is the attribute of claim sub"},
		{name: "claim mapped to nothing", opts: harness.Options{Env: with("CLAIM_ATTRIBUTES", "sub=")}, wantText: "CLAIM_ATTRIBUTES: claim sub maps to no attribute"},
		{name: "two claims to one attribute", opts: harness.Options{Env: with("CLAIM_ATTRIBUTES", "sub=user.id,oid=user.id")}, wantText: "CLAIM_ATTRIBUTES: claims sub and oid both map to user.id"},
		{name: "client resource overwriting service.version", opts: harness.Options{Env: with("CLIENT_RESOURCE_ATTRIBUTES", "service.version=9")}, wantText: "CLIENT_RESOURCE_ATTRIBUTES: service.version is the client's"},
		{name: "malformed client resource attributes", opts: harness.Options{Env: with("CLIENT_RESOURCE_ATTRIBUTES", "env")}, wantText: `invalid CLIENT_RESOURCE_ATTRIBUTES "env": pair 1 is not key=value`},
		{name: "bad log output", opts: harness.Options{Env: with("LOG_OUTPUT", "file")}, wantText: `invalid LOG_OUTPUT "file": must be both, otlp or stdout`},
		{name: "malformed resource attributes", opts: harness.Options{Env: with("OTEL_RESOURCE_ATTRIBUTES", "team=obs,oops")}, wantText: `invalid OTEL_RESOURCE_ATTRIBUTES "team=obs,oops": pair 2 is not key=value`},
		{name: "skip-verify with own logs upstream", opts: harness.Options{Env: withEach("OTEL_EXPORTER_OTLP_ENDPOINT", "https://gw.example.com:4317", "UPSTREAM_TLS_INSECURE_SKIP_VERIFY", "true")}, wantText: "UPSTREAM_TLS_INSECURE_SKIP_VERIFY cannot apply to the process's own logs"},
		{name: "bad log level", opts: harness.Options{Env: with("LOG_LEVEL", "verbose")}, wantText: `invalid LOG_LEVEL "verbose": must be debug, info, warn or error`},
		{name: "bad log format", opts: harness.Options{Env: with("LOG_FORMAT", "text")}, wantText: `invalid LOG_FORMAT "text": must be json or console`},
		// Faults in different groups are reported together.
		{name: "two faults at once", opts: harness.Options{Env: map[string]string{
			"LOG_LEVEL":                   "verbose",
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317",
			"OIDC_AUDIENCE":               harness.Audience,
		}}, wantText: `invalid LOG_LEVEL "verbose"`, alsoText: "OIDC_ISSUER_URL is required"},
		{name: "mounted with a bad log level", opts: harness.Options{ConfigYAML: directConfig, Env: set("LOG_FORMAT", "text")}, wantText: `invalid LOG_FORMAT "text"`},
		// Rules across a group's variables.
		{name: "certificate without its key", opts: harness.Options{Env: with("TLS_KEY_FILE", "")}, wantText: "TLS_CERT_FILE and TLS_KEY_FILE must be set together"},
		{name: "spike limit above the limit", opts: harness.Options{Env: with("MEMORY_SPIKE_LIMIT_MIB", "64")}, wantText: "MEMORY_SPIKE_LIMIT_MIB (64) must be less than MEMORY_LIMIT_MIB (64)"},
		// Values that parse, refused by the component they configure.
		{name: "body cap of zero", opts: harness.Options{Env: with("MAX_REQUEST_BODY_BYTES", "0")}, wantText: "max_request_body_size must be between 6 and 2147483647"},
		{name: "body cap below the gRPC frame header", opts: harness.Options{Env: with("MAX_REQUEST_BODY_BYTES", "5")}, wantText: "max_request_body_size must be between 6 and 2147483647"},
		{name: "issuer is not a URL", opts: harness.Options{Env: with("OIDC_ISSUER_URL", "issuer.example.com")}, wantText: "issuer_url must be an absolute http or https URL"},
		{name: "required scope with a space", opts: harness.Options{Env: with("REQUIRED_SCOPE", "telemetry write")}, wantText: "required_scope must be one non-empty scope token"},
		{name: "negative clock skew", opts: harness.Options{Env: with("CLOCK_SKEW", "-1s")}, wantText: "clock_skew must not be negative"},
		// A mounted configuration is the collector's to validate.
		{name: "no listen endpoint", opts: harness.Options{ConfigYAML: emptyEndpoint, Env: base}, wantText: "endpoint must be set"},
		{name: "mounted file missing", opts: harness.Options{Env: with("COLLECTOR_CONFIG", "/nonexistent/collector.yaml")}, wantText: "/nonexistent/collector.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := harness.RunToExit(t, binary, tc.opts)
			assert.NotZero(t, got.ExitCode, got.Output)
			assert.Contains(t, got.Stderr, tc.wantText, "stderr, of all output:\n%s", got.Output)
			assert.Contains(t, got.Stderr, tc.alsoText, "stderr, of all output:\n%s", got.Output)
			if tc.absentText != "" {
				assert.NotContains(t, got.Output, tc.absentText)
			}
		})
	}
}
