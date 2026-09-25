package integration

import (
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// rejectionsMetric counts requests the authenticator refused, by reason.
const rejectionsMetric = "otelcol_oidcclientauth_rejections"

// rejectionReasons is the closed set of reason labels docs/token-profile.md
// documents.
var rejectionReasons = []string{"no_token", "invalid_token", "missing_scope", "missing_claim", "not_ready"}

// oidcEnv points a collector at iss.
func oidcEnv(iss *harness.Issuer) map[string]string {
	return map[string]string{
		"OIDC_ISSUER_URL": iss.URL(),
		"OIDC_AUDIENCE":   harness.Audience,
	}
}

// awaitReady waits until the collector has loaded its provider: health
// answers before discovery completes, since discovery never gates it.
func awaitReady(t *testing.T, cl clients) {
	t.Helper()
	require.Eventually(t, func() bool {
		return cl.export(t.Context(), t, protocolHTTP, harness.SignalTraces, harness.Traces("ready", 0)).httpStatus == http.StatusOK
	}, eventually, 20*time.Millisecond, "the collector never became ready")
}

// presentation is how a request carries its credential, or fails to.
type presentation struct {
	// authorization is the whole Authorization value; empty sends none.
	authorization string
	// cookie is a Cookie header over HTTP, cookie metadata over gRPC.
	cookie string
	// query is an access_token query parameter; HTTP only.
	query string
}

func bearer(token string) presentation {
	return presentation{authorization: "Bearer " + token}
}

// present exports one span carrying marker, presenting pr and nothing else.
func (c clients) present(t *testing.T, p protocol, pr presentation, marker string) outcome {
	t.Helper()
	m := harness.Traces(marker, 1)
	switch p {
	case protocolHTTP:
		body, err := m.MarshalProto()
		require.NoError(t, err)
		h := http.Header{"Content-Type": {"application/x-protobuf"}}
		if pr.authorization != "" {
			h.Set("Authorization", pr.authorization)
		}
		if pr.cookie != "" {
			h.Set("Cookie", pr.cookie)
		}
		path := harness.SignalTraces.Path()
		if pr.query != "" {
			path += "?access_token=" + url.QueryEscape(pr.query)
		}
		resp, err := c.http.WithBearer("").Do(t.Context(), http.MethodPost, path, h, body)
		require.NoError(t, err)
		if resp.Status == http.StatusUnauthorized {
			assert.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"), "a 401 names its scheme")
		}
		return httpOutcome(t, resp)
	case protocolGRPC:
		require.Empty(t, pr.query, "gRPC has no query parameters")
		ctx := t.Context()
		if pr.authorization != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", pr.authorization)
		}
		if pr.cookie != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "cookie", pr.cookie)
		}
		return grpcOutcome(c.grpc.WithBearer("").Export(ctx, m, harness.CompressionNone))
	default:
		require.FailNow(t, "unknown protocol", "%d", int(p))
		return outcome{}
	}
}

// with returns a copy of c with kv set.
func with(c harness.Claims, kv harness.Claims) harness.Claims {
	out := maps.Clone(c)
	maps.Copy(out, kv)
	return out
}

