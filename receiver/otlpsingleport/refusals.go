package otlpsingleport

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/vcheesbrough/otlp-collector-oidc/receiver/otlpsingleport/internal/metadata"
)

// transport is how a request reached the listener. Its String is the
// transport label, the same values receiverhelper uses.
type transport int

const (
	transportHTTP transport = iota
	transportGRPC
)

func (t transport) String() string {
	switch t {
	case transportHTTP:
		return "http"
	case transportGRPC:
		return "grpc"
	default:
		return "unknown"
	}
}

// refusalReason is why a request was refused before it reached the pipeline.
// It is a closed set, so the reason label is bounded: a new reason is a new
// constant, and the exhaustive linter fails every switch that misses it.
type refusalReason int

const (
	// refusalMethod: an OTLP/HTTP path with a method other than POST.
	refusalMethod refusalReason = iota
	// refusalMediaType: a Content-Type that is not an OTLP encoding.
	refusalMediaType
	// refusalBodyTooLarge: the decompressed body or message exceeds the cap.
	refusalBodyTooLarge
	// refusalDecode: a body or message that is not a valid export request.
	refusalDecode
	// refusalUnknownPath: no route, including a signal with no pipeline.
	refusalUnknownPath
	// refusalDecompress: confighttp's decompressor refused the body.
	refusalDecompress
)

func (r refusalReason) String() string {
	switch r {
	case refusalMethod:
		return "method"
	case refusalMediaType:
		return "media_type"
	case refusalBodyTooLarge:
		return "body_too_large"
	case refusalDecode:
		return "decode"
	case refusalUnknownPath:
		return "unknown_path"
	case refusalDecompress:
		return "decompress"
	default:
		return "unknown"
	}
}

// refusals counts requests refused before the pipeline.
type refusals struct {
	counter  metric.Int64Counter
	receiver string
}

func newRefusals(tb *metadata.TelemetryBuilder, receiverID string) *refusals {
	return &refusals{counter: tb.OtlpsingleportRequestsRefused, receiver: receiverID}
}

func (r *refusals) count(ctx context.Context, t transport, reason refusalReason) {
	r.counter.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(
		attribute.String("receiver", r.receiver),
		attribute.String("transport", t.String()),
		attribute.String("reason", reason.String()),
	)))
}
