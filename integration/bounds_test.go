package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The payload bounds (ALLOWED_SERVICE_NAMES, MAX_FUTURE_SKEW, MAX_PAST_AGE):
// what does not pass is dropped after the client's 200 and counted. Each
// request that should lose something also carries a witness resource that
// passes, so the witness arriving proves the request was processed.

// boundsEnv is the shape the bounds run in.
func boundsEnv() map[string]string {
	return map[string]string{
		// A backslash, to prove the pattern reaches OTTL escaped.
		"ALLOWED_SERVICE_NAMES": `app-(web|ios)|svc-\d+`,
		"MAX_FUTURE_SKEW":       "5m",
		"MAX_PAST_AGE":          "1h",
	}
}

// resourceItem is one resource of a request: its service.name (none when
// empty), marker, and the one span's start or record's time.
type resourceItem struct {
	service string
	// numeric sets service.name to a number instead of service.
	numeric bool
	// end, when set, is the span's end instead of at plus a second.
	end    time.Time
	marker string
	at     time.Time
	noTime bool // no timestamp: a record without one, a span without a start
}

func tracesOf(items ...resourceItem) ptraceotlp.ExportRequest {
	td := ptrace.NewTraces()
	for i, it := range items {
		rs := td.ResourceSpans().AppendEmpty()
		fillBoundsResource(rs.Resource(), it)
		span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
		span.SetName("bounded")
		span.SetTraceID(pcommon.TraceID{9, 9, byte(i + 1)})
		span.SetSpanID(pcommon.SpanID{9, byte(i + 1)})
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(it.at))
		end := it.at.Add(time.Second)
		if !it.end.IsZero() {
			end = it.end
		}
		if it.noTime {
			span.SetStartTimestamp(0)
		}
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
	}
	return ptraceotlp.NewExportRequestFromTraces(td)
}

func logsOf(items ...resourceItem) plogotlp.ExportRequest {
	ld := plog.NewLogs()
	for _, it := range items {
		rl := ld.ResourceLogs().AppendEmpty()
		fillBoundsResource(rl.Resource(), it)
		lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		lr.Body().SetStr("bounded")
		if !it.noTime {
			lr.SetTimestamp(pcommon.NewTimestampFromTime(it.at))
		}
	}
	return plogotlp.NewExportRequestFromLogs(ld)
}

func fillBoundsResource(r pcommon.Resource, it resourceItem) {
	switch {
	case it.numeric:
		r.Attributes().PutInt("service.name", 42)
	case it.service != "":
		r.Attributes().PutStr("service.name", it.service)
	}
	r.Attributes().PutStr(harness.MarkerKey, it.marker)
}

// exportBoth sends the traces and logs of items over p and requires 200/OK.
func exportBoth(t *testing.T, env shipped, p protocol, items ...resourceItem) {
	t.Helper()
	got := env.clients.export(t.Context(), t, p, harness.SignalTraces, tracesOf(items...))
	require.Equal(t, codes.OK, got.code, "a client is told nothing of a drop: %s", got.message)
	got = env.clients.export(t.Context(), t, p, harness.SignalLogs, logsOf(items...))
	require.Equal(t, codes.OK, got.code, "a client is told nothing of a drop: %s", got.message)
}

// arrived waits for marker's span and record, and returns the span's start
// and end and the record's time.
func arrived(t *testing.T, sink *harness.Sink, marker string) (start, end, logged time.Time) {
	t.Helper()
	var td ptrace.Traces
	var ld plog.Logs
	require.Eventually(t, func() bool {
		r := sink.Received()
		var okT, okL bool
		td, okT = harness.FindTraces(r.Traces, marker)
		ld, okL = harness.FindLogs(r.Logs, marker)
		return okT && okL
	}, eventually, 10*time.Millisecond, "%s never arrived as a span and a record", marker)
	span := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	return span.StartTimestamp().AsTime(), span.EndTimestamp().AsTime(), lr.Timestamp().AsTime()
}

// absent asserts marker reached the sink as neither span nor record.
func absentAt(t *testing.T, sink *harness.Sink, marker string) {
	t.Helper()
	r := sink.Received()
	_, okT := harness.FindTraces(r.Traces, marker)
	_, okL := harness.FindLogs(r.Logs, marker)
	assert.False(t, okT, "%s's span was not dropped", marker)
	assert.False(t, okL, "%s's record was not dropped", marker)
}