// without returns a copy of c without keys.
func without(c harness.Claims, keys ...string) harness.Claims {
	out := maps.Clone(c)
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// TestAuth is the token profile's conformance suite: every rule in
// docs/token-profile.md is a scenario here, over both protocols, asserting
// the exact refusal text, the counter and that nothing reaches the sink.
func TestAuth(t *testing.T) {
	// A warning interval longer than the run, so every reason warns exactly
	// once however long the run takes.
	env := startShipped(t, map[string]string{"REJECTION_LOG_INTERVAL": "1h"})
	iss := env.issuer
	valid := iss.Claims()
	now := time.Now()

	// tokens is every token the run presents, none of which may be logged.
	var tokens []string
	keep := func(token string) string {
		tokens = append(tokens, token)
		return token
	}

	t.Run("accepted", func(t *testing.T) {
		cases := []struct {
			name string
			pr   presentation
		}{
			{name: "RS256", pr: bearer(keep(iss.Sign(t, jose.RS256, valid)))},
			{name: "RS384", pr: bearer(keep(iss.Sign(t, jose.RS384, valid)))},
			{name: "RS512", pr: bearer(keep(iss.Sign(t, jose.RS512, valid)))},
			{name: "ES256", pr: bearer(keep(iss.Sign(t, jose.ES256, valid)))},
			{name: "lower-case scheme", pr: presentation{authorization: "bearer " + keep(iss.Sign(t, jose.RS256, valid))}},
			{name: "aud array containing the audience", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"aud": []string{"other", harness.Audience}}))))},
			{name: "scp array", pr: bearer(keep(iss.Sign(t, jose.RS256, with(without(valid, "scope"), harness.Claims{"scp": []string{"openid", "telemetry:write"}}))))},
			{name: "scp string", pr: bearer(keep(iss.Sign(t, jose.RS256, with(without(valid, "scope"), harness.Claims{"scp": "openid telemetry:write"}))))},
			{name: "optional claims absent", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "email", "name"))))},
			{name: "no iat", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "iat"))))},
			{name: "unknown claims ignored", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"groups": []string{"a"}, "acr": "1"}))))},
			{name: "expired within the clock skew", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"exp": now.Add(-30 * time.Second).Unix()}))))},
			{name: "nbf ahead within the clock skew", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"nbf": now.Add(30 * time.Second).Unix()}))))},
			{name: "iat ahead within the clock skew", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"iat": now.Add(30 * time.Second).Unix()}))))},
		}
		for _, tc := range cases {
			for _, p := range protocols {
				t.Run(tc.name+"/"+p.String(), func(t *testing.T) {
					t.Parallel()
					marker := "auth/accepted/" + tc.name + "/" + p.String()
					got := env.clients.present(t, p, tc.pr, marker)
					require.Equal(t, codes.OK, got.code, got.message)
					require.Eventually(t, func() bool {
						_, ok := harness.FindTraces(env.sink.Received().Traces, marker)
						return ok
					}, eventually, 10*time.Millisecond, "%s never reached the sink", marker)
				})
			}
		}
	})

	validToken := iss.Sign(t, jose.RS256, valid)
	refused := []struct {
		name     string
		pr       presentation
		httpOnly bool
		message  string
		reason   string
	}{
		{name: "no Authorization header", pr: presentation{}, message: "no token", reason: "no_token"},
		{name: "token in a cookie", pr: presentation{cookie: "access_token=" + keep(validToken)}, message: "no token", reason: "no_token"},
		{name: "token in a query parameter", pr: presentation{query: keep(validToken)}, httpOnly: true, message: "no token", reason: "no_token"},
		{name: "Basic scheme", pr: presentation{authorization: "Basic dXNlcjpwYXNz"}, message: "no token", reason: "no_token"},
		{name: "Bearer with no token", pr: presentation{authorization: "Bearer "}, message: "no token", reason: "no_token"},
		{name: "opaque access token", pr: bearer(keep("2YotnFZFEjr1zCsicMWpAA")), message: "invalid token: malformed", reason: "invalid_token"},
		{name: "alg none", pr: bearer(keep(iss.Unsigned(t, valid))), message: "invalid token: unsupported alg", reason: "invalid_token"},
		{name: "HS256 keyed by the RSA public key", pr: bearer(keep(iss.SignHMACConfused(t, valid))), message: "invalid token: unsupported alg", reason: "invalid_token"},
		{name: "PS256", pr: bearer(keep(iss.Sign(t, jose.PS256, valid))), message: "invalid token: unsupported alg", reason: "invalid_token"},
		{name: "kid never published", pr: bearer(keep(iss.SignUnpublished(t, valid))), message: "invalid token: unknown kid", reason: "invalid_token"},
		{name: "signature does not verify", pr: bearer(keep(harness.Tamper(t, validToken, with(valid, harness.Claims{"sub": "mallory"})))), message: "invalid token: bad signature", reason: "invalid_token"},
		{name: "wrong iss", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"iss": iss.URL() + "/"})))), message: "invalid token: wrong iss", reason: "invalid_token"},
		{name: "wrong aud", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"aud": "someone-else"})))), message: "invalid token: wrong aud", reason: "invalid_token"},
		{name: "aud array without the audience", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"aud": []string{"a", "b"}})))), message: "invalid token: wrong aud", reason: "invalid_token"},
		{name: "no aud", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "aud")))), message: "invalid token: wrong aud", reason: "invalid_token"},
		{name: "no exp", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "exp")))), message: "invalid token: missing exp", reason: "invalid_token"},
		{name: "expired", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"exp": now.Add(-2 * time.Minute).Unix()})))), message: "invalid token: expired", reason: "invalid_token"},
		{name: "nbf ahead", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"nbf": now.Add(2 * time.Minute).Unix()})))), message: "invalid token: not yet valid", reason: "invalid_token"},
		{name: "iat ahead", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"iat": now.Add(2 * time.Minute).Unix()})))), message: "invalid token: issued in the future", reason: "invalid_token"},
		{name: "exp not a number", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"exp": "tomorrow"})))), message: "invalid token: malformed", reason: "invalid_token"},
		{name: "no scope", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "scope")))), message: "missing scope: telemetry:write", reason: "missing_scope"},
		{name: "scope without telemetry:write", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"scope": "openid profile telemetry:read"})))), message: "missing scope: telemetry:write", reason: "missing_scope"},
		{name: "an ID token", pr: bearer(keep(iss.Sign(t, jose.RS256, iss.IDTokenClaims()))), message: "missing scope: telemetry:write", reason: "missing_scope"},
		{name: "no sub", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "sub")))), message: "missing claim: sub", reason: "missing_claim"},
		{name: "empty sub", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"sub": ""})))), message: "missing claim: sub", reason: "missing_claim"},
		{name: "no preferred_username", pr: bearer(keep(iss.Sign(t, jose.RS256, without(valid, "preferred_username")))), message: "missing claim: preferred_username", reason: "missing_claim"},
		{name: "empty preferred_username", pr: bearer(keep(iss.Sign(t, jose.RS256, with(valid, harness.Claims{"preferred_username": ""})))), message: "missing claim: preferred_username", reason: "missing_claim"},
	}
	var refusedMarkers []string

	// One at a time: each reads the counters around its own request.
	t.Run("refused", func(t *testing.T) {
		for _, tc := range refused {
			for _, p := range protocols {
				if tc.httpOnly && p == protocolGRPC {
					continue
				}
				t.Run(tc.name+"/"+p.String(), func(t *testing.T) {
					marker := "auth/refused/" + tc.name + "/" + p.String()
					refusedMarkers = append(refusedMarkers, marker)
					before := scrape(t, env.collector)
					got := env.clients.present(t, p, tc.pr, marker)
					after := scrape(t, env.collector)

					assert.Equal(t, codes.Unauthenticated, got.code, got.message)
					if p == protocolHTTP {
						assert.Equal(t, http.StatusUnauthorized, got.httpStatus)
					}
					assert.Equal(t, tc.message, got.message, "the refusal names the rule")

					reason := map[string]string{"reason": tc.reason}
					assert.InDelta(t, 1, after.Sum(rejectionsMetric, reason)-before.Sum(rejectionsMetric, reason), 0, "%s%v", rejectionsMetric, reason)
					assert.InDelta(t, 1, after.Sum(rejectionsMetric, nil)-before.Sum(rejectionsMetric, nil), 0, "one request, one rejection")
					assert.InDelta(t, 0, after.Sum(refusedMetric, nil)-before.Sum(refusedMetric, nil), 0,
						"an authenticator refusal is not the receiver's refusal")
				})
			}
		}
	})

	t.Run("only the documented reasons are counted", func(t *testing.T) {
		for _, smp := range scrape(t, env.collector) {
			if smp.Name == rejectionsMetric {
				assert.Contains(t, rejectionReasons, smp.Labels["reason"], "%v", smp.Labels)
			}
		}
	})

	t.Run("nothing refused reaches the sink", func(t *testing.T) {
		for _, p := range protocols {
			marker := "auth/after-refusals/" + p.String()
			require.Equal(t, codes.OK, env.clients.present(t, p, bearer(validToken), marker).code)
			require.Eventually(t, func() bool {
				_, ok := harness.FindTraces(env.sink.Received().Traces, marker)
				return ok
			}, eventually, 10*time.Millisecond, "%s never reached the sink", marker)
		}
		received := env.sink.Received().Traces
		for _, marker := range refusedMarkers {
			_, ok := harness.FindTraces(received, marker)
			assert.False(t, ok, "%s reached the sink", marker)
		}
	})

	t.Run("one warning per reason, never the token", func(t *testing.T) {
		for range 20 {
			got := env.clients.present(t, protocolHTTP, presentation{}, "auth/hammer")
			require.Equal(t, "no token", got.message)
		}
		out := env.collector.Output()
		for _, reason := range []string{"no_token", "invalid_token", "missing_scope", "missing_claim"} {
			warnings := regexp.MustCompile(`(?m)^.*\bwarn\b.*Refused a request.*"reason": "`+reason+`".*$`).FindAllString(out, -1)
			assert.Len(t, warnings, 1, "warnings for %s:\n%s", reason, strings.Join(warnings, "\n"))
		}
		for _, token := range tokens {
			assert.NotContains(t, out, token, "a token was logged")
			if i := strings.LastIndexByte(token, '.'); i > 0 && i < len(token)-1 {
				assert.NotContains(t, out, token[i+1:], "a token's signature was logged")
			}
		}
	})
}

