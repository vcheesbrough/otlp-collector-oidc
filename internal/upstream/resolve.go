package upstream

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Resolve is the exporter configuration of each of signals, the ones that
// have a pipeline, from the environment lookup reads. Another signal's
// variables are not read: they could not change what is exported. It reports
// every missing, malformed or conflicting variable, each once, not just the
// first.
func Resolve(lookup func(name string) (string, bool), signals ...Signal) (map[Signal]ExporterConfig, error) {
	r := resolver{lookup: lookup, seen: map[string]bool{}}
	out := make(map[Signal]ExporterConfig, len(signals))
	for _, sig := range signals {
		if cfg, ok := r.signal(sig); ok {
			out[sig] = cfg
		}
	}
	if len(out) == len(signals) && !r.anyGRPC {
		if raw, ok := lookup(Prefix + suffixInsecure); ok && strings.EqualFold(raw, "true") {
			r.fail(fmt.Errorf("%s=true, but no upstream uses grpc, the only protocol it applies to", Prefix+suffixInsecure))
		}
	}
	if len(out) == len(signals) && !r.anyTLS {
		for _, s := range settings() {
			if !s.tlsFile {
				continue
			}
			if raw, ok := lookup(s.baseName()); ok && raw != "" {
				r.fail(fmt.Errorf("%s is set, but every upstream is plaintext", s.baseName()))
			}
		}
	}
	if err := errors.Join(r.errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// resolver collects the faults of one Resolve. A base variable is read once
// per signal, so its fault would otherwise be reported three times.
type resolver struct {
	lookup  func(string) (string, bool)
	errs    []error
	seen    map[string]bool
	faulted bool // the signal being resolved has a fault, reported or repeated
	anyTLS  bool // some signal resolved to a TLS upstream
	anyGRPC bool // some signal resolved to a gRPC upstream
}

func (r *resolver) fail(err error) {
	r.faulted = true
	if !r.seen[err.Error()] {
		r.seen[err.Error()] = true
		r.errs = append(r.errs, err)
	}
}

// read is one setting's value for a signal.
type read struct {
	setting   setting
	name      string // the variable the value came from, or the base name
	raw       string // "" when unset and without a default
	perSignal bool   // the value came from the signal's own variable
}

func (r *resolver) value(s setting, sig Signal) read {
	if s.perSignal {
		if raw, ok := r.lookup(s.signalName(sig)); ok && raw != "" {
			return read{setting: s, name: s.signalName(sig), raw: raw, perSignal: true}
		}
	}
	if raw, ok := r.lookup(s.baseName()); ok && raw != "" {
		return read{setting: s, name: s.baseName(), raw: raw}
	}
	return read{setting: s, name: s.baseName(), raw: s.def}
}

func (r *resolver) invalid(v read, err error) {
	r.fail(&VariableError{Name: v.name, Value: shown(v.setting, v.raw), Err: err})
}

// signal resolves sig; ok is false when any of its variables is at fault.
func (r *resolver) signal(sig Signal) (ExporterConfig, bool) {
	r.faulted = false
	vals := map[string]read{}
	for _, s := range settings() {
		vals[s.suffix] = r.value(s, sig)
	}
	var cfg ExporterConfig

	protocol := vals[suffixProtocol]
	if err := cfg.Protocol.parse(protocol.raw); err != nil {
		r.invalid(protocol, err)
	}
	endpoint := vals[suffixEndpoint]
	switch {
	case endpoint.raw == "":
		r.fail(&VariableError{Name: endpoint.name, Err: ErrRequired})
	case cfg.Protocol != "":
		if err := cfg.setEndpoint(endpoint.raw, endpoint.perSignal); err != nil {
			r.invalid(endpoint, err)
		}
	}

	insecure := vals[suffixInsecure]
	forcePlaintext, err := parseBool(insecure.raw)
	if err != nil {
		r.invalid(insecure, err)
	}
	skip := vals[nameSkipVerify]
	skipVerify, err := parseBool(skip.raw)
	if err != nil {
		r.invalid(skip, err)
	}
	headers := vals[suffixHeaders]
	if cfg.Headers, err = parseHeaders(headers.raw); err != nil {
		r.invalid(headers, err)
	}
	timeout := vals[suffixTimeout]
	if ms, err := strconv.ParseInt(timeout.raw, 10, 64); err != nil || ms < 1 || ms > int64(time.Hour/time.Millisecond) {
		r.invalid(timeout, errors.New("must be a whole number of milliseconds, 1 to 3600000"))
	} else {
		cfg.Timeout = time.Duration(ms) * time.Millisecond
	}
	compression := vals[suffixCompression]
	if err := cfg.Compression.parse(compression.raw); err != nil {
		r.invalid(compression, err)
	}
	queue := vals[nameQueueSize]
	if cfg.QueueSize, err = strconv.ParseInt(queue.raw, 10, 64); err != nil || cfg.QueueSize < 1 {
		r.invalid(queue, errors.New("must be a whole number, at least 1"))
	}
	retry := vals[nameRetryMaxElapsed]
	if cfg.RetryMaxElapsed, err = time.ParseDuration(retry.raw); err != nil || cfg.RetryMaxElapsed < 0 {
		r.invalid(retry, errors.New("must be a duration such as 60s, or 0s for no limit"))
	}
	cfg.RetryInitialInterval = capped(defaultRetryInitialInterval, cfg.RetryMaxElapsed)
	cfg.RetryMaxInterval = capped(defaultRetryMaxInterval, cfg.RetryMaxElapsed)

	// Rules across variables, once each has parsed.
	if r.faulted {
		return ExporterConfig{}, false
	}
	switch cfg.Protocol {
	case ProtocolGRPC:
		cfg.Plaintext = cfg.Plaintext || forcePlaintext
		r.anyGRPC = true
	case ProtocolHTTPProtobuf:
		// As the SDK has it, INSECURE does not apply to HTTP, so an inherited
		// base value is for the gRPC signals; the signal's own is a mistake.
		if forcePlaintext && !cfg.Plaintext && insecure.perSignal {
			r.fail(fmt.Errorf("%s=true applies to grpc only: for plaintext http/protobuf, make %s an http:// URL", insecure.name, endpoint.name))
		}
	}
	certificate, clientCert, clientKey := vals[suffixCertificate], vals[suffixClientCertificate], vals[suffixClientKey]
	if (clientCert.raw == "") != (clientKey.raw == "") {
		r.fail(fmt.Errorf("%s and %s must be set together", clientCert.name, clientKey.name))
	}
	if cfg.Plaintext {
		// A base file is for the TLS signals; only one that can apply to
		// nothing is a fault, which Resolve reports once every signal is known.
		for _, v := range []read{certificate, clientCert, clientKey} {
			if v.raw != "" && v.perSignal {
				r.fail(fmt.Errorf("%s is set, but the %s upstream is plaintext", v.name, sig))
			}
		}
	} else {
		r.anyTLS = true
		cfg.SkipVerify = skipVerify
		cfg.CAFile, cfg.ClientCertFile, cfg.ClientKeyFile = certificate.raw, clientCert.raw, clientKey.raw
	}
	if r.faulted {
		return ExporterConfig{}, false
	}
	return cfg, true
}

// setEndpoint reads raw as the protocol wants it. gRPC dials host:port, so
// anything more would be dropped and a missing port dialled as 443; HTTP
// takes a URL, a base one with the signal path still to append.
func (c *ExporterConfig) setEndpoint(raw string, perSignal bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("must be an http:// or https:// URL, such as http://collector:4317")
	}
	c.Plaintext = u.Scheme == "http"
	switch c.Protocol {
	case ProtocolGRPC:
		if u.Port() == "" {
			return errors.New("must name a port, such as http://collector:4317")
		}
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("must be scheme://host:port only, with no credentials, path or query")
		}
		c.Endpoint = u.Host
	case ProtocolHTTPProtobuf:
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("must be a URL with no credentials, query or fragment")
		}
		c.Endpoint = raw
		c.SignalPath = !perSignal
	}
	return nil
}

// capped is d, or limit when that is shorter; a zero limit is no limit.
func capped(d, limit time.Duration) time.Duration {
	if limit > 0 && limit < d {
		return limit
	}
	return d
}

// parseBool accepts the SDK's spelling of a boolean, in any case.
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New("must be true or false")
}