func TestBounds(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, boundsEnv(), nil)
	filtered := func() float64 {
		s := scrape(t, env.collector)
		return s.Sum("otelcol_processor_filter_spans_filtered", nil) + s.Sum("otelcol_processor_filter_logs_filtered", nil)
	}

	for _, p := range protocols {
		t.Run("service names/"+p.String(), func(t *testing.T) {
			m := "bounds/names/" + p.String()
			before := filtered()
			// One request mixing an allowed name, a disallowed one, one that
			// only contains an allowed name, and none at all.
			exportBoth(t, env, p,
				resourceItem{service: "app-web", marker: m + "/allowed", at: time.Now()},
				resourceItem{service: "rogue", marker: m + "/rogue", at: time.Now()},
				resourceItem{service: "my-app-web-2", marker: m + "/substring", at: time.Now()},
				resourceItem{marker: m + "/unnamed", at: time.Now()},
				resourceItem{numeric: true, marker: m + "/numeric", at: time.Now()},
				resourceItem{service: "svc-12", marker: m + "/escaped", at: time.Now()},
			)
			arrived(t, sink, m+"/allowed")
			arrived(t, sink, m+"/escaped")
			for _, dropped := range []string{"/rogue", "/substring", "/unnamed", "/numeric"} {
				absentAt(t, sink, m+dropped)
			}
			assert.Eventually(t, func() bool { return filtered()-before == 8 }, eventually, 50*time.Millisecond,
				"the filter's counters moved by %v, want 8 (4 spans, 4 records)", filtered()-before)
		})

		t.Run("past age/"+p.String(), func(t *testing.T) {
			m := "bounds/past/" + p.String()
			before := filtered()
			exportBoth(t, env, p,
				resourceItem{service: "app-ios", marker: m + "/inside", at: time.Now().Add(-50 * time.Minute)},
				resourceItem{service: "app-ios", marker: m + "/beyond", at: time.Now().Add(-2 * time.Hour)},
			)
			start, _, logged := arrived(t, sink, m+"/inside")
			assert.WithinDuration(t, time.Now().Add(-50*time.Minute), start, time.Minute, "a span inside MAX_PAST_AGE arrives unchanged")
			assert.WithinDuration(t, time.Now().Add(-50*time.Minute), logged, time.Minute)
			absentAt(t, sink, m+"/beyond")
			assert.Eventually(t, func() bool { return filtered()-before == 2 }, eventually, 50*time.Millisecond,
				"the filter's counters moved by %v, want 2 (a span, a record)", filtered()-before)
		})

		t.Run("future skew/"+p.String(), func(t *testing.T) {
			m := "bounds/future/" + p.String()
			sent := time.Now()
			exportBoth(t, env, p,
				resourceItem{service: "app-web", marker: m + "/inside", at: sent.Add(4 * time.Minute)},
				resourceItem{service: "app-web", marker: m + "/beyond", at: sent.Add(time.Hour)},
			)
			start, end, logged := arrived(t, sink, m+"/inside")
			assert.WithinDuration(t, sent.Add(4*time.Minute), start, time.Second, "inside MAX_FUTURE_SKEW is unchanged")
			assert.WithinDuration(t, sent.Add(4*time.Minute+time.Second), end, time.Second)
			assert.WithinDuration(t, sent.Add(4*time.Minute), logged, time.Second)

			start, end, logged = arrived(t, sink, m+"/beyond")
			received := time.Now()
			for name, ts := range map[string]time.Time{"start": start, "end": end, "log time": logged} {
				assert.False(t, ts.Before(sent) || ts.After(received), "%s %v was not clamped to the collector's now (sent %v, received %v)", name, ts, sent, received)
			}
			assert.False(t, end.Before(start), "the clamped span ends before it starts")

			// Starting inside the skew and ending beyond it: the end becomes
			// the start, never earlier.
			m2 := m + "/straddling"
			got := env.clients.export(t.Context(), t, p, harness.SignalTraces, tracesOf(
				resourceItem{service: "app-web", marker: m2, at: sent.Add(4 * time.Minute), end: sent.Add(time.Hour)}))
			require.Equal(t, codes.OK, got.code, got.message)
			var td ptrace.Traces
			require.Eventually(t, func() bool {
				var ok bool
				td, ok = harness.FindTraces(sink.Received().Traces, m2)
				return ok
			}, eventually, 10*time.Millisecond, "%s never arrived", m2)
			span := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
			assert.WithinDuration(t, sent.Add(4*time.Minute), span.StartTimestamp().AsTime(), time.Second, "the start inside the skew is unchanged")
			assert.Equal(t, span.StartTimestamp(), span.EndTimestamp(), "the end beyond the skew becomes the start")
		})
	}

	for _, p := range protocols {
		t.Run("untimed/"+p.String(), func(t *testing.T) {
			// A record with no timestamp is kept; a span with no start is
			// dropped. A timed witness in the same requests proves both were
			// processed.
			m := "bounds/untimed/" + p.String()
			exportBoth(t, env, p,
				resourceItem{service: "app-web", marker: m + "/untimed", noTime: true},
				resourceItem{service: "app-web", marker: m + "/witness", at: time.Now()},
			)
			arrived(t, sink, m+"/witness")
			r := sink.Received()
			_, spanKept := harness.FindTraces(r.Traces, m+"/untimed")
			_, recordKept := harness.FindLogs(r.Logs, m+"/untimed")
			assert.False(t, spanKept, "a span with no start is dropped")
			assert.True(t, recordKept, "a record with no timestamp is kept")
		})
	}
}

// TestBoundsAnyName admits every name with ".*" (a resource with no
// service.name is still dropped); the shipped shape runs with it, and every
// other scenario relies on it.
func TestBoundsAnyName(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, nil, nil)
	for _, p := range protocols {
		m := "bounds/any/" + p.String()
		exportBoth(t, env, p,
			resourceItem{service: "anything-at-all", marker: m + "/named", at: time.Now()},
			resourceItem{marker: m + "/unnamed", at: time.Now()},
		)
		arrived(t, sink, m+"/named")
		absentAt(t, sink, m+"/unnamed")
	}
}