// TestKeys covers the key set's lifecycle: a rotated-in kid is found by one
// refresh, a stream of unknown kids cannot make every request a fetch, and a
// key the issuer removes stops being trusted within OIDC_JWKS_REFRESH.
func TestKeys(t *testing.T) {
	t.Run("rotation in, and unknown kids are throttled", func(t *testing.T) {
		env := startShipped(t, nil)
		iss := env.issuer
		fetches := iss.JWKSFetches()
		iss.Rotate(t)
		rotated := iss.Sign(t, jose.RS256, iss.Claims())
		for _, p := range protocols {
			got := env.clients.present(t, p, bearer(rotated), "keys/rotated/"+p.String())
			require.Equal(t, codes.OK, got.code, got.message)
		}
		assert.Equal(t, int64(1), iss.JWKSFetches()-fetches, "one refresh finds the new key; the second request uses the cache")

		fetches = iss.JWKSFetches()
		for i := range 10 {
			p := protocols[i%len(protocols)]
			got := env.clients.present(t, p, bearer(iss.SignUnpublished(t, iss.Claims())), "keys/unknown/"+p.String())
			require.Equal(t, "invalid token: unknown kid", got.message)
		}
		assert.Equal(t, int64(0), iss.JWKSFetches()-fetches, "unknown kids right after a refresh fetch nothing")
	})

	t.Run("rotation out", func(t *testing.T) {
		env := startShipped(t, map[string]string{"OIDC_JWKS_REFRESH": "200ms"})
		iss := env.issuer
		old := iss.Sign(t, jose.RS256, iss.Claims())
		require.Equal(t, codes.OK, env.clients.present(t, protocolHTTP, bearer(old), "keys/old").code)

		iss.Rotate(t)
		current := iss.Sign(t, jose.ES256, iss.Claims())
		iss.RetireOld()
		for _, p := range protocols {
			require.Eventually(t, func() bool {
				return env.clients.present(t, p, bearer(old), "keys/retired/"+p.String()).message == "invalid token: unknown kid"
			}, eventually, 50*time.Millisecond, "a retired key was still trusted over %s", p)
			got := env.clients.present(t, p, bearer(current), "keys/current/"+p.String())
			assert.Equal(t, codes.OK, got.code, got.message)
		}

		iss.Stop()
		require.Eventually(t, func() bool {
			return strings.Contains(env.collector.Output(), "JWKS refresh failed; keeping the cached keys")
		}, eventually, 50*time.Millisecond, "the scheduled refresh never met the stopped issuer")
		for _, p := range protocols {
			got := env.clients.present(t, p, bearer(current), "keys/issuer-gone/"+p.String())
			assert.Equal(t, codes.OK, got.code, "a failed refresh keeps the cache: %s", got.message)
		}
	})
}

