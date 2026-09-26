package upstream

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ExporterConfig is everything one exporter needs. It is comparable, so
// signals that resolve to the same value can share one exporter: the renderer
// de-duplicates with a map keyed by it.
type ExporterConfig struct {
	Protocol Protocol
	// Endpoint is host:port for gRPC, and a URL for HTTP.
	Endpoint string
	// SignalPath is true for an HTTP base endpoint, to which the exporter
	// appends /v1/<signal>; a per-signal HTTP endpoint is used verbatim.
	SignalPath bool
	// Plaintext is true for no TLS at all: an http:// endpoint, or
	// OTEL_EXPORTER_OTLP_INSECURE over gRPC. The fields below it apply only
	// when it is false.
	Plaintext bool
	// SkipVerify accepts a TLS certificate that does not verify. It is
	// UPSTREAM_TLS_INSECURE_SKIP_VERIFY, a different knob from Plaintext.
	SkipVerify     bool
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string

	Headers         Headers
	Timeout         time.Duration
	Compression     Compression
	QueueSize       int64
	RetryMaxElapsed time.Duration
	// RetryInitialInterval and RetryMaxInterval are the first backoff and
	// the cap on it: the collector's 5s and 30s, each shortened to
	// RetryMaxElapsed when that is shorter, which the collector requires.
	RetryInitialInterval time.Duration
	RetryMaxInterval     time.Duration
}

// The collector's own retry backoff.
const (
	defaultRetryInitialInterval = 5 * time.Second
	defaultRetryMaxInterval     = 30 * time.Second
)

// String describes the configuration for a log line, with no secret in it:
// headers are named, never valued.
func (c ExporterConfig) String() string {
	var b strings.Builder
	b.WriteString(string(c.Protocol) + " " + c.Endpoint)
	if c.SignalPath {
		b.WriteString(" (+/v1/<signal>)")
	}
	if c.Plaintext {
		b.WriteString(" plaintext")
	} else {
		b.WriteString(" tls")
	}
	if names := c.Headers.Names(); len(names) > 0 {
		b.WriteString(" headers=" + strings.Join(names, ","))
	}
	return b.String()
}

// Header is one request header sent upstream.
type Header struct {
	Name  string
	Value string
}

// Headers is a header list in a comparable form: each pair percent-encoded
// and the pairs joined with "&", sorted by name. Pairs decodes it.
type Headers string

// Pairs is the header list, sorted by name.
func (h Headers) Pairs() []Header {
	if h == "" {
		return nil
	}
	var out []Header
	for pair := range strings.SplitSeq(string(h), "&") {
		k, v, _ := strings.Cut(pair, "=")
		// Encoded by newHeaders with QueryEscape, so these cannot fail.
		name, _ := url.QueryUnescape(k)
		value, _ := url.QueryUnescape(v)
		out = append(out, Header{Name: name, Value: value})
	}
	return out
}

// Names is the headers' names, sorted.
func (h Headers) Names() []string {
	var out []string
	for _, p := range h.Pairs() {
		out = append(out, p.Name)
	}
	return out
}

// Redacted is h with every value replaced, for a configuration that is logged.
func (h Headers) Redacted() Headers {
	pairs := h.Pairs()
	for i := range pairs {
		pairs[i].Value = "<redacted>"
	}
	return newHeaders(pairs)
}

func newHeaders(pairs []Header) Headers {
	slices.SortStableFunc(pairs, func(a, b Header) int { return strings.Compare(a.Name, b.Name) })
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = url.QueryEscape(p.Name) + "=" + url.QueryEscape(p.Value)
	}
	return Headers(strings.Join(encoded, "&"))
}

// parseHeaders reads the SDK's 'key=value,key2=value2' form. Values, and
// keys, are percent-decoded; a later pair replaces an earlier one of the
// same name. Errors never quote the value, which may be a credential.
func parseHeaders(raw string) (Headers, error) {
	byName := map[string]int{}
	var pairs []Header
	n := 0
	for item := range strings.SplitSeq(raw, ",") {
		n++
		if strings.TrimSpace(item) == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		name, errK := url.PathUnescape(strings.TrimSpace(k))
		value, errV := url.PathUnescape(strings.TrimSpace(v))
		switch {
		case !ok || name == "":
			return "", pairError(n, "is not key=value")
		case errK != nil || errV != nil:
			return "", pairError(n, "has a malformed percent-encoding")
		case !validHeaderName(name):
			return "", pairError(n, "has a name that is not an HTTP header token")
		case !validHeaderValue(value):
			return "", pairError(n, "has a value with a control or non-ASCII character")
		}
		if i, seen := byName[strings.ToLower(name)]; seen {
			pairs[i] = Header{Name: name, Value: value}
			continue
		}
		byName[strings.ToLower(name)] = len(pairs)
		pairs = append(pairs, Header{Name: name, Value: value})
	}
	return newHeaders(pairs), nil
}

// validHeaderValue is visible ASCII, space and tab: RFC 9110's field value
// without obs-text, which gRPC metadata refuses. Anything else would pass
// startup and fail every export.
func validHeaderValue(value string) bool {
	for i := range len(value) {
		if c := value[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			return false
		}
	}
	return true
}

func pairError(n int, what string) error {
	return errors.New("pair " + strconv.Itoa(n) + " " + what)
}

// validHeaderName is RFC 9110's token: what an HTTP/2 or gRPC header name may be.
func validHeaderName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}
