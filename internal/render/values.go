package render

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Addr is a host:port the process listens on.
type Addr struct {
	Host string
	Port string
}

// UnmarshalText parses host:port with a host and a port from 1 to 65535.
func (a *Addr) UnmarshalText(text []byte) error {
	host, port, err := net.SplitHostPort(string(text))
	if err != nil || host == "" {
		return errors.New("must be host:port, such as 0.0.0.0:4318")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("port must be a number from 1 to 65535")
	}
	*a = Addr{Host: host, Port: port}
	return nil
}

// String is host:port.
func (a Addr) String() string {
	return net.JoinHostPort(a.Host, a.Port)
}

// LogLevel is the least severity the process logs.
type LogLevel string

// The log levels, as the collector names them.
const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// UnmarshalText accepts exactly one of the log levels.
func (l *LogLevel) UnmarshalText(text []byte) error {
	switch v := LogLevel(text); v {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		*l = v
		return nil
	}
	return errors.New("must be debug, info, warn or error")
}

// LogFormat is how a log line is encoded on stdout.
type LogFormat string

// The log encodings, as zap names them.
const (
	LogFormatJSON    LogFormat = "json"
	LogFormatConsole LogFormat = "console"
)

// UnmarshalText accepts exactly one of the log encodings.
func (f *LogFormat) UnmarshalText(text []byte) error {
	switch v := LogFormat(text); v {
	case LogFormatJSON, LogFormatConsole:
		*f = v
		return nil
	}
	return errors.New("must be json or console")
}

// LogOutput is where the process's own log lines go.
type LogOutput string

// The outputs, as LOG_OUTPUT spells them.
const (
	LogOutputBoth   LogOutput = "both"
	LogOutputOTLP   LogOutput = "otlp"
	LogOutputStdout LogOutput = "stdout"
)

// UnmarshalText accepts exactly one of the outputs.
func (o *LogOutput) UnmarshalText(text []byte) error {
	switch v := LogOutput(text); v {
	case LogOutputBoth, LogOutputOTLP, LogOutputStdout:
		*o = v
		return nil
	}
	return errors.New("must be both, otlp or stdout")
}

// Upstream reports whether lines are exported as OTLP.
func (o LogOutput) Upstream() bool {
	switch o {
	case LogOutputBoth, LogOutputOTLP:
		return true
	case LogOutputStdout:
	}
	return false
}

// Stdout reports whether lines are written to stdout.
func (o LogOutput) Stdout() bool {
	switch o {
	case LogOutputBoth, LogOutputStdout:
		return true
	case LogOutputOTLP:
	}
	return false
}

// KeyValue is one pair of a KeyValueList.
type KeyValue struct {
	Key   string
	Value string
}

// KeyValueList is the OpenTelemetry 'key=value,key2=value2' form, as
// OTEL_RESOURCE_ATTRIBUTES uses it: values percent-decoded, a later pair
// replacing an earlier one of the same key, in first-seen order.
type KeyValueList []KeyValue

// UnmarshalText parses the list. An empty text is an empty list; a pair
// without '=' or with an empty key is refused.
func (l *KeyValueList) UnmarshalText(text []byte) error {
	var out KeyValueList
	index := map[string]int{}
	n := 0
	for item := range strings.SplitSeq(string(text), ",") {
		n++
		if strings.TrimSpace(item) == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		key := strings.TrimSpace(k)
		value, err := url.PathUnescape(strings.TrimSpace(v))
		switch {
		case !ok || key == "":
			return fmt.Errorf("pair %d is not key=value", n)
		case err != nil:
			return fmt.Errorf("pair %d has a malformed percent-encoding", n)
		}
		if i, seen := index[key]; seen {
			out[i].Value = value
			continue
		}
		index[key] = len(out)
		out = append(out, KeyValue{Key: key, Value: value})
	}
	*l = out
	return nil
}

// String is the list in its variable form, for the startup line.
func (l KeyValueList) String() string {
	pairs := make([]string, len(l))
	for i, kv := range l {
		pairs[i] = kv.Key + "=" + url.PathEscape(kv.Value)
	}
	return strings.Join(pairs, ",")
}

// Has reports whether key is in the list.
func (l KeyValueList) Has(key string) bool {
	return slices.ContainsFunc(l, func(kv KeyValue) bool { return kv.Key == key })
}

// Without is the list less the given keys.
func (l KeyValueList) Without(keys ...string) KeyValueList {
	var out KeyValueList
	for _, kv := range l {
		if !slices.Contains(keys, kv.Key) {
			out = append(out, kv)
		}
	}
	return out
}
