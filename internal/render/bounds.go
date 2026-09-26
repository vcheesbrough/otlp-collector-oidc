package render

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"time"
)

// The payload bounds (DESIGN §5): what a client can cost downstream. Each
// bound is one function returning its OTTL, which the template composes for
// the traces and logs pipelines alike. OTTL is code: each statement's intent
// is stated where it is written, so a collector upgrade that changes OTTL's
// semantics is a change here and nowhere else.

// ServiceNamePattern is ALLOWED_SERVICE_NAMES: a regular expression that must
// match the whole of a resource's service.name.
type ServiceNamePattern string

// UnmarshalText accepts a regular expression in Go's syntax, which OTTL's
// IsMatch uses too.
func (p *ServiceNamePattern) UnmarshalText(text []byte) error {
	if _, err := regexp.Compile(anchored(string(text))); err != nil {
		var serr *syntax.Error
		if errors.As(err, &serr) {
			return fmt.Errorf("must be a regular expression: %s", serr.Code)
		}
		return errors.New("must be a regular expression")
	}
	*p = ServiceNamePattern(text)
	return nil
}

// anchored makes a pattern match the whole name: IsMatch finds a match
// anywhere, so "api" would admit "not-api-at-all".
func anchored(pattern string) string {
	return "^(?:" + pattern + ")$"
}

// serviceNameFilter drops a resource, with everything under it, whose
// service.name is absent, not a string, or not matched by pattern. IsString
// comes first so IsMatch never sees a value it would fail on.
func serviceNameFilter(pattern ServiceNamePattern) string {
	return fmt.Sprintf(`not IsString(resource.attributes["service.name"]) or not IsMatch(resource.attributes["service.name"], %s)`, ottlString(anchored(string(pattern))))
}

// pastAgeSpanFilter drops a span that started longer ago than maxAge.
func pastAgeSpanFilter(maxAge time.Duration) string {
	return fmt.Sprintf(`span.start_time < Now() - Duration(%s)`, ottlString(maxAge.String()))
}

// pastAgeLogFilter drops a log record timestamped longer ago than maxAge. A
// record with no timestamp (zero) is kept: its age is unknown, and the
// backend uses the observed time.
func pastAgeLogFilter(maxAge time.Duration) string {
	return fmt.Sprintf(`log.time_unix_nano != 0 and log.time < Now() - Duration(%s)`, ottlString(maxAge.String()))
}

// futureSkewClamps set each timestamp further ahead than skew to now. A span
// clamped at its start is clamped at its end too, since the end is later
// still, so the span never ends before it starts.
func futureSkewSpanClamps(skew time.Duration) []string {
	d := ottlString(skew.String())
	return []string{
		fmt.Sprintf(`set(span.start_time, Now()) where span.start_time > Now() + Duration(%s)`, d),
		fmt.Sprintf(`set(span.end_time, Now()) where span.end_time > Now() + Duration(%s)`, d),
	}
}

func futureSkewLogClamps(skew time.Duration) []string {
	d := ottlString(skew.String())
	return []string{
		fmt.Sprintf(`set(log.time, Now()) where log.time > Now() + Duration(%s)`, d),
	}
}

// ottlString is s as an OTTL string literal.
func ottlString(s string) string {
	out := []byte{'"'}
	for i := range len(s) {
		switch c := s[i]; c {
		case '"', '\\':
			out = append(out, '\\', c)
		default:
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}
