package otlpsingleport

import (
	"net/http"
	"strings"

	"google.golang.org/grpc"
	// Registers the gzip codec, so a client's grpc-encoding: gzip is decoded.
	_ "google.golang.org/grpc/encoding/gzip"
)

// newGRPCServer builds the gRPC server that runs inside the HTTP/2 listener
// through grpc.Server.ServeHTTP. It never listens itself. maxMessageSize is
// the listener's decompressed body cap, so both protocols share one bound.
func newGRPCServer(maxMessageSize int64) *grpc.Server {
	return grpc.NewServer(grpc.MaxRecvMsgSize(int(maxMessageSize)))
}

// isGRPC reports whether a request is gRPC: HTTP/2 with a gRPC content type.
// Everything else is OTLP/HTTP or nothing.
func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}
