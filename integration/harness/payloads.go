package harness

import (
	"strconv"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// MarkerKey is the resource attribute that tells one scenario's payload from
// every other's at the sink.
const MarkerKey = "test.case"

// Traces is an export request of n spans under one resource carrying marker.
func Traces(marker string, n int) ptraceotlp.ExportRequest {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	fillResource(rs.Resource(), marker)
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("otlp-collector-oidc/integration")
	now := time.Now()
	for i := range n {
		span := ss.Spans().AppendEmpty()
		span.SetName("span-" + strconv.Itoa(i))
		span.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i + 1)})
		span.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, byte(i + 1)})
		span.SetKind(ptrace.SpanKindClient)
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Millisecond)))
		span.Attributes().PutStr("http.request.method", "GET")
		span.Attributes().PutInt("index", int64(i))
		span.Status().SetCode(ptrace.StatusCodeOk)
	}
	return ptraceotlp.NewExportRequestFromTraces(td)
}

// Logs is an export request of n log records under one resource carrying
// marker.
func Logs(marker string, n int) plogotlp.ExportRequest {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	fillResource(rl.Resource(), marker)
	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName("otlp-collector-oidc/integration")
	now := time.Now()
	for i := range n {
		lr := sl.LogRecords().AppendEmpty()
		lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
		lr.SetSeverityNumber(plog.SeverityNumberInfo)
		lr.SetSeverityText("INFO")
		lr.Body().SetStr("record " + strconv.Itoa(i))
		lr.Attributes().PutInt("index", int64(i))
	}
	return plogotlp.NewExportRequestFromLogs(ld)
}

// Metrics is an export request of one gauge with n data points under one
// resource carrying marker.
func Metrics(marker string, n int) pmetricotlp.ExportRequest {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	fillResource(rm.Resource(), marker)
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("otlp-collector-oidc/integration")
	g := sm.Metrics().AppendEmpty()
	g.SetName("integration.gauge")
	dps := g.SetEmptyGauge().DataPoints()
	now := time.Now()
	for i := range n {
		dp := dps.AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetIntValue(int64(i))
		dp.Attributes().PutInt("index", int64(i))
	}
	return pmetricotlp.NewExportRequestFromMetrics(md)
}

func fillResource(r pcommon.Resource, marker string) {
	r.Attributes().PutStr("service.name", "integration")
	r.Attributes().PutStr(MarkerKey, marker)
}

// FindTraces returns, as a traces value of its own, the resource that
// carries marker, and whether any received request held it.
func FindTraces(received []ptrace.Traces, marker string) (ptrace.Traces, bool) {
	for _, td := range received {
		for _, rs := range td.ResourceSpans().All() {
			if hasMarker(rs.Resource(), marker) {
				out := ptrace.NewTraces()
				rs.CopyTo(out.ResourceSpans().AppendEmpty())
				return out, true
			}
		}
	}
	return ptrace.Traces{}, false
}

// FindLogs is FindTraces for logs.
func FindLogs(received []plog.Logs, marker string) (plog.Logs, bool) {
	for _, ld := range received {
		for _, rl := range ld.ResourceLogs().All() {
			if hasMarker(rl.Resource(), marker) {
				out := plog.NewLogs()
				rl.CopyTo(out.ResourceLogs().AppendEmpty())
				return out, true
			}
		}
	}
	return plog.Logs{}, false
}

// FindMetrics is FindTraces for metrics.
func FindMetrics(received []pmetric.Metrics, marker string) (pmetric.Metrics, bool) {
	for _, md := range received {
		for _, rm := range md.ResourceMetrics().All() {
			if hasMarker(rm.Resource(), marker) {
				out := pmetric.NewMetrics()
				rm.CopyTo(out.ResourceMetrics().AppendEmpty())
				return out, true
			}
		}
	}
	return pmetric.Metrics{}, false
}

func hasMarker(r pcommon.Resource, marker string) bool {
	v, ok := r.Attributes().Get(MarkerKey)
	return ok && v.Str() == marker
}

// OwnLog is a log record the collector exported about itself, with its
// resource.
type OwnLog struct {
	Record   plog.LogRecord
	Resource pcommon.Resource
}

// FindOwnLogs is every record whose resource's service.name is serviceName
// and whose body is message.
func FindOwnLogs(received []plog.Logs, serviceName, message string) []OwnLog {
	var out []OwnLog
	for _, ld := range received {
		for _, rl := range ld.ResourceLogs().All() {
			if name, ok := rl.Resource().Attributes().Get("service.name"); !ok || name.Str() != serviceName {
				continue
			}
			for _, sl := range rl.ScopeLogs().All() {
				for _, lr := range sl.LogRecords().All() {
					if lr.Body().AsString() == message {
						out = append(out, OwnLog{Record: lr, Resource: rl.Resource()})
					}
				}
			}
		}
	}
	return out
}
