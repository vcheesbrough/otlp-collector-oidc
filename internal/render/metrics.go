package render

import (
	"fmt"
	"slices"
	"strings"
)

// The client metrics pipeline (DESIGN §5, "Metrics"): a separate, stricter
// chain. It has no identity action at all, and this file never refers to
// identity.go's functions, so user.* and session.* cannot reach a datapoint
// by construction. Name and key allowlists are data; the resource is
// reduced to what identifies the client build and the deployment.

// MetricNamePattern is ALLOWED_METRIC_NAMES: a regular expression the whole
// metric name must match. Empty admits nothing.
type MetricNamePattern struct {
	ServiceNamePattern
}

// UnmarshalText accepts the empty string (every metric dropped) or a pattern
// ServiceNamePattern accepts.
func (p *MetricNamePattern) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*p = MetricNamePattern{}
		return nil
	}
	return p.ServiceNamePattern.UnmarshalText(text)
}

// metricNameFilter drops a metric whose name the pattern does not match, or
// every metric when there is no pattern.
func metricNameFilter(pattern MetricNamePattern) string {
	if pattern.ServiceNamePattern == "" {
		return "true"
	}
	return fmt.Sprintf(`not IsMatch(metric.name, %s)`, ottlString(anchored(string(pattern.ServiceNamePattern))))
}

// metricResourceKeys is what a client metric's resource keeps: the client
// build's identity. The deployment's attributes are upserted after.
func metricResourceKeys() []string {
	return []string{"service.name", "service.version"}
}

// metricResourceReduce keeps only metricResourceKeys on the resource. It is
// the resource statement; metricKeepKeys is the datapoint one.
func metricResourceReduce() string {
	return fmt.Sprintf(`keep_keys(resource.attributes, %s)`, ottlList(metricResourceKeys()))
}

// metricKeepKeys keeps only the allowlisted datapoint attribute keys, less
// any key that could carry identity: every claim target, and user.*,
// session.* and enduser.* whatever the allowlist says.
func metricKeepKeys(allowed []string, claims KeyValueList) string {
	kept := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if !identityKey(key, claims) {
			kept = append(kept, key)
		}
	}
	return fmt.Sprintf(`keep_keys(datapoint.attributes, %s)`, ottlList(kept))
}

// identityKey is a key a metric never carries.
func identityKey(key string, claims KeyValueList) bool {
	for _, prefix := range []string{"user.", "session.", "enduser."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return slices.ContainsFunc(claims, func(kv KeyValue) bool { return kv.Value == key })
}

// metricProcessors is the client metrics chain; resource/metrics only when
// the deployment states attributes to add.
func metricProcessors(clientResource KeyValueList) []string {
	chain := []string{"memory_limiter", "filter/metrics", "transform/metrics"}
	if len(clientResource) > 0 {
		chain = append(chain, "resource/metrics")
	}
	return append(chain, "deltatocumulative", "batch")
}

// ottlList is items as an OTTL list of string literals.
func ottlList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = ottlString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
