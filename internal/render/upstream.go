package render

import (
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/vcheesbrough/otlp-collector-oidc/internal/upstream"
)

// UpstreamSettings is every signal's resolved upstream. Its variables are
// internal/upstream's to read and document, since one variable may mean a
// different exporter per signal; the template reaches it through Exporters
// and ExporterFor.
type UpstreamSettings struct {
	bySignal map[upstream.Signal]upstream.ExporterConfig
}

// pipelineSignals is the signals the template gives a pipeline, and so the
// only ones whose upstream is resolved. The metrics pipeline adds metrics.
func pipelineSignals() []upstream.Signal {
	return []upstream.Signal{upstream.SignalTraces, upstream.SignalLogs}
}

func (u *UpstreamSettings) resolve(lookup Lookup) error {
	resolved, err := upstream.Resolve(lookup, pipelineSignals()...)
	if err != nil {
		// Each fault already names its variable, as the other groups' do.
		return err
	}
	u.bySignal = resolved
	return nil
}

func (*UpstreamSettings) reference() (string, []variable) {
	var vars []variable
	for _, v := range upstream.Variables() {
		vars = append(vars, variable{name: v.Name, required: v.Required, def: v.Default, doc: v.Doc})
	}
	return upstream.PerSignalNote, vars
}

// Exporter is one exporter block: the configuration some signals share, and
// the keys its endpoint is rendered under.
type Exporter struct {
	// ID is the component id, such as otlp_grpc or otlp_http/logs.
	ID     string
	Config upstream.ExporterConfig
	// Endpoints are the endpoint keys and their values: endpoint for gRPC
	// and an HTTP base URL, <signal>_endpoint for each signal of a
	// per-signal HTTP URL.
	Endpoints []Endpoint
	Headers   []upstream.Header
}

// Endpoint is one endpoint key of an exporter.
type Endpoint struct {
	Key   string
	Value string
}

// Exporters is one exporter per distinct configuration among the resolved
// signals, in signal order.
func (u UpstreamSettings) Exporters() []Exporter {
	var out []Exporter
	for _, e := range u.all() {
		out = append(out, e.Exporter)
	}
	return out
}

// ExporterFor is the id of the exporter the named signal's pipeline uses.
func (u UpstreamSettings) ExporterFor(signal string) (string, error) {
	sig, err := upstream.ParseSignal(signal)
	if err != nil {
		return "", err
	}
	for _, e := range u.all() {
		if slices.Contains(e.signals, sig) {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("no upstream resolved for %s", signal)
}

// exporter is an Exporter and the signals it serves.
type exporter struct {
	Exporter
	signals []upstream.Signal
}

// all groups the resolved signals by configuration. The id names the signals
// an exporter serves unless it serves them all, so a deployment that sets no
// per-signal variable has one plain otlp_grpc or otlp_http.
func (u UpstreamSettings) all() []exporter {
	var order []upstream.ExporterConfig
	bySignals := map[upstream.ExporterConfig][]upstream.Signal{}
	for _, sig := range upstream.Signals() {
		cfg, ok := u.bySignal[sig]
		if !ok {
			continue
		}
		if _, seen := bySignals[cfg]; !seen {
			order = append(order, cfg)
		}
		bySignals[cfg] = append(bySignals[cfg], sig)
	}
	out := make([]exporter, 0, len(order))
	for _, cfg := range order {
		sigs := bySignals[cfg]
		id := componentType(cfg.Protocol)
		if len(sigs) < len(u.bySignal) {
			names := make([]string, len(sigs))
			for i, sig := range sigs {
				names[i] = sig.String()
			}
			id += "/" + strings.Join(names, "_")
		}
		out = append(out, exporter{
			Exporter: Exporter{ID: id, Config: cfg, Endpoints: endpoints(cfg, sigs), Headers: cfg.Headers.Pairs()},
			signals:  sigs,
		})
	}
	return out
}

func componentType(p upstream.Protocol) string {
	switch p {
	case upstream.ProtocolGRPC:
		return "otlp_grpc"
	case upstream.ProtocolHTTPProtobuf:
		return "otlp_http"
	}
	panic("render: no exporter for protocol " + string(p)) // programming error
}

// endpoints is where the exporter sends: a per-signal HTTP URL is set as the
// signal's own key, so the exporter uses it verbatim.
func endpoints(cfg upstream.ExporterConfig, sigs []upstream.Signal) []Endpoint {
	if cfg.Protocol == upstream.ProtocolGRPC || cfg.SignalPath {
		return []Endpoint{{Key: "endpoint", Value: cfg.Endpoint}}
	}
	out := make([]Endpoint, len(sigs))
	for i, sig := range sigs {
		out[i] = Endpoint{Key: sig.String() + "_endpoint", Value: cfg.Endpoint}
	}
	return out
}

// redacted is u with every header value replaced, for the rendering that is
// logged.
func (u UpstreamSettings) redacted() UpstreamSettings {
	out := UpstreamSettings{bySignal: make(map[upstream.Signal]upstream.ExporterConfig, len(u.bySignal))}
	for sig, cfg := range u.bySignal {
		cfg.Headers = cfg.Headers.Redacted()
		out.bySignal[sig] = cfg
	}
	return out
}

// logFields is each signal's upstream for the startup line, without secrets.
func (u UpstreamSettings) logFields() []zap.Field {
	var out []zap.Field
	for _, sig := range upstream.Signals() {
		if cfg, ok := u.bySignal[sig]; ok {
			out = append(out, zap.String("upstream."+sig.String(), cfg.String()))
		}
	}
	return out
}
