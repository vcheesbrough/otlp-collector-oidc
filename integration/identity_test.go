package integration

import (
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// Identity from the token and the deployment's attributes are stamped on
// every span and log record (CLAIM_ATTRIBUTES, CLIENT_RESOURCE_ATTRIBUTES),
// overwriting what the client claimed about itself.

// forged is a client's own claims about who and where it is.
func forged(resource, item pcommon.Map) {
	resource.PutStr("service.name", "client-app")
	resource.PutStr("service.version", "1.2.3")
	resource.PutStr("deployment.environment.name", "forged")
	resource.PutStr("telemetry_source", "forged")
	resource.PutStr("client.kept", "yes")
	item.PutStr("user.id", "forged-id")
	item.PutStr("user.email", "forged@example.com")
	item.PutStr("app.kept", "yes")
}

func forgedTraces(marker string) ptraceotlp.ExportRequest {
	req := harness.Traces(marker, 2)
	rs := req.Traces().ResourceSpans().At(0)
	for _, span := range rs.ScopeSpans().At(0).Spans().All() {
		forged(rs.Resource().Attributes(), span.Attributes())
	}
	return req
}

func forgedLogs(marker string) plogotlp.ExportRequest {
	req := harness.Logs(marker, 2)
	rl := req.Logs().ResourceLogs().At(0)
	for _, lr := range rl.ScopeLogs().At(0).LogRecords().All() {
		forged(rl.Resource().Attributes(), lr.Attributes())
	}
	return req
}

// stamped is one received resource and the attributes of each span or
// record under it.
type stamped struct {
	resource map[string]any
	items    []map[string]any
}

// awaitStamped waits for marker's traces and logs at sink and returns both.
func awaitStamped(t *testing.T, sink *harness.Sink, marker string) []stamped {
	t.Helper()
	var out []stamped
	require.Eventually(t, func() bool {
		r := sink.Received()
		td, okT := harness.FindTraces(r.Traces, marker)
		ld, okL := harness.FindLogs(r.Logs, marker)
		if !okT || !okL {
			return false
		}
		out = nil
		rs := td.ResourceSpans().At(0)
		s := stamped{resource: rs.Resource().Attributes().AsRaw()}
		for _, span := range rs.ScopeSpans().At(0).Spans().All() {
			s.items = append(s.items, span.Attributes().AsRaw())
		}
		out = append(out, s)
		rl := ld.ResourceLogs().At(0)
		s = stamped{resource: rl.Resource().Attributes().AsRaw()}
		for _, lr := range rl.ScopeLogs().At(0).LogRecords().All() {
			s.items = append(s.items, lr.Attributes().AsRaw())
		}
		out = append(out, s)
		return true
	}, eventually, 10*time.Millisecond, "%s never reached the sink as traces and logs", marker)
	return out
}

// sendForged exports forged traces and logs marked marker over p.
func sendForged(t *testing.T, cl clients, p protocol, marker string) {
	t.Helper()
	got := cl.export(t.Context(), t, p, harness.SignalTraces, forgedTraces(marker))
	require.Equal(t, codes.OK, got.code, got.message)
	got = cl.export(t.Context(), t, p, harness.SignalLogs, forgedLogs(marker))
	require.Equal(t, codes.OK, got.code, got.message)
}

// TestIdentity stamps the token's identity and the deployment's attributes
// over a forging client's, over both protocols. A token with every mapped
// claim gets all four attributes; one without email and name gets neither,
// not the client's forged email.
func TestIdentity(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{
		"CLIENT_RESOURCE_ATTRIBUTES": "deployment.environment.name=it,telemetry_source=client",
	}, nil)
	lean := newClients(t, env.collector.ListenAddr, env.pool, env.issuer.Sign(t, jose.RS256, without(env.issuer.Claims(), "email", "name")))

	for _, p := range protocols {
		t.Run("full token/"+p.String(), func(t *testing.T) {
			marker := "identity/full/" + p.String()
			sendForged(t, env.clients, p, marker)
			for _, got := range awaitStamped(t, sink, marker) {
				assert.Equal(t, map[string]any{
					"service.name":                "client-app",
					"service.version":             "1.2.3",
					"deployment.environment.name": "it",
					"telemetry_source":            "client",
					"client.kept":                 "yes",
					harness.MarkerKey:             marker,
				}, got.resource, "the deployment's attributes overwrite the client's; the rest is the client's")
				for _, item := range got.items {
					assert.Equal(t, "user-1", item["user.id"])
					assert.Equal(t, "alice", item["user.name"])
					assert.Equal(t, "alice@example.com", item["user.email"])
					assert.Equal(t, "Alice Example", item["user.full_name"])
					assert.Equal(t, "yes", item["app.kept"])
					for k := range item {
						assert.NotContains(t, k, "auth.", "a temporary copy survived: %v", item)
					}
				}
			}
		})
		t.Run("token without optional claims/"+p.String(), func(t *testing.T) {
			marker := "identity/lean/" + p.String()
			sendForged(t, lean, p, marker)
			for _, got := range awaitStamped(t, sink, marker) {
				for _, item := range got.items {
					assert.Equal(t, "user-1", item["user.id"])
					assert.Equal(t, "alice", item["user.name"])
					assert.NotContains(t, item, "user.email", "absent, not the client's forged value")
					assert.NotContains(t, item, "user.full_name")
				}
			}
		})
	}
}