// TestNotReady starts the collector with its issuer unreachable: it runs and
// is healthy, answers not ready, and recovers without a restart once the
// issuer appears.
func TestNotReady(t *testing.T) {
	cert := harness.NewCertificate(t)
	sink := harness.NewSink(t)
	iss := harness.NewIssuer(t)
	vars := oidcEnv(iss)
	maps.Copy(vars, map[string]string{
		"TLS_CERT_FILE":               cert.CertFile,
		"TLS_KEY_FILE":                cert.KeyFile,
		"OTEL_EXPORTER_OTLP_ENDPOINT": sink.Endpoint(),
		"BATCH_TIMEOUT":               "50ms",
		"OIDC_DISCOVERY_RETRY":        "100ms",
	})
	c := harness.Start(t, binary, harness.Options{Env: vars})
	cl := newClients(t, c.ListenAddr, cert.Pool, iss.Sign(t, jose.RS256, iss.Claims()))

	for _, p := range protocols {
		t.Run("not ready/"+p.String(), func(t *testing.T) {
			notReady := map[string]string{"reason": "not_ready"}
			before := scrape(t, c)
			got := cl.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("not-ready/"+p.String(), 1))
			after := scrape(t, c)

			assert.Equal(t, codes.Unavailable, got.code, got.message)
			assert.Equal(t, "not ready", got.message)
			assert.Equal(t, 5*time.Second, got.retryDelay, "the client is told when to retry")
			if p == protocolHTTP {
				assert.Equal(t, http.StatusServiceUnavailable, got.httpStatus)
				assert.Equal(t, "5", got.retryAfter)
			}
			assert.InDelta(t, 1, after.Sum(rejectionsMetric, notReady)-before.Sum(rejectionsMetric, notReady), 0, "%s%v", rejectionsMetric, notReady)
		})
		t.Run("no token is still no token/"+p.String(), func(t *testing.T) {
			got := cl.present(t, p, presentation{}, "not-ready/no-token/"+p.String())
			assert.Equal(t, codes.Unauthenticated, got.code, got.message)
			assert.Equal(t, "no token", got.message)
		})
	}

	assert.True(t, c.Healthy(t.Context()), "an unreachable issuer does not make the process unhealthy")
	assert.Contains(t, c.Output(), "OIDC discovery failed")

	iss.Start(t)
	for _, p := range protocols {
		marker := "not-ready/recovered/" + p.String()
		require.Eventually(t, func() bool {
			return cl.export(t.Context(), t, p, harness.SignalTraces, harness.Traces(marker, 1)).code == codes.OK
		}, eventually, 50*time.Millisecond, "requests over %s never succeeded after the issuer appeared", p)
		require.Eventually(t, func() bool {
			_, ok := harness.FindTraces(sink.Received().Traces, marker)
			return ok
		}, eventually, 10*time.Millisecond, "%s never reached the sink", marker)
	}

	t.Run("keys stay cached when the issuer goes away", func(t *testing.T) {
		iss.Stop()
		for _, p := range protocols {
			got := cl.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("not-ready/issuer-gone/"+p.String(), 1))
			assert.Equal(t, codes.OK, got.code, got.message)
		}
	})
}
