package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The environment is the whole configuration interface. Every variable is
// exercised here, or in another shape, by what it changes from outside.
// LISTEN_ADDR, HEALTH_ADDR and SELF_METRICS_ADDR are moved off their defaults
// by the harness for every process (oneListener asserts where they open);
// MAX_REQUEST_BODY_BYTES is TestSmallCap; TLS_CERT_FILE and TLS_KEY_FILE are
// the mounted pair every client verifies; the embedded pair is the image
// smoke's.

// TestIdentityVariables changes every authenticator variable whose effect a
// client can see: the refusals name the new values, and a token the default
// skew would refuse passes.
func TestIdentityVariables(t *testing.T) {
	t.Parallel()
	// The scope carries a ${...} reference, which must reach the
	// authenticator as literal text: rendering escapes it.
	const scope = "otlp:${env:TMPDIR}"
	valid := func(iss *harness.Issuer) harness.Claims {
		return without(with(iss.Claims(), harness.Claims{"scope": "openid " + scope}), "preferred_username")
	}
	env := startShippedWith(t, harness.NewSink(t), map[string]string{
		"REQUIRED_SCOPE":  scope,
		"REQUIRED_CLAIMS": "sub, email",
		"CLOCK_SKEW":      "10m",
	}, valid)

	cases := []struct {
		name        string
		claims      harness.Claims
		wantCode    codes.Code
		wantMessage string
	}{
		{name: "the default scope is not enough", claims: env.issuer.Claims(), wantCode: codes.Unauthenticated, wantMessage: "missing scope: " + scope},
		{name: "a required claim missing", claims: without(valid(env.issuer), "email"), wantCode: codes.Unauthenticated, wantMessage: "missing claim: email"},
		{name: "a default claim no longer required", claims: valid(env.issuer), wantCode: codes.OK},
		{name: "expired within the widened skew", claims: with(valid(env.issuer), harness.Claims{"exp": time.Now().Add(-2 * time.Minute).Unix()}), wantCode: codes.OK},
		{name: "expired beyond the widened skew", claims: with(valid(env.issuer), harness.Claims{"exp": time.Now().Add(-11 * time.Minute).Unix()}), wantCode: codes.Unauthenticated, wantMessage: "invalid token: expired"},
	}
	for _, tc := range cases {
		for _, p := range protocols {
			t.Run(tc.name+"/"+p.String(), func(t *testing.T) {
				token := env.issuer.Sign(t, jose.RS256, tc.claims)
				got := env.clients.present(t, p, bearer(token), "identity/"+tc.name+"/"+p.String())
				assert.Equal(t, tc.wantCode, got.code, "claims %v: %s", tc.claims, got.message)
				if tc.wantMessage != "" {
					assert.Equal(t, tc.wantMessage, got.message, "claims %v", tc.claims)
				}
			})
		}
	}
}

// TestUpstreamTLS points OTEL_EXPORTER_OTLP_ENDPOINT at an https:// sink whose
// certificate the process trusts through SSL_CERT_FILE: data arrives, so the
// exporter spoke TLS and verified the chain.
func TestUpstreamTLS(t *testing.T) {
	t.Parallel()
	upstreamCert := harness.NewCertificate(t)
	sink := harness.NewTLSSink(t, upstreamCert)
	require.True(t, strings.HasPrefix(sink.Endpoint(), "https://"))
	env := startShippedWith(t, sink, map[string]string{"SSL_CERT_FILE": upstreamCert.CertFile}, nil)

	for _, p := range protocols {
		marker := "upstream-tls/" + p.String()
		got := env.clients.export(t.Context(), t, p, harness.SignalTraces, harness.Traces(marker, 1))
		require.Equal(t, codes.OK, got.code, got.message)
		require.Eventually(t, func() bool {
			_, ok := harness.FindTraces(sink.Received().Traces, marker)
			return ok
		}, eventually, 10*time.Millisecond, "%s never reached the TLS sink", marker)
	}
}

// corsOrigin is the origin the CORS shape allows.
const corsOrigin = "https://app.example.com"

// preflight asks, as a browser on origin would, whether it may POST traces
// with a bearer token.
func preflight(t *testing.T, cl clients, origin string) harness.Response {
	t.Helper()
	resp, err := cl.http.WithBearer("").Do(t.Context(), http.MethodOptions, harness.SignalTraces.Path(), http.Header{
		"Origin":                         {origin},
		"Access-Control-Request-Method":  {http.MethodPost},
		"Access-Control-Request-Headers": {"authorization,content-type"},
	}, nil)
	require.NoError(t, err)
	return resp
}

