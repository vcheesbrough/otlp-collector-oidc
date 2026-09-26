package upstream

import (
	"errors"
	"fmt"
	"net/url"
)

// Prefix is the SDK's name for every exporter variable; a per-signal form
// inserts TRACES_, LOGS_ or METRICS_ after it.
const Prefix = "OTEL_EXPORTER_OTLP_"

// Variable is one variable as the configuration reference documents it.
type Variable struct {
	Name     string
	Required bool
	Default  string
	Doc      string
}

// setting is one row of the variable table. A per-signal setting is read
// as Prefix+signal+suffix before Prefix+suffix; a global one has its whole
// name in suffix and applies to every signal.
type setting struct {
	suffix    string
	perSignal bool
	required  bool
	secret    bool // never shown in an error or a log line
	tlsFile   bool // meaningless for a plaintext upstream
	def       string
	doc       string
}

// The suffixes Resolve reads. They name the table's rows.
const (
	suffixEndpoint          = "ENDPOINT"
	suffixProtocol          = "PROTOCOL"
	suffixHeaders           = "HEADERS"
	suffixTimeout           = "TIMEOUT"
	suffixCompression       = "COMPRESSION"
	suffixCertificate       = "CERTIFICATE"
	suffixClientCertificate = "CLIENT_CERTIFICATE"
	suffixClientKey         = "CLIENT_KEY"
	suffixInsecure          = "INSECURE"
	nameSkipVerify          = "UPSTREAM_TLS_INSECURE_SKIP_VERIFY"
	nameQueueSize           = "UPSTREAM_QUEUE_SIZE"
	nameRetryMaxElapsed     = "UPSTREAM_RETRY_MAX_ELAPSED"
)

// settings is the variable table: resolution and the reference both read it,
// so a variable is described once. In the doc text, 'x' is set as code.
func settings() []setting {
	return []setting{
		{suffix: suffixEndpoint, perSignal: true, required: true, doc: "Upstream URL. 'http://' is plaintext, 'https://' is TLS verified against the system roots (or 'SSL_CERT_FILE'). With 'grpc' it is 'scheme://host:port' only; with 'http/protobuf' the base endpoint gets '/v1/<signal>' appended and a per-signal one is used verbatim. Not needed when every signal with a pipeline has its own"},
		{suffix: suffixProtocol, perSignal: true, def: string(ProtocolGRPC), doc: "'grpc' or 'http/protobuf'; selects the exporter component"},
		{suffix: suffixHeaders, perSignal: true, secret: true, doc: "'key=value,key2=value2' sent on every export, values percent-decoded: a tenant id, a hosted backend's token. Never logged"},
		{suffix: suffixTimeout, perSignal: true, def: "10000", doc: "Export timeout in milliseconds"},
		{suffix: suffixCompression, perSignal: true, def: string(CompressionGzip), doc: "'gzip' or 'none'"},
		{suffix: suffixCertificate, perSignal: true, tlsFile: true, doc: "CA file that alone verifies the upstream's certificate, in place of the system roots"},
		{suffix: suffixClientCertificate, perSignal: true, tlsFile: true, doc: "Client certificate (PEM) for mTLS to the upstream; set together with 'OTEL_EXPORTER_OTLP_CLIENT_KEY'"},
		{suffix: suffixClientKey, perSignal: true, tlsFile: true, doc: "Its private key (PEM)"},
		{suffix: suffixInsecure, perSignal: true, def: "false", doc: "'true' for plaintext gRPC whatever the scheme. It does not apply to 'http/protobuf', where the scheme alone decides: a base value is ignored there, a per-signal one refused"},
		{suffix: nameSkipVerify, def: "false", doc: "'true' accepts an upstream certificate that does not verify. No SDK equivalent; applies to every TLS upstream, except that the process's own logs always verify, so with a TLS logs upstream it needs 'LOG_OUTPUT=stdout'"},
		{suffix: nameQueueSize, def: "1000", doc: "Export requests held per exporter and pipeline while the upstream is unreachable; beyond it they are dropped and counted. No SDK equivalent"},
		{suffix: nameRetryMaxElapsed, def: "60s", doc: "How long a failing export is retried before it is dropped and counted; '0s' retries forever. The backoff (first 5s, at most 30s) is shortened to it. No SDK equivalent"},
	}
}

// Variables is the reference's view of the table: each per-signal setting
// under its base name, then the global ones.
func Variables() []Variable {
	out := make([]Variable, 0, len(settings()))
	for _, s := range settings() {
		out = append(out, Variable{Name: s.baseName(), Required: s.required, Default: s.def, Doc: s.doc})
	}
	return out
}

// Names is every variable Resolve may read, per-signal forms included.
func Names() []string {
	var out []string
	for _, s := range settings() {
		out = append(out, s.baseName())
		if s.perSignal {
			for _, sig := range Signals() {
				out = append(out, s.signalName(sig))
			}
		}
	}
	return out
}

// PerSignalNote is the reference's paragraph on the per-signal forms.
const PerSignalNote = "Every `" + Prefix + "*` variable above also has `" + Prefix + "TRACES_*`, `" +
	Prefix + "LOGS_*` and `" + Prefix + "METRICS_*` forms, which take precedence for that signal, as " +
	"the OpenTelemetry SDK specification defines them. One exporter is rendered per distinct " +
	"signal configuration, so with no per-signal variable set all signals share one. The `UPSTREAM_*` " +
	"variables apply to every exporter."

// baseName is the variable's name without a signal.
func (s setting) baseName() string {
	if s.perSignal {
		return Prefix + s.suffix
	}
	return s.suffix
}

// signalName is the variable's name for sig; a global setting has one name.
func (s setting) signalName(sig Signal) string {
	if s.perSignal {
		return Prefix + sig.variablePart() + s.suffix
	}
	return s.suffix
}

// ErrRequired marks a required variable that is unset or empty.
var ErrRequired = errors.New("is required")

// VariableError is a variable that is missing, malformed, or in conflict with
// another. Value is empty for a secret variable, so it is never shown.
type VariableError struct {
	Name  string
	Value string
	Err   error
}

// Error names the variable, and its value unless the value is secret.
func (e *VariableError) Error() string {
	switch {
	case errors.Is(e.Err, ErrRequired):
		return e.Name + " " + e.Err.Error()
	case e.Value == "":
		return fmt.Sprintf("invalid %s: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("invalid %s %q: %v", e.Name, e.Value, e.Err)
}

// Unwrap returns Err.
func (e *VariableError) Unwrap() error { return e.Err }

// shown is raw as an error may show it: nothing for a secret, and a URL's
// password masked.
func shown(s setting, raw string) string {
	if s.secret {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		return u.Redacted()
	}
	return raw
}
