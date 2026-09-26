package integration

import (
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// Client metrics (DESIGN §5, "Metrics"): allowlisted by name and datapoint
// key, delta converted to cumulative under a stream cap, and never carrying
// identity. The resource is reduced to service.name and service.version, so
// each scenario marks its payload by a service.name of its own.

type metricKind int

const (
	kindDeltaSum metricKind = iota
	kindCumulativeSum
	kindDeltaHistogram
	kindDeltaExpHistogram
)

// metricSpec is one metric with one datapoint.
type metricSpec struct {
	name  string
	kind  metricKind
	value int64  // a sum's value
	count uint64 // a histogram's count, all in its first bucket
	attrs map[string]any
	start time.Time
	at    time.Time
}

// metricsRequest is one resource, named service, carrying specs.
func metricsRequest(service string, resource map[string]any, specs ...metricSpec) pmetricotlp.ExportRequest {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", service)
	rm.Resource().Attributes().PutStr("service.version", "1.2.3")
	_ = rm.Resource().Attributes().FromRaw(merge(rm.Resource().Attributes().AsRaw(), resource))
	sm := rm.ScopeMetrics().AppendEmpty()
	for _, s := range specs {
		m := sm.Metrics().AppendEmpty()
		m.SetName(s.name)
		start, at := pcommon.NewTimestampFromTime(s.start), pcommon.NewTimestampFromTime(s.at)
		switch s.kind {
		case kindDeltaSum, kindCumulativeSum:
			sum := m.SetEmptySum()
			sum.SetIsMonotonic(true)
			sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			if s.kind == kindCumulativeSum {
				sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			}
			dp := sum.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(at)
			dp.SetIntValue(s.value)
			_ = dp.Attributes().FromRaw(s.attrs)
		case kindDeltaHistogram:
			h := m.SetEmptyHistogram()
			h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			dp := h.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(at)
			dp.SetCount(s.count)
			dp.SetSum(float64(s.count))
			dp.ExplicitBounds().FromRaw([]float64{1, 10})
			dp.BucketCounts().FromRaw([]uint64{s.count, 0, 0})
			_ = dp.Attributes().FromRaw(s.attrs)
		case kindDeltaExpHistogram:
			h := m.SetEmptyExponentialHistogram()
			h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
			dp := h.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(at)
			dp.SetCount(s.count)
			dp.SetSum(float64(s.count))
			dp.SetScale(0)
			dp.Positive().BucketCounts().FromRaw([]uint64{s.count})
			_ = dp.Attributes().FromRaw(s.attrs)
		}
	}
	return pmetricotlp.NewExportRequestFromMetrics(md)
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// receivedMetric is one arrival of a metric under a service.
type receivedMetric struct {
	resource map[string]any
	metric   pmetric.Metric
}

// metricArrivals is every arrival of name from service, in order.
func metricArrivals(sink *harness.Sink, service, name string) []receivedMetric {
	var out []receivedMetric
	for _, md := range sink.Received().Metrics {
		for _, rm := range md.ResourceMetrics().All() {
			if v, ok := rm.Resource().Attributes().Get("service.name"); !ok || v.Str() != service {
				continue
			}
			for _, sm := range rm.ScopeMetrics().All() {
				for _, m := range sm.Metrics().All() {
					if m.Name() == name {
						out = append(out, receivedMetric{resource: rm.Resource().Attributes().AsRaw(), metric: m})
					}
				}
			}
		}
	}
	return out
}

func awaitMetric(t *testing.T, sink *harness.Sink, service, name string, n int) []receivedMetric {
	t.Helper()
	var got []receivedMetric
	require.Eventually(t, func() bool {
		got = metricArrivals(sink, service, name)
		return len(got) >= n
	}, eventually, 10*time.Millisecond, "%s from %s never arrived %d times", name, service, n)
	return got
}

func sendMetrics(t *testing.T, cl clients, p protocol, req pmetricotlp.ExportRequest) {
	t.Helper()
	got := cl.export(t.Context(), t, p, harness.SignalMetrics, req)
	require.Equal(t, codes.OK, got.code, got.message)
}

// metricsEnv allows three names and one key, and names the deployment. The
// allowlist also names identity keys, which must be stripped anyway.
func metricsEnv() map[string]string {
	return map[string]string{
		"ALLOWED_METRIC_NAMES":          `app\.(requests|latency|size)`,
		"ALLOWED_METRIC_ATTRIBUTE_KEYS": "route,user.id,user.name,session.id",
		"CLIENT_RESOURCE_ATTRIBUTES":    "deployment.environment.name=it,telemetry_source=client",
	}
}

// forgedIdentity is what a client claims about who it is, everywhere.
func forgedIdentity() map[string]any {
	return map[string]any{"user.id": "forged", "user.name": "forged", "session.id": "s-1", "enduser.id": "e-1"}
}

// TestMetrics accepts allowlisted client metrics over both protocols, routed
// by a METRICS override to their own sink, and strips everything else.
func TestMetrics(t *testing.T) {
	t.Parallel()
	traces := harness.NewSink(t)
	metrics := harness.NewSinkWith(t, harness.SinkOptions{Transport: harness.TransportHTTP})
	env := metricsEnv()
	env["OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"] = "http/protobuf"
	env["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] = metrics.Endpoint() + "/v1/metrics"
	shape := startShippedWith(t, traces, env, nil)
	filtered := func() float64 {
		return scrape(t, shape.collector).Sum("otelcol_processor_filter_datapoints_filtered", nil)
	}

	for _, p := range protocols {
		t.Run("identity and allowlists/"+p.String(), func(t *testing.T) {
			svc := "metrics-allow-" + p.String()
			t0 := time.Now().Add(-time.Minute)
			attrs := merge(forgedIdentity(), map[string]any{"route": "/checkout", "secret": "k"})
			resource := merge(forgedIdentity(), map[string]any{"telemetry_source": "forged", "deployment.environment.name": "forged", "host.name": "phone"})
			before := filtered()
			sendMetrics(t, shape.clients, p, metricsRequest(svc, resource,
				metricSpec{name: "app.requests", kind: kindDeltaSum, value: 3, attrs: attrs, start: t0, at: t0.Add(10 * time.Second)},
				metricSpec{name: "app.unregistered", kind: kindDeltaSum, value: 1, start: t0, at: t0.Add(10 * time.Second)},
			))
			got := awaitMetric(t, metrics, svc, "app.requests", 1)[0]
			assert.Equal(t, map[string]any{
				"service.name": svc, "service.version": "1.2.3",
				"deployment.environment.name": "it", "telemetry_source": "client",
			}, got.resource, "the resource is the client build and the deployment, nothing else")
			dp := got.metric.Sum().DataPoints().At(0)
			assert.Equal(t, map[string]any{"route": "/checkout"}, dp.Attributes().AsRaw(), "only allowlisted, non-identity keys survive")
			assert.Empty(t, metricArrivals(metrics, svc, "app.unregistered"), "an unregistered metric reached the sink")
			assert.Eventually(t, func() bool { return filtered()-before == 1 }, eventually, 50*time.Millisecond, "the name drop was not counted")
			assert.Empty(t, traces.Received().Metrics, "metrics followed the base upstream, not the METRICS override")
		})

		t.Run("delta to cumulative/"+p.String(), func(t *testing.T) {
			svc := "metrics-delta-" + p.String()
			t0 := time.Now().Add(-time.Minute)
			attrs := map[string]any{"route": "/a"}
			sendMetrics(t, shape.clients, p, metricsRequest(svc, nil,
				metricSpec{name: "app.requests", kind: kindDeltaSum, value: 3, attrs: attrs, start: t0, at: t0.Add(10 * time.Second)},
				metricSpec{name: "app.latency", kind: kindDeltaHistogram, count: 2, attrs: attrs, start: t0, at: t0.Add(10 * time.Second)},
				metricSpec{name: "app.size", kind: kindDeltaExpHistogram, count: 5, attrs: attrs, start: t0, at: t0.Add(10 * time.Second)},
			))
			awaitMetric(t, metrics, svc, "app.requests", 1)
			sendMetrics(t, shape.clients, p, metricsRequest(svc, nil,
				metricSpec{name: "app.requests", kind: kindDeltaSum, value: 4, attrs: attrs, start: t0.Add(10 * time.Second), at: t0.Add(20 * time.Second)},
			))
			sums := awaitMetric(t, metrics, svc, "app.requests", 2)
			last := sums[len(sums)-1].metric.Sum()
			assert.Equal(t, pmetric.AggregationTemporalityCumulative, last.AggregationTemporality())
			assert.Equal(t, int64(7), last.DataPoints().At(0).IntValue(), "3 then 4 accumulates to 7")

			h := awaitMetric(t, metrics, svc, "app.latency", 1)[0].metric.Histogram()
			assert.Equal(t, pmetric.AggregationTemporalityCumulative, h.AggregationTemporality(), "a delta histogram arrives cumulative")
			e := awaitMetric(t, metrics, svc, "app.size", 1)[0].metric.ExponentialHistogram()
			assert.Equal(t, pmetric.AggregationTemporalityCumulative, e.AggregationTemporality(), "a delta exponential histogram arrives cumulative")
		})

		t.Run("cumulative passes through/"+p.String(), func(t *testing.T) {
			svc := "metrics-cumulative-" + p.String()
			t0 := time.Now().Add(-time.Minute)
			sendMetrics(t, shape.clients, p, metricsRequest(svc, nil,
				metricSpec{name: "app.requests", kind: kindCumulativeSum, value: 42, start: t0, at: t0.Add(10 * time.Second)},
			))
			sum := awaitMetric(t, metrics, svc, "app.requests", 1)[0].metric.Sum()
			assert.Equal(t, pmetric.AggregationTemporalityCumulative, sum.AggregationTemporality())
			assert.Equal(t, int64(42), sum.DataPoints().At(0).IntValue())
		})
	}

	t.Run("no identifier is a label on :8888", func(t *testing.T) {
		// The identifier-cardinality test: listed by key, so a new metric is
		// covered without editing it. service_instance_id on target_info is
		// the process's, not an identifier of anyone.
		forbidden := regexp.MustCompile(`^(user|session|enduser)_|^(sub|sid|jti|token|authorization|bearer)$|_token$`)
		var bad []string
		for _, s := range scrape(t, shape.collector) {
			for k := range s.Labels {
				if forbidden.MatchString(k) {
					bad = append(bad, s.Name+"{"+k+"}")
				}
			}
		}
		sort.Strings(bad)
		assert.Empty(t, bad, "identifier-shaped label keys on :8888")
	})
}

// TestMetricsEmptyAllowlists drops every client metric when no name is
// allowed, while the client still gets 200.
func TestMetricsEmptyAllowlists(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, nil, nil)
	for _, p := range protocols {
		before := scrape(t, env.collector).Sum("otelcol_processor_filter_datapoints_filtered", nil)
		sendMetrics(t, env.clients, p, metricsRequest("metrics-empty-"+p.String(), nil,
			metricSpec{name: "app.requests", kind: kindDeltaSum, value: 1, attrs: map[string]any{"route": "/a"}, start: time.Now().Add(-time.Second), at: time.Now()},
		))
		assert.Eventually(t, func() bool {
			return scrape(t, env.collector).Sum("otelcol_processor_filter_datapoints_filtered", nil)-before == 1
		}, eventually, 50*time.Millisecond, "the drop was not counted")
		assert.Empty(t, metricArrivals(sink, "metrics-empty-"+p.String(), "app.requests"))
	}
}

// TestMetricsStreamCap admits MAX_METRIC_STREAMS streams, drops and counts
// the next, and admits a new one once a stream is forgotten after
// DELTA_MAX_STALE.
func TestMetricsStreamCap(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := metricsEnv()
	env["MAX_METRIC_STREAMS"] = "3"
	env["DELTA_MAX_STALE"] = "2s"
	shape := startShippedWith(t, sink, env, nil)
	svc := "metrics-cap"
	stream := func(route string) metricSpec {
		return metricSpec{name: "app.requests", kind: kindDeltaSum, value: 1, attrs: map[string]any{"route": route}, start: time.Now().Add(-time.Second), at: time.Now()}
	}
	routes := func() map[string]bool {
		out := map[string]bool{}
		for _, r := range metricArrivals(sink, svc, "app.requests") {
			dps := r.metric.Sum().DataPoints()
			for i := range dps.Len() {
				v, _ := dps.At(i).Attributes().Get("route")
				out[v.Str()] = true
			}
		}
		return out
	}

	for i := range 3 {
		sendMetrics(t, shape.clients, protocolHTTP, metricsRequest(svc, nil, stream("/"+strconv.Itoa(i))))
	}
	require.Eventually(t, func() bool { return len(routes()) == 3 }, eventually, 10*time.Millisecond, "streams 1..3 arrived: %v", routes())

	// The fourth, with a witness in the same request: the witness is an
	// existing stream, so it arrives, and the fourth does not.
	sendMetrics(t, shape.clients, protocolGRPC, metricsRequest(svc, nil, stream("/0"), stream("/beyond")))
	require.Eventually(t, func() bool { return len(metricArrivals(sink, svc, "app.requests")) >= 4 }, eventually, 10*time.Millisecond)
	assert.False(t, routes()["/beyond"], "stream 4 arrived past the cap")
	assert.Eventually(t, func() bool {
		return scrape(t, shape.collector).Sum("otelcol_deltatocumulative_datapoints", map[string]string{"error": "limit"}) >= 1
	}, eventually, 50*time.Millisecond, "the dropped stream was not counted")

	// Once the streams are stale, a new stream fits. deltatocumulative
	// sweeps stale streams once a minute (v0.161.0), so this takes up to
	// DELTA_MAX_STALE plus a minute; keep offering the stream until then.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for deadline := time.Now().Add(90 * time.Second); !routes()["/later"]; <-tick.C {
		require.True(t, time.Now().Before(deadline), "a new stream never fitted after DELTA_MAX_STALE")
		sendMetrics(t, shape.clients, protocolHTTP, metricsRequest(svc, nil, stream("/later")))
	}
}