// TestCORS allows one origin. CORS is a browser's rule, and browsers speak
// OTLP/HTTP only, so this shape has no gRPC half.
func TestCORS(t *testing.T) {
	t.Parallel()
	env := startShipped(t, map[string]string{"CORS_ALLOWED_ORIGINS": corsOrigin + ", https://other.example.net"})

	t.Run("preflight from an allowed origin", func(t *testing.T) {
		resp := preflight(t, env.clients, corsOrigin)
		assert.Less(t, resp.Status, 300, "preflight answered %d", resp.Status)
		assert.Equal(t, corsOrigin, resp.Header.Get("Access-Control-Allow-Origin"))
		assert.Contains(t, strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers")), "authorization")
	})
	t.Run("preflight from another origin", func(t *testing.T) {
		resp := preflight(t, env.clients, "https://evil.example.com")
		assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "headers: %v", resp.Header)
	})
	t.Run("export from an allowed origin", func(t *testing.T) {
		body, err := harness.Traces("cors/export", 1).MarshalProto()
		require.NoError(t, err)
		resp, err := env.clients.http.Do(t.Context(), http.MethodPost, harness.SignalTraces.Path(), http.Header{
			"Origin":       {corsOrigin},
			"Content-Type": {"application/x-protobuf"},
		}, body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.Status, "%s", resp.Body)
		assert.Equal(t, corsOrigin, resp.Header.Get("Access-Control-Allow-Origin"))
	})
}

// corsOff is the shipped shape's half of TestCORS: with CORS_ALLOWED_ORIGINS
// unset, no answer carries a CORS header, whatever the origin asks.
func corsOff(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		t.Parallel()
		resp := preflight(t, env.clients, corsOrigin)
		for name := range resp.Header {
			assert.False(t, strings.HasPrefix(strings.ToLower(name), "access-control-"), "%s: %v", name, resp.Header)
		}
	}
}

// defaultLogs is the shipped shape's logging: JSON lines on stdout, the
// startup line naming the rendered source and the resolved values, and no
// rendered YAML at info.
func defaultLogs(env shipped) func(*testing.T) {
	return func(t *testing.T) {
		t.Parallel()
		out := env.collector.Stdout()
		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.NotEmpty(t, lines)
		var startup map[string]any
		for _, line := range lines {
			var entry map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &entry), "not a JSON line: %s", line)
			if entry["msg"] == "Rendered the configuration from the environment" {
				startup = entry
			}
		}
		require.NotNil(t, startup, "no startup line:\n%s", out)
		assert.Equal(t, "rendered", startup["source"])
		assert.Equal(t, harness.Audience, startup["OIDC_AUDIENCE"])
		assert.Equal(t, "50ms", startup["BATCH_TIMEOUT"])
		assert.Equal(t, "sub,preferred_username", startup["REQUIRED_CLAIMS"], "defaults are logged too")
		upstream := "grpc " + strings.TrimPrefix(env.sink.Endpoint(), "http://") + " plaintext"
		for _, signal := range []string{"traces", "logs"} {
			assert.Equal(t, upstream, startup["upstream."+signal], "each signal's resolved upstream")
		}
		assert.NotContains(t, startup, "upstream.metrics", "metrics has no pipeline, so no upstream")
		assert.NotContains(t, out, "Rendered configuration", "the YAML is logged at debug only")
	}
}

// TestDebugConsoleLogs sets LOG_LEVEL=debug and LOG_FORMAT=console: the
// rendered YAML is logged, and stdout is no longer JSON.
func TestDebugConsoleLogs(t *testing.T) {
	t.Parallel()
	env := startShipped(t, map[string]string{"LOG_LEVEL": "debug", "LOG_FORMAT": "console"})
	out := env.collector.Stdout()
	assert.Contains(t, out, "Rendered configuration")
	assert.Contains(t, out, "otlpsingleport:", "the rendered YAML is in the debug line")
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		assert.False(t, strings.HasPrefix(line, "{"), "a JSON line with LOG_FORMAT=console: %s", line)
	}
	assert.Contains(t, out, "\tdebug\t", "debug lines are written")
}

// mountedConfig is a whole pipeline of the deployer's own, using both
// custom components. Its values are its own; only the addresses the harness
// moves are read from the environment.
const mountedConfig = `
receivers:
  otlpsingleport:
    endpoint: ${env:LISTEN_ADDR}
    tls:
      cert_file: %[1]q
      key_file: %[2]q
    auth:
      authenticator: oidcclientauth
exporters:
  otlp_grpc:
    endpoint: %[3]q
    tls:
      insecure: true
extensions:
  health_check:
    endpoint: ${env:HEALTH_ADDR}
  oidcclientauth:
    issuer_url: %[4]q
    audience: %[5]q
service:
  extensions: [health_check, oidcclientauth]
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: ${env:HARNESS_METRICS_HOST}
                port: ${env:HARNESS_METRICS_PORT}
  pipelines:
    traces:
      receivers: [otlpsingleport]
      exporters: [otlp_grpc]
`

