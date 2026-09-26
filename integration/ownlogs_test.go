package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The process's own logs go where client logs go, as OTLP, identified as
// itself (LOG_OUTPUT, OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES).

const (
	ownService    = "otlp-collector-oidc"
	refusedLine   = "Refused a request"
	renderedLine  = "Rendered the configuration from the environment"
	ownLogsSecret = "not-a-jwt-7c1d" // #nosec G101 -- made up, to prove it is never exported
)

// awaitOwnLog waits until sink holds a record of message from service.
func awaitOwnLog(t *testing.T, sink *harness.Sink, service, message string) harness.OwnLog {
	t.Helper()
	var found []harness.OwnLog
	require.Eventually(t, func() bool {
		found = harness.FindOwnLogs(sink.Received().Logs, service, message)
		return len(found) > 0
	}, eventually, 20*time.Millisecond, "%q from %s never reached the logs upstream", message, service)
	return found[0]
}

// refuse sends one request with a malformed bearer over each protocol, so
// the authenticator logs its rate-limited warning.
func refuse(t *testing.T, env shipped) {
	t.Helper()
	for _, p := range protocols {
		got := env.clients.present(t, p, bearer(ownLogsSecret), "own-logs/"+p.String())
		require.Equal(t, codes.Unauthenticated, got.code, got.message)
	}
}

// TestOwnLogs is the default, LOG_OUTPUT=both: the run command's startup
// line and the authenticator's refusal warning reach the logs upstream as
// OTLP, from the collector's own service, with the build's version, the
// deployer's resource attributes and the same instance id as target_info on
// :8888; stdout keeps its copy; the refused token is exported nowhere.
func TestOwnLogs(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=it%20test,service.version=9.9.9",
	}, nil)
	refuse(t, env)

	startup := awaitOwnLog(t, sink, ownService, renderedLine)
	refusal := awaitOwnLog(t, sink, ownService, refusedLine)
	reason, ok := refusal.Record.Attributes().Get("reason")
	require.True(t, ok, "the refusal carries its reason: %v", refusal.Record.Attributes().AsRaw())
	assert.Equal(t, "invalid_token", reason.Str())

	metrics := scrape(t, env.collector)
	info, ok := metrics.Find("target_info")
	require.True(t, ok, "target_info on :8888")
	for _, own := range []harness.OwnLog{startup, refusal} {
		attrs := own.Resource.Attributes().AsRaw()
		assert.Equal(t, binary.Version, attrs["service.version"], "the build's version, not OTEL_RESOURCE_ATTRIBUTES'")
		assert.Equal(t, "it test", attrs["deployment.environment.name"])
		assert.Equal(t, info.Labels["service_instance_id"], attrs["service.instance.id"], "one instance id on logs and metrics")
	}
	assert.Equal(t, binary.Version, info.Labels["service_version"])
	assert.Equal(t, "it test", info.Labels["deployment_environment_name"])
	assert.Equal(t, ownService, info.Labels["service_name"])

	for _, ld := range sink.Received().Logs {
		for _, rl := range ld.ResourceLogs().All() {
			assert.NotContains(t, fmt.Sprint(rl.Resource().Attributes().AsRaw()), ownLogsSecret)
			for _, sl := range rl.ScopeLogs().All() {
				for _, lr := range sl.LogRecords().All() {
					assert.NotContains(t, lr.Body().AsString()+fmt.Sprint(lr.Attributes().AsRaw()), ownLogsSecret, "a token was exported")
				}
			}
		}
	}
	out := env.collector.Stdout()
	assert.Contains(t, out, refusedLine, "stdout keeps a copy")
	assert.Contains(t, out, "Ignoring service.version in OTEL_RESOURCE_ATTRIBUTES")
	assert.NotContains(t, out, ownLogsSecret)
}

// TestOwnLogsOTLPOnly follows a LOGS override to an HTTP upstream, with its
// own service name, and writes nothing to stdout.
func TestOwnLogsOTLPOnly(t *testing.T) {
	t.Parallel()
	traces := harness.NewSink(t)
	logs := harness.NewSinkWith(t, harness.SinkOptions{Transport: harness.TransportHTTP})
	env := startShippedWith(t, traces, map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": logs.Endpoint() + "/otlp/v1/logs",
		"LOG_OUTPUT":                       "otlp",
		"OTEL_SERVICE_NAME":                "collector-it",
	}, nil)
	refuse(t, env)

	awaitOwnLog(t, logs, "collector-it", renderedLine)
	awaitOwnLog(t, logs, "collector-it", refusedLine)
	assert.Empty(t, traces.Received().Logs, "own logs reached the traces upstream")
	assert.Empty(t, strings.TrimSpace(env.collector.Stdout()), "LOG_OUTPUT=otlp writes nothing to stdout")
}

// TestOwnLogsStdoutOnly sends nothing of its own upstream, while client logs
// still go there.
func TestOwnLogsStdoutOnly(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{"LOG_OUTPUT": "stdout"}, nil)
	refuse(t, env)
	deliver(t, env, "own-logs-stdout", sink, sink)

	assert.Never(t, func() bool {
		return len(harness.FindOwnLogs(sink.Received().Logs, ownService, refusedLine)) > 0 ||
			len(harness.FindOwnLogs(sink.Received().Logs, ownService, renderedLine)) > 0
	}, 3*time.Second, 100*time.Millisecond, "LOG_OUTPUT=stdout sent own logs upstream")
	assert.Contains(t, env.collector.Stdout(), refusedLine)
}

// TestOwnLogsUpstreamDown starts with the logs upstream unreachable: the
// process starts, is healthy, answers clients and keeps its lines on stdout.
func TestOwnLogsUpstreamDown(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	sink.SetBehaviour(t, harness.BehaviourDown)
	env := startShippedWith(t, sink, nil, nil)
	refuse(t, env)

	assert.True(t, env.collector.Healthy(t.Context()))
	require.Eventually(t, func() bool {
		return strings.Contains(env.collector.Stdout(), refusedLine)
	}, eventually, 20*time.Millisecond, "the refusal never reached stdout")
	assert.Contains(t, env.collector.Stdout(), renderedLine)
}
