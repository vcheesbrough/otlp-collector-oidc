package integration

import (
	"net/http"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The behind-Traefik tier (DESIGN §9): a real Traefik container in front of
// the collector, configured as docs/proxies/traefik.md says, with the
// scenario tables run through it unchanged. Without a mount prefix one
// router takes both protocols; under /otlp the HTTP router strips the prefix
// and the gRPC router, whose method paths are the client's, does not.

// via is env with clients that reach its collector through f.
func (env shipped) via(t *testing.T, f harness.Frontend) shipped {
	t.Helper()
	// pool stays the collector's: it is for clients built to its listener.
	env.clients = newClientsVia(t, f, env.issuer.Sign(t, jose.RS256, env.issuer.Claims()))
	return env
}

func TestBehindTraefik(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"", "/otlp"} {
		name := "unprefixed"
		if prefix != "" {
			name = "mounted at " + prefix
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			direct := startShipped(t, authEnv())
			env := direct.via(t, harness.StartTraefik(t, direct.collector.ListenAddr, prefix))

			t.Run("no bearer gets the collector's refusal", func(t *testing.T) {
				for _, p := range protocols {
					got := env.clients.present(t, p, presentation{}, "traefik/no-token"+prefix+"/"+p.String())
					assert.Equal(t, codes.Unauthenticated, got.code, "%s: %s", p, got.message)
					assert.Equal(t, "no token", got.message, p.String())
				}
			})
			t.Run("only the OTLP paths are routed", func(t *testing.T) {
				// The collector counts every unknown path it answers; the
				// proxy's own 404 leaves the count where it was.
				unknown := map[string]string{"transport": "http", "reason": "unknown_path"}
				before := scrape(t, env.collector).Sum("otelcol_otlpsingleport_requests_refused", unknown)
				resp, err := env.clients.http.Do(t.Context(), http.MethodPost, "/elsewhere", nil, nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusNotFound, resp.Status, "%s", resp.Body)
				assert.InDelta(t, before, scrape(t, env.collector).Sum("otelcol_otlpsingleport_requests_refused", unknown), 0, "the collector answered an unrouted path")
			})
			t.Run("fidelity", fidelityScenarios(env))
			t.Run("token profile", func(t *testing.T) { tokenProfile(t, env) })
		})
	}
}
