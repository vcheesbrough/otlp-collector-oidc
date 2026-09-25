package otlpsingleport

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	// Registers the gzip codec, so a client's grpc-encoding: gzip is decoded.
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// grpcFrameHeader is the gRPC length prefix. confighttp's body cap counts the
// raw request stream, prefix included, so the message cap is this much less.
const grpcFrameHeader = 5

// newGRPCServer builds the gRPC server that runs inside the HTTP/2 listener
// through grpc.Server.ServeHTTP. It never listens itself. bodyCap is the
// listener's decompressed body cap; a message is capped 5 bytes below it, so
// every oversized message fails the same way, as ResourceExhausted.
func newGRPCServer(bodyCap int64, r *refusals) *grpc.Server {
	g := &grpcRefusals{refusals: r}
	return grpc.NewServer(
		grpc.MaxRecvMsgSize(int(bodyCap-grpcFrameHeader)),
		grpc.StatsHandler(g),
		grpc.UnaryInterceptor(g.markHandled),
		grpc.UnknownServiceHandler(g.unknownService),
	)
}

// isGRPC reports whether a request is gRPC: HTTP/2 with a gRPC content type.
// Everything else is OTLP/HTTP or nothing.
func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// grpcRefusals counts RPCs grpc-go refuses before an Export handler runs: an
// unknown service or method, an oversized message, a message that does not
// decode. An RPC that reached a handler is the pipeline's to count, as
// receiver_refused_*.
type grpcRefusals struct {
	refusals *refusals
}

var _ stats.Handler = (*grpcRefusals)(nil)

type handledKey struct{}

// markHandled runs only once the request message has decoded, and records
// that this RPC reached its handler.
func (g *grpcRefusals) markHandled(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if handled, ok := ctx.Value(handledKey{}).(*atomic.Bool); ok {
		handled.Store(true)
	}
	return next(ctx, req)
}

// unknownService answers a service or method with no registration — a
// signal with no pipeline — as grpc-go would, and counts it: grpc-go calls no
// stats handler for an RPC it cannot route.
func (g *grpcRefusals) unknownService(_ any, stream grpc.ServerStream) error {
	ctx := stream.Context()
	if handled, ok := ctx.Value(handledKey{}).(*atomic.Bool); ok {
		handled.Store(true)
	}
	g.refusals.count(ctx, transportGRPC, refusalUnknownPath)
	method, _ := grpc.MethodFromServerStream(stream)
	return status.Errorf(codes.Unimplemented, "unknown method %s", method)
}

// TagRPC implements stats.Handler.
func (*grpcRefusals) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, handledKey{}, &atomic.Bool{})
}

// HandleRPC implements stats.Handler.
func (g *grpcRefusals) HandleRPC(ctx context.Context, s stats.RPCStats) {
	end, ok := s.(*stats.End)
	if !ok || end.Error == nil {
		return
	}
	if handled, ok := ctx.Value(handledKey{}).(*atomic.Bool); ok && handled.Load() {
		return
	}
	if reason, refused := grpcRefusalReason(status.Code(end.Error)); refused {
		g.refusals.count(ctx, transportGRPC, reason)
	}
}

// TagConn implements stats.Handler.
func (*grpcRefusals) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn implements stats.Handler.
func (*grpcRefusals) HandleConn(context.Context, stats.ConnStats) {}

// grpcRefusalReason classifies an RPC that failed before its handler. A
// client that cancelled or timed out was not refused.
func grpcRefusalReason(c codes.Code) (refusalReason, bool) {
	switch c {
	case codes.Unimplemented:
		return refusalUnknownPath, true
	case codes.ResourceExhausted:
		return refusalBodyTooLarge, true
	case codes.Canceled, codes.DeadlineExceeded, codes.OK:
		return 0, false
	case codes.Unknown, codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
		codes.PermissionDenied, codes.FailedPrecondition, codes.Aborted, codes.OutOfRange,
		codes.Internal, codes.Unavailable, codes.DataLoss, codes.Unauthenticated:
		return refusalDecode, true
	default:
		return refusalDecode, true
	}
}
