package integration

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// directConfig wires the receiver straight to the exporter, with no batch,
// queue or retry between them, so the upstream's answer reaches the client.
// The shipped pipeline decouples the two by design; this shape exists to
// observe the receiver's consumer-error mapping from outside, and to drive
// its metrics path, which the shipped pipeline does not wire yet.
const directConfig = `
receivers:
  otlpsingleport:
    endpoint: ${env:LISTEN_ADDR}
    tls:
      cert_file: ${env:TLS_CERT_FILE}
      key_file: ${env:TLS_KEY_FILE}
exporters:
  otlp_grpc:
    endpoint: ${env:OTEL_EXPORTER_OTLP_ENDPOINT}
    tls:
      insecure: true
    timeout: 5s
    sending_queue:
      enabled: false
    retry_on_failure:
      enabled: false
extensions:
  health_check:
    endpoint: ${env:HEALTH_ADDR}
service:
  extensions: [health_check]
  pipelines:
    traces:
      receivers: [otlpsingleport]
      exporters: [otlp_grpc]
    logs:
      receivers: [otlpsingleport]
      exporters: [otlp_grpc]
    metrics:
      receivers: [otlpsingleport]
      exporters: [otlp_grpc]
`

// TestDirect drives the upstream's answers through to the client. Scenarios
// run one at a time: the sink's behaviour is shared by all of them.
func TestDirect(t *testing.T) {
	cert := harness.NewCertificate(t)
	sink := harness.NewSink(t)
	c := harness.Start(t, binary, harness.Options{
		ConfigYAML: directConfig,
		Env: map[string]string{
			"TLS_CERT_FILE":               cert.CertFile,
			"TLS_KEY_FILE":                cert.KeyFile,
			"OTEL_EXPORTER_OTLP_ENDPOINT": sink.Endpoint(),
		},
	})
	cl := newClients(t, c.ListenAddr, cert.Pool)

	signals := []struct {
		signal   harness.Signal
		message  func(marker string) harness.Message
		accepted string
		refused  string
	}{
		{
			signal:   harness.SignalTraces,
			message:  func(m string) harness.Message { return harness.Traces(m, 2) },
			accepted: "otelcol_receiver_accepted_spans",
			refused:  "otelcol_receiver_refused_spans",
		},
		{
			signal:   harness.SignalLogs,
			message:  func(m string) harness.Message { return harness.Logs(m, 2) },
			accepted: "otelcol_receiver_accepted_log_records",
			refused:  "otelcol_receiver_refused_log_records",
		},
		{
			signal:   harness.SignalMetrics,
			message:  func(m string) harness.Message { return harness.Metrics(m, 2) },
			accepted: "otelcol_receiver_accepted_metric_points",
			refused:  "otelcol_receiver_refused_metric_points",
		},
	}

	cases := []struct {
		name           string
		behaviour      harness.Behaviour
		wantHTTP       int
		wantCode       codes.Code
		wantRetryHint  bool
		wantDelivered  bool
		wantAccepted   float64
		wantRefused    float64
		wantMessageHas string
	}{
		{name: "upstream accepts", behaviour: harness.BehaviourOK, wantHTTP: http.StatusOK, wantCode: codes.OK, wantDelivered: true, wantAccepted: 2},
		{name: "upstream fails permanently", behaviour: harness.BehaviourPermanent, wantHTTP: http.StatusBadRequest, wantCode: codes.InvalidArgument, wantRefused: 2, wantMessageHas: "sink told to fail permanently"},
		{name: "upstream fails retryably", behaviour: harness.BehaviourRetryable, wantHTTP: http.StatusServiceUnavailable, wantCode: codes.Unavailable, wantRetryHint: true, wantRefused: 2, wantMessageHas: "sink told to fail retryably"},
		{name: "upstream is down", behaviour: harness.BehaviourDown, wantHTTP: http.StatusServiceUnavailable, wantCode: codes.Unavailable, wantRefused: 2},
	}

	for _, tc := range cases {
		for _, s := range signals {
			for _, p := range protocols {
				t.Run(tc.name+"/"+s.signal.String()+"/"+p.String(), func(t *testing.T) {
					sink.SetBehaviour(t, tc.behaviour)
					t.Cleanup(func() { sink.SetBehaviour(t, harness.BehaviourOK) })
					labels := map[string]string{"receiver": "otlpsingleport", "transport": p.String()}
					before := scrape(t, c)

					marker := "direct/" + t.Name()
					got := cl.export(t.Context(), t, p, s.signal, s.message(marker))

					assert.Equal(t, tc.wantCode, got.code, got.message)
					if p == protocolHTTP {
						assert.Equal(t, tc.wantHTTP, got.httpStatus, got.message)
					}
					assert.Contains(t, got.message, tc.wantMessageHas)
					if tc.wantRetryHint {
						assert.Equal(t, harness.SinkRetryDelay, got.retryDelay, "the upstream's retry delay reaches the client")
						if p == protocolHTTP {
							assert.Equal(t, "7", got.retryAfter)
						}
					} else {
						assert.Empty(t, got.retryAfter)
					}

					after := scrape(t, c)
					assert.InDelta(t, tc.wantAccepted, after.Sum(s.accepted, labels)-before.Sum(s.accepted, labels), 0, s.accepted)
					assert.InDelta(t, tc.wantRefused, after.Sum(s.refused, labels)-before.Sum(s.refused, labels), 0, s.refused)

					r := sink.Received()
					var delivered bool
					switch s.signal {
					case harness.SignalTraces:
						_, delivered = harness.FindTraces(r.Traces, marker)
					case harness.SignalLogs:
						_, delivered = harness.FindLogs(r.Logs, marker)
					case harness.SignalMetrics:
						_, delivered = harness.FindMetrics(r.Metrics, marker)
					}
					require.Equal(t, tc.wantDelivered, delivered, "delivered to the sink")
				})
			}
		}
	}
}
