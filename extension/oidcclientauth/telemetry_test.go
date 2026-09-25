package oidcclientauth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRejectionWindow is a unit test because the window's edge is a matter
// of time: from outside, only an interval longer than the run (one warning
// per reason, which the integration suite asserts) is deterministic. Here a
// fake clock walks across the edge.
func TestRejectionWindow(t *testing.T) {
	const interval = time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	r := newRejections(nil, nil, interval, func() time.Time { return now })

	steps := []struct {
		name           string
		at             time.Duration
		why            reason
		wantLog        bool
		wantSuppressed int64
	}{
		{name: "first no_token warns", at: 0, why: reasonNoToken, wantLog: true},
		{name: "second within the interval is suppressed", at: time.Second, why: reasonNoToken},
		{name: "third within the interval is suppressed", at: 59 * time.Second, why: reasonNoToken},
		{name: "another reason has its own window", at: 59 * time.Second, why: reasonMissingScope, wantLog: true},
		{name: "at the interval, warns with the count it suppressed", at: interval, why: reasonNoToken, wantLog: true, wantSuppressed: 2},
		{name: "the window restarts from that warning", at: interval + time.Second, why: reasonNoToken},
		{name: "and ends an interval later", at: 2 * interval, why: reasonNoToken, wantLog: true, wantSuppressed: 1},
	}
	for _, s := range steps {
		now = start.Add(s.at)
		suppressed, logged := r.admit(s.why)
		assert.Equal(t, s.wantLog, logged, "%s: logged", s.name)
		assert.Equal(t, s.wantSuppressed, suppressed, "%s: suppressed", s.name)
	}
}
