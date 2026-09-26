package integration

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// protocol is how a scenario reaches the one port.
type protocol int

const (
	protocolHTTP protocol = iota
	protocolGRPC
)

// protocols is every protocol a scenario runs over.
var protocols = [...]protocol{protocolHTTP, protocolGRPC}

func (p protocol) String() string {
	switch p {
	case protocolHTTP:
		return "http"
	case protocolGRPC:
		return "grpc"
	default:
		return fmt.Sprintf("protocol(%d)", int(p))
	}
}

// clients reaches one collector over both protocols.
type clients struct {
	http  *harness.HTTPClient
	http1 *harness.HTTPClient
	grpc  *harness.GRPCClient
}

// newClients returns clients for addr that present bearer on every request,
// or no token when it is empty.
func newClients(t *testing.T, addr string, pool *x509.CertPool, bearer string) clients {
	t.Helper()
	return newClientsVia(t, harness.DirectTo(addr, pool), bearer)
}

// newClientsVia is newClients through any frontend: a proxy in front of the
// collector, or the collector itself.
func newClientsVia(t *testing.T, f harness.Frontend, bearer string) clients {
	t.Helper()
	return clients{
		http:  harness.NewHTTPClientTo(f.BaseURL(), f.Pool(), false).WithBearer(bearer),
		http1: harness.NewHTTPClientTo(f.BaseURL(), f.Pool(), true).WithBearer(bearer),
		grpc:  harness.NewGRPCClient(t, f.GRPCTarget(), f.Pool()).WithBearer(bearer),
	}
}

// outcome is an export's answer in terms both protocols share.
type outcome struct {
	// httpStatus is the HTTP status; zero over gRPC.
	httpStatus int
	// code is the gRPC code, or over HTTP the code of the rpc.Status body.
	code    codes.Code
	message string
	// retryAfter is the HTTP Retry-After header; empty over gRPC.
	retryAfter string
	// retryDelay is the RetryInfo delay the answer carried, if any.
	retryDelay time.Duration
}

// export sends m for signal s over p, uncompressed and protobuf-encoded over
// HTTP, and normalises the answer.
func (c clients) export(ctx context.Context, t *testing.T, p protocol, s harness.Signal, m harness.Message) outcome {
	t.Helper()
	switch p {
	case protocolHTTP:
		resp, err := c.http.Export(ctx, s, m, harness.EncodingProtobuf, harness.CompressionNone)
		require.NoError(t, err)
		return httpOutcome(t, resp)
	case protocolGRPC:
		return grpcOutcome(c.grpc.Export(ctx, m, harness.CompressionNone))
	default:
		require.FailNow(t, "unknown protocol", "%d", int(p))
		return outcome{}
	}
}

func httpOutcome(t *testing.T, resp harness.Response) outcome {
	t.Helper()
	o := outcome{httpStatus: resp.Status, retryAfter: resp.Header.Get("Retry-After")}
	if resp.Status == http.StatusOK {
		o.code = codes.OK
		return o
	}
	if resp.Header.Get("Content-Type") != "application/x-protobuf" {
		o.code = codes.Unknown
		o.message = string(resp.Body)
		return o
	}
	var st spb.Status
	require.NoError(t, proto.Unmarshal(resp.Body, &st), "error body is not an rpc.Status: %q", resp.Body)
	full := status.FromProto(&st)
	o.code, o.message, o.retryDelay = full.Code(), full.Message(), retryDelay(full)
	return o
}

func grpcOutcome(err error) outcome {
	st := status.Convert(err)
	return outcome{code: st.Code(), message: st.Message(), retryDelay: retryDelay(st)}
}

func retryDelay(st *status.Status) time.Duration {
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			return ri.GetRetryDelay().AsDuration()
		}
	}
	return 0
}