// TestIdentityCustomClaims maps sub alone to a renamed attribute, and leaves
// the client's resource exactly as sent with CLIENT_RESOURCE_ATTRIBUTES
// unset.
func TestIdentityCustomClaims(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{"CLAIM_ATTRIBUTES": "sub=enduser.id"}, nil)
	for _, p := range protocols {
		marker := "identity/custom/" + p.String()
		for _, sig := range []harness.Signal{harness.SignalTraces, harness.SignalLogs} {
			msg := harness.Message(harness.Traces(marker, 1))
			if sig == harness.SignalLogs {
				msg = harness.Logs(marker, 1)
			}
			got := env.clients.export(t.Context(), t, p, sig, msg)
			require.Equal(t, codes.OK, got.code, got.message)
		}
		for _, got := range awaitStamped(t, sink, marker) {
			assert.Equal(t, map[string]any{"service.name": "integration", harness.MarkerKey: marker}, got.resource, "the client's resource, as sent")
			for _, item := range got.items {
				assert.Equal(t, "user-1", item["enduser.id"])
				for _, k := range []string{"user.id", "user.name", "user.email", "user.full_name"} {
					assert.NotContains(t, item, k, "a default mapping applied")
				}
			}
		}
	}
}

// TestIdentityBatched sends two users' spans within one batch window: each
// keeps its own identity, since it is materialised before batch.
func TestIdentityBatched(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{"BATCH_TIMEOUT": "2s"}, nil)
	bob := newClients(t, env.collector.ListenAddr, env.pool, env.issuer.Sign(t, jose.RS256, with(env.issuer.Claims(), harness.Claims{"sub": "user-2", "preferred_username": "bob"})))

	for _, p := range protocols {
		for name, cl := range map[string]clients{"alice": env.clients, "bob": bob} {
			got := cl.export(t.Context(), t, p, harness.SignalTraces, harness.Traces("identity/batched/"+name+"/"+p.String(), 1))
			require.Equal(t, codes.OK, got.code, got.message)
		}
	}
	for _, p := range protocols {
		for name, sub := range map[string]string{"alice": "user-1", "bob": "user-2"} {
			marker := "identity/batched/" + name + "/" + p.String()
			var td harness.Received
			require.Eventually(t, func() bool {
				td = sink.Received()
				_, ok := harness.FindTraces(td.Traces, marker)
				return ok
			}, eventually, 10*time.Millisecond, "%s never arrived", marker)
			found, _ := harness.FindTraces(td.Traces, marker)
			span := found.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
			id, _ := span.Attributes().Get("user.id")
			user, _ := span.Attributes().Get("user.name")
			assert.Equal(t, sub, id.Str(), marker)
			assert.Equal(t, name, user.Str(), marker)
		}
	}
}
