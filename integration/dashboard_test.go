package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
	"google.golang.org/grpc/codes"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// The dashboard and the alert rules are shipped artefacts that query the
// collector's own metrics by name. This proves every name they use is one the
// binary exports on :8888, as Prometheus stores it, so a rename breaks the
// build rather than a deployer's dashboard. It walks every "expr" in every
// file generically, so a new panel or rule is covered without editing it.

// metricToken is a metric name as the queries write it: the collector's own
// namespace, and target_info, the build-info series.
var metricToken = regexp.MustCompile(`\b(otelcol_[a-z0-9_]+|target_info)\b`)

// targetInfoSelector and selectorLabel find the labels a query selects
// target_info on: the join that filters every panel and alert.
var (
	targetInfoSelector = regexp.MustCompile(`target_info\{([^}]*)\}`)
	selectorLabel      = regexp.MustCompile(`([a-z_]+)\s*(?:=~|!~|!=|=)`)
)

// artefactQueries is every expr in dashboards/*.json and alerts/*.yaml.
func artefactQueries(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, pattern := range []string{"../dashboards/*.json", "../alerts/*.yaml"} {
		files, err := filepath.Glob(pattern)
		require.NoError(t, err)
		require.NotEmpty(t, files, "no artefact matches %s", pattern)
		for _, file := range files {
			raw, err := os.ReadFile(file) // #nosec G304 -- the repository's own artefacts
			require.NoError(t, err)
			var doc any
			if strings.HasSuffix(file, ".json") {
				require.NoError(t, json.Unmarshal(raw, &doc), file)
			} else {
				require.NoError(t, yaml.Unmarshal(raw, &doc), file)
			}
			collectExprs(doc, func(expr string) { out[file] = append(out[file], expr) })
			require.NotEmpty(t, out[file], "%s has no expr", file)
		}
	}
	return out
}

func collectExprs(node any, fn func(string)) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if s, ok := v.(string); ok && k == "expr" {
				fn(s)
				continue
			}
			collectExprs(v, fn)
		}
	case []any:
		for _, v := range n {
			collectExprs(v, fn)
		}
	}
}

// failureCounters are created on their first non-zero value, so the shape
// below drives each before the names are checked.
func failureCounters() []string {
	return []string{
		"otelcol_exporter_send_failed_spans",
		"otelcol_exporter_send_failed_log_records",
		"otelcol_exporter_enqueue_failed_spans",
		"otelcol_exporter_enqueue_failed_log_records",
		"otelcol_oidcclientauth_rejections",
		"otelcol_otlpsingleport_requests_refused",
		"otelcol_processor_filter_spans_filtered",
		"otelcol_processor_filter_logs_filtered",
	}
}

// TestArtefactMetrics runs a shape in which every series the artefacts chart
// exists: traffic accepted and exported, a refused token, a request refused
// before the pipeline, and an upstream down behind a one-request queue with
// a short retry, so exports are both dropped at the queue and given up.
func TestArtefactMetrics(t *testing.T) {
	t.Parallel()
	sink := harness.NewSink(t)
	env := startShippedWith(t, sink, map[string]string{
		"UPSTREAM_QUEUE_SIZE":        "1",
		"UPSTREAM_RETRY_MAX_ELAPSED": "1s",
		"LOG_OUTPUT":                 "stdout",
		"OTEL_RESOURCE_ATTRIBUTES":   "deployment.environment.name=artefacts",
	}, nil)
	deliver(t, env, "artefacts", sink, sink)
	got := env.clients.present(t, protocolHTTP, bearer("not-a-jwt"), "artefacts/refused")
	require.Equal(t, codes.Unauthenticated, got.code, got.message)
	resp, err := env.clients.http.Do(t.Context(), http.MethodGet, harness.SignalTraces.Path(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusMethodNotAllowed, resp.Status)
	// A resource with no service.name, dropped by the bounds.
	exportBoth(t, env, protocolHTTP, resourceItem{marker: "artefacts/unnamed", at: time.Now()})

	sink.SetBehaviour(t, harness.BehaviourDown)
	// A loop rather than require.Eventually: each pass asserts every export's
	// answer, which a condition function must not do off the test goroutine.
	deadline := time.Now().Add(eventually)
	for missing := failureCounters(); len(missing) > 0; missing = absent(scrape(t, env.collector), failureCounters()) {
		require.True(t, time.Now().Before(deadline), "never created: %v", missing)
		for _, p := range protocols {
			for _, sig := range []harness.Signal{harness.SignalTraces, harness.SignalLogs} {
				msg := harness.Message(harness.Traces("artefacts/down", 1))
				if sig == harness.SignalLogs {
					msg = harness.Logs("artefacts/down", 1)
				}
				got := env.clients.export(t.Context(), t, p, sig, msg)
				require.Equal(t, codes.OK, got.code, got.message)
			}
		}
	}

	samples := scrape(t, env.collector)
	exported := map[string]bool{}
	for _, s := range samples {
		exported[s.Name] = true
	}
	names := make([]string, 0, len(exported))
	for n := range exported {
		names = append(names, n)
	}
	slices.Sort(names)
	info, ok := samples.Find("target_info")
	require.True(t, ok, "no target_info on :8888")
	assert.Equal(t, "artefacts", info.Labels["deployment_environment_name"])
	for file, exprs := range artefactQueries(t) {
		for _, expr := range exprs {
			for _, name := range metricToken.FindAllString(expr, -1) {
				assert.True(t, exported[name], "%s queries %s, which :8888 does not export\nexpr: %s\nexported: %v", file, name, expr, names)
			}
			// job and instance are the scraper's, not on :8888.
			for _, sel := range targetInfoSelector.FindAllStringSubmatch(expr, -1) {
				for _, l := range selectorLabel.FindAllStringSubmatch(sel[1], -1) {
					assert.Contains(t, info.Labels, l[1], "%s selects target_info on %s, which it does not carry\nexpr: %s", file, l[1], expr)
				}
			}
		}
	}
}

// absent is every name in names that samples has no series of.
func absent(samples harness.Samples, names []string) []string {
	var out []string
	for _, n := range names {
		if _, ok := samples.Find(n); !ok {
			out = append(out, n)
		}
	}
	return out
}