// TestMountedConfig runs a COLLECTOR_CONFIG file with none of the required
// variables set: rendering is skipped, the file's pipeline runs, and both
// custom components work from it.
func TestMountedConfig(t *testing.T) {
	t.Parallel()
	cert := harness.NewCertificate(t)
	sink := harness.NewSink(t)
	iss := harness.NewIssuer(t)
	iss.Start(t)
	yaml := fmt.Sprintf(mountedConfig, cert.CertFile, cert.KeyFile, strings.TrimPrefix(sink.Endpoint(), "http://"), iss.URL(), harness.Audience)
	c := harness.Start(t, binary, harness.Options{ConfigYAML: yaml})
	cl := newClients(t, c.ListenAddr, cert.Pool, iss.Sign(t, jose.RS256, iss.Claims()))
	awaitReady(t, cl)

	assert.Contains(t, c.Stdout(), `"source":"mounted"`)
	assert.NotContains(t, c.Stdout(), "Rendered the configuration")
	for _, p := range protocols {
		t.Run("no token/"+p.String(), func(t *testing.T) {
			got := cl.present(t, p, presentation{}, "mounted/no-token/"+p.String())
			assert.Equal(t, codes.Unauthenticated, got.code, got.message)
			assert.Equal(t, "no token", got.message)
		})
		t.Run("valid token/"+p.String(), func(t *testing.T) {
			marker := "mounted/" + p.String()
			got := cl.export(t.Context(), t, p, harness.SignalTraces, harness.Traces(marker, 1))
			require.Equal(t, codes.OK, got.code, got.message)
			require.Eventually(t, func() bool {
				_, ok := harness.FindTraces(sink.Received().Traces, marker)
				return ok
			}, eventually, 10*time.Millisecond, "%s never reached the sink", marker)
		})
	}
}

// TestCertificateReload replaces the mounted pair on disk under a running
// process: within TLS_RELOAD_INTERVAL, new connections are served the new
// certificate, with no restart.
func TestCertificateReload(t *testing.T) {
	t.Parallel()
	first := harness.NewCertificate(t)
	second := harness.NewCertificate(t)
	sink := harness.NewSink(t)
	iss := harness.NewIssuer(t)
	iss.Start(t)
	vars := oidcEnv(iss)
	vars["TLS_CERT_FILE"] = first.CertFile
	vars["TLS_KEY_FILE"] = first.KeyFile
	vars["TLS_RELOAD_INTERVAL"] = "100ms"
	vars["OTEL_EXPORTER_OTLP_ENDPOINT"] = sink.Endpoint()
	c := harness.Start(t, binary, harness.Options{Env: vars})
	token := iss.Sign(t, jose.RS256, iss.Claims())
	awaitReady(t, newClients(t, c.ListenAddr, first.Pool, token))

	for _, f := range []struct{ from, to string }{{second.CertFile, first.CertFile}, {second.KeyFile, first.KeyFile}} {
		b, err := os.ReadFile(f.from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(f.to, b, 0o600)) // #nosec G703 -- the test's own temporary files
	}

	// A new client each time: a new connection is a new handshake, which
	// fails until the process serves a certificate the client trusts.
	servesSecond := map[protocol]func() bool{
		protocolHTTP: func() bool {
			resp, err := harness.NewHTTPClient(c.ListenAddr, second.Pool, false).WithBearer(token).
				Export(t.Context(), harness.SignalTraces, harness.Traces("reload/http", 1), harness.EncodingProtobuf, harness.CompressionNone)
			return err == nil && resp.Status == http.StatusOK
		},
		protocolGRPC: func() bool {
			return harness.NewGRPCClient(t, c.ListenAddr, second.Pool).WithBearer(token).
				Export(t.Context(), harness.Traces("reload/grpc", 1), harness.CompressionNone) == nil
		},
	}
	for _, p := range protocols {
		require.Eventually(t, servesSecond[p], eventually, 100*time.Millisecond, "the replaced certificate was never served over %s", p)
	}
}

// TestRenderedFile plants a symlink where the rendered configuration goes,
// as another user of a shared temporary directory could: the process
// replaces it with a file of its own, readable only by itself, and never
// writes through it.
func TestRenderedFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
	rendered := filepath.Join(tmp, "otlp-collector-oidc.yaml")
	require.NoError(t, os.Symlink(victim, rendered))

	env := startShipped(t, map[string]string{"TMPDIR": tmp})

	got, err := os.ReadFile(victim) // #nosec G304 -- the test's own file
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got), "the rendered configuration was written through the symlink")
	info, err := os.Lstat(rendered)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "mode %v", info.Mode())
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Contains(t, env.collector.Stdout(), `"path":"`+rendered+`"`)
}
