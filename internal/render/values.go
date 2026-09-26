package render

import (
	"errors"
	"net"
	"net/url"
	"strconv"
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

// URLEndpoint is an OTLP endpoint URL whose scheme says whether the
// connection uses TLS.
type URLEndpoint struct {
	// HostPort is the URL's host and port, which is what the gRPC exporter
	// dials.
	HostPort string
	// Insecure is true for http://: plaintext, no TLS.
	Insecure bool
}

// UnmarshalText parses an http:// or https:// URL with a host and a port and
// nothing else: the gRPC exporter dials host:port, so anything more would be
// dropped, and a missing port would be dialled as 443.
func (e *URLEndpoint) UnmarshalText(text []byte) error {
	u, err := url.Parse(string(text))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("must be an http:// or https:// URL, such as http://collector:4317")
	}
	if u.Port() == "" {
		return errors.New("must name a port, such as http://collector:4317")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be scheme://host:port only, with no credentials, path or query")
	}
	*e = URLEndpoint{HostPort: u.Host, Insecure: u.Scheme == "http"}
	return nil
}

// String is the endpoint as a URL again.
func (e URLEndpoint) String() string {
	if e.Insecure {
		return "http://" + e.HostPort
	}
	return "https://" + e.HostPort
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
