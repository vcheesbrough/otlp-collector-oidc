// Package upstream resolves the standard OTEL_EXPORTER_OTLP_* variables, and
// the few UPSTREAM_* ones with no SDK equivalent, into one exporter
// configuration per signal.
//
// It owns their semantics as the OpenTelemetry SDK specification defines
// them: a per-signal variable takes precedence over the base one for its
// signal; a base HTTP endpoint gets /v1/<signal> appended while a per-signal
// one is used verbatim; the timeout is in milliseconds; headers are
// comma-separated key=value pairs with percent-encoded values. Resolve is a
// pure function of its lookup; internal/render turns what it returns into
// exporter blocks and never reads one of these variables itself.
package upstream
