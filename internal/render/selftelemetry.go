package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	otelconf "go.opentelemetry.io/contrib/otelconf/v0.3.0"

	"github.com/vcheesbrough/otlp-collector-oidc/internal/build"
	"github.com/vcheesbrough/otlp-collector-oidc/internal/upstream"
)

// The process's own logs are exported by the collector's telemetry SDK
// (service::telemetry::logs::processors) to the logs signal's upstream, so
// an OTEL_EXPORTER_OTLP_LOGS_* override is followed. The SDK's exporter has
// no retry and its own bounded queue; it verifies TLS always.

// Reserved resource keys the environment may not set: the build's version.
const serviceVersionKey = "service.version"

// logProcessor, batchProcessor and otlpExporter are the schema of
// service::telemetry::logs::processors (the OpenTelemetry configuration
// schema, file format 0.3), with only the fields this product sets. They are
// both rendered, as JSON (a YAML subset), and read back by the run command
// into the SDK's own types, so the collector and the run command's own lines
// export identically.
type logProcessor struct {
	Batch batchProcessor `json:"batch"`
}

type batchProcessor struct {
	Exporter struct {
		OTLP otlpExporter `json:"otlp"`
	} `json:"exporter"`
}

type otlpExporter struct {
	Protocol          string       `json:"protocol"`
	Endpoint          string       `json:"endpoint"`
	Insecure          bool         `json:"insecure,omitempty"`
	Certificate       string       `json:"certificate,omitempty"`
	ClientCertificate string       `json:"client_certificate,omitempty"`
	ClientKey         string       `json:"client_key,omitempty"`
	Headers           []namedValue `json:"headers,omitempty"`
	Compression       string       `json:"compression"`
	Timeout           int64        `json:"timeout"` // milliseconds
}

type namedValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// selfLogProcessors is the own-logs export, or nil when LOG_OUTPUT keeps
// the lines on stdout alone.
func (s Settings) selfLogProcessors() []logProcessor {
	cfg, ok := s.Upstream.logs()
	if !s.Own.Output.Upstream() || !ok {
		return nil
	}
	var p logProcessor
	p.Batch.Exporter.OTLP = selfLogsExporter(cfg)
	return []logProcessor{p}
}

// selfLogsExporter is cfg as the SDK's OTLP log exporter reads it: a URL
// whose scheme decides TLS (it ignores insecure with https://), and for HTTP
// the full path, which the SDK uses as given.
func selfLogsExporter(cfg upstream.ExporterConfig) otlpExporter {
	e := otlpExporter{
		Protocol:    string(cfg.Protocol),
		Insecure:    cfg.Plaintext,
		Compression: string(cfg.Compression),
		Timeout:     cfg.Timeout.Milliseconds(),
	}
	switch cfg.Protocol {
	case upstream.ProtocolGRPC:
		scheme := "https://"
		if cfg.Plaintext {
			scheme = "http://"
		}
		e.Endpoint = scheme + cfg.Endpoint
	case upstream.ProtocolHTTPProtobuf:
		e.Endpoint = cfg.Endpoint
		if cfg.SignalPath {
			e.Endpoint = strings.TrimSuffix(cfg.Endpoint, "/") + "/v1/logs"
		}
	}
	if !cfg.Plaintext {
		e.Certificate, e.ClientCertificate, e.ClientKey = cfg.CAFile, cfg.ClientCertFile, cfg.ClientKeyFile
	}
	for _, h := range cfg.Headers.Pairs() {
		e.Headers = append(e.Headers, namedValue{Name: h.Name, Value: h.Value})
	}
	return e
}

// SelfLogProcessors is the template's view: the processors as a JSON flow
// value with every "$" doubled, so no value can start a ${...} reference.
// Empty when there are none.
func (s Settings) SelfLogProcessors() (string, error) {
	procs := s.selfLogProcessors()
	if procs == nil {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(procs); err != nil {
		return "", fmt.Errorf("encoding the own-logs processors: %w", err)
	}
	return strings.ReplaceAll(strings.TrimSpace(buf.String()), "$", "$$"), nil
}

// FallbackServiceName is the process's service.name when OTEL_SERVICE_NAME
// is unset: the one in OTEL_RESOURCE_ATTRIBUTES, as the SDK convention has
// it, else the binary's name.
func (s Settings) FallbackServiceName() string {
	for _, kv := range s.Own.ResourceAttributes {
		if kv.Key == "service.name" {
			return kv.Value
		}
	}
	return build.Command
}

// serviceName is the process's service.name.
func (s Settings) serviceName() string {
	if s.Own.ServiceName != "" {
		return s.Own.ServiceName
	}
	return s.FallbackServiceName()
}

// ownResource is the process's resource beyond what the collector adds
// itself (service.version from the build, service.instance.id): its name,
// then OTEL_RESOURCE_ATTRIBUTES less the keys the name and the build own.
func (s Settings) ownResource() []KeyValue {
	out := []KeyValue{{Key: "service.name", Value: s.serviceName()}}
	return append(out, s.Own.ResourceAttributes.Without("service.name", serviceVersionKey)...)
}

// sdkConfig is the SDK configuration the run command logs through before
// the collector exists, from the same processors and resource the collector
// is rendered with.
func (s Settings) sdkConfig() (otelconf.OpenTelemetryConfiguration, error) {
	procs := s.selfLogProcessors()
	raw, err := json.Marshal(procs)
	if err != nil {
		return otelconf.OpenTelemetryConfiguration{}, fmt.Errorf("encoding the own-logs processors: %w", err)
	}
	var sdkProcs []otelconf.LogRecordProcessor
	if err := json.Unmarshal(raw, &sdkProcs); err != nil {
		return otelconf.OpenTelemetryConfiguration{}, fmt.Errorf("reading the own-logs processors: %w", err)
	}
	attrs := []otelconf.AttributeNameValue{{Name: serviceVersionKey, Value: build.Version()}}
	if s.InstanceID != "" {
		attrs = append(attrs, otelconf.AttributeNameValue{Name: "service.instance.id", Value: s.InstanceID})
	}
	for _, kv := range s.ownResource() {
		attrs = append(attrs, otelconf.AttributeNameValue{Name: kv.Key, Value: kv.Value})
	}
	format := "0.3"
	return otelconf.OpenTelemetryConfiguration{
		FileFormat:     &format,
		LoggerProvider: &otelconf.LoggerProvider{Processors: sdkProcs},
		Resource:       &otelconf.Resource{Attributes: attrs},
	}, nil
}
