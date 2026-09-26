package upstream

import "errors"

// Signal is one kind of telemetry, each with its own exporter configuration.
type Signal int

// The signals, in the order the SDK specification lists them.
const (
	SignalTraces Signal = iota
	SignalLogs
	SignalMetrics
)

// Signals is every signal, in order.
func Signals() []Signal {
	return []Signal{SignalTraces, SignalLogs, SignalMetrics}
}

// String is the signal's name as the collector and the OTLP paths spell it.
func (s Signal) String() string {
	switch s {
	case SignalTraces:
		return "traces"
	case SignalLogs:
		return "logs"
	case SignalMetrics:
		return "metrics"
	}
	return "unknown"
}

// variablePart is the signal's infix in a per-signal variable name.
func (s Signal) variablePart() string {
	switch s {
	case SignalTraces:
		return "TRACES_"
	case SignalLogs:
		return "LOGS_"
	case SignalMetrics:
		return "METRICS_"
	}
	panic("upstream: no variable part for signal") // programming error
}

// ParseSignal is the signal named s, as String spells it.
func ParseSignal(s string) (Signal, error) {
	for _, sig := range Signals() {
		if sig.String() == s {
			return sig, nil
		}
	}
	return 0, errors.New("unknown signal " + s)
}

// Protocol is the OTLP transport to the upstream.
type Protocol string

// The protocols, as OTEL_EXPORTER_OTLP_PROTOCOL spells them. The SDK's
// http/json is not offered: the collector's HTTP exporter sends protobuf.
const (
	ProtocolGRPC         Protocol = "grpc"
	ProtocolHTTPProtobuf Protocol = "http/protobuf"
)

func (p *Protocol) parse(raw string) error {
	switch v := Protocol(raw); v {
	case ProtocolGRPC, ProtocolHTTPProtobuf:
		*p = v
		return nil
	}
	return errors.New("must be grpc or http/protobuf")
}

// Compression is how export requests are compressed.
type Compression string

// The compressions, as OTEL_EXPORTER_OTLP_COMPRESSION spells them.
const (
	CompressionGzip Compression = "gzip"
	CompressionNone Compression = "none"
)

func (c *Compression) parse(raw string) error {
	switch v := Compression(raw); v {
	case CompressionGzip, CompressionNone:
		*c = v
		return nil
	}
	return errors.New("must be gzip or none")
}
