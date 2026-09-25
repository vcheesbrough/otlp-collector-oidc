package oidcclientauth

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

// rejections counts every refusal under its reason and logs at most one
// warning per reason per interval, so a client hammering one mistake cannot
// flood the log. The token is never logged.
type rejections struct {
	counter  metric.Int64Counter
	logger   *zap.Logger
	interval time.Duration
	now      func() time.Time

	mu sync.Mutex // guards windows
	// windows is keyed by the closed reason set, so it is bounded.
	windows map[reason]*logWindow
}

// logWindow is one reason's warning state: when the last warning was
// written and how many refusals have gone unlogged since.
type logWindow struct {
	logged     time.Time
	suppressed int64
}

func newRejections(counter metric.Int64Counter, logger *zap.Logger, interval time.Duration, now func() time.Time) *rejections {
	return &rejections{
		counter:  counter,
		logger:   logger,
		interval: interval,
		now:      now,
		windows:  map[reason]*logWindow{},
	}
}

// record counts one refusal and warns about it unless its reason warned
// within the interval. err is the refusal's text, never the token.
func (r *rejections) record(ctx context.Context, why reason, err error) {
	r.counter.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attribute.String("reason", why.String()))))
	if suppressed, ok := r.admit(why); ok {
		r.logger.Warn("Refused a request",
			zap.String("reason", why.String()),
			zap.Error(err),
			zap.Int64("suppressed", suppressed))
	}
}

// admit reports whether a warning for why may be written now and, if so, how
// many refusals for it went unlogged since the last one.
func (r *rejections) admit(why reason) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	w, seen := r.windows[why]
	if !seen {
		r.windows[why] = &logWindow{logged: now}
		return 0, true
	}
	if now.Sub(w.logged) < r.interval {
		w.suppressed++
		return 0, false
	}
	suppressed := w.suppressed
	w.logged, w.suppressed = now, 0
	return suppressed, true
}
