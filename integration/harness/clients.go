package harness

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
)

// Signal is an OTLP signal.
type Signal int

// The OTLP signals.
const (
	SignalTraces Signal = iota
	SignalLogs
	SignalMetrics
)

func (s Signal) String() string {
	switch s {
	case SignalTraces:
		return "traces"
	case SignalLogs:
		return "logs"
	case SignalMetrics:
		return "metrics"
	default:
		return fmt.Sprintf("signal(%d)", int(s))
	}
}

// Path is the signal's OTLP/HTTP path.
func (s Signal) Path() string {
	return "/v1/" + s.String()
}

// Encoding is an OTLP/HTTP body encoding.
type Encoding int

// The OTLP/HTTP encodings.
const (
	EncodingProtobuf Encoding = iota
	EncodingJSON
)

func (e Encoding) String() string {
	switch e {
	case EncodingProtobuf:
		return "protobuf"
	case EncodingJSON:
		return "json"
	default:
		return fmt.Sprintf("encoding(%d)", int(e))
	}
}

// ContentType is the encoding's media type.
func (e Encoding) ContentType() string {
	switch e {
	case EncodingProtobuf:
		return "application/x-protobuf"
	case EncodingJSON:
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// Compression is how a request body is compressed on the wire.
type Compression int

// The request compressions the harness sends.
const (
	CompressionNone Compression = iota
	CompressionGzip
)

func (c Compression) String() string {
	switch c {
	case CompressionNone:
		return "plain"
	case CompressionGzip:
		return "gzip"
	default:
		return fmt.Sprintf("compression(%d)", int(c))
	}
}

// Message is an OTLP export request as pdata builds it.
type Message interface {
	MarshalProto() ([]byte, error)
	MarshalJSON() ([]byte, error)
}

// Encode serialises m in e.
func Encode(m Message, e Encoding) ([]byte, error) {
	switch e {
	case EncodingProtobuf:
		return m.MarshalProto()
	case EncodingJSON:
		return m.MarshalJSON()
	default:
		return nil, fmt.Errorf("unknown encoding %d", int(e))
	}
}

// Gzip compresses b.
func Gzip(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// Response is an HTTP answer, read in full.
type Response struct {
	Status int
	Proto  string
	Header http.Header
	// Trailer carries gRPC's grpc-status when a raw request speaks gRPC.
	Trailer http.Header
	Body    []byte
}

// HTTPClient speaks OTLP/HTTP to the listener over TLS.
type HTTPClient struct {
	base   string
	client *http.Client
	// bearer, when set, is sent as Authorization: Bearer on every request
	// whose header does not set Authorization itself.
	bearer string
}

// WithBearer returns a client on the same connection pool that presents
// token, or no token when it is empty.
func (c *HTTPClient) WithBearer(token string) *HTTPClient {
	return &HTTPClient{base: c.base, client: c.client, bearer: token}
}

// NewHTTPClient returns a client for addr that trusts pool. It negotiates
// HTTP/2 unless http1 is set.
func NewHTTPClient(addr string, pool *x509.CertPool, http1 bool) *HTTPClient {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: !http1,
	}
	if http1 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &HTTPClient{base: "https://" + addr, client: &http.Client{Transport: tr}}
}

// Do sends one request and reads the whole answer.
func (c *HTTPClient) Do(ctx context.Context, method, path string, header http.Header, body []byte) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("building request: %w", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.bearer != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	return Response{Status: resp.StatusCode, Proto: resp.Proto, Header: resp.Header, Trailer: resp.Trailer, Body: b}, nil
}

// Export posts m to the signal's path in encoding e with compression comp.
func (c *HTTPClient) Export(ctx context.Context, s Signal, m Message, e Encoding, comp Compression) (Response, error) {
	body, err := Encode(m, e)
	if err != nil {
		return Response{}, err
	}
	header := http.Header{"Content-Type": {e.ContentType()}}
	if comp == CompressionGzip {
		if body, err = Gzip(body); err != nil {
			return Response{}, err
		}
		header.Set("Content-Encoding", "gzip")
	}
	return c.Do(ctx, http.MethodPost, s.Path(), header, body)
}

// GRPCClient speaks OTLP/gRPC to the listener over TLS.
type GRPCClient struct {
	conn *grpc.ClientConn
	// bearer, when set, is sent as authorization: Bearer metadata on every
	// call.
	bearer string
}

// WithBearer returns a client on the same connection that presents token,
// or no token when it is empty.
func (c *GRPCClient) WithBearer(token string) *GRPCClient {
	return &GRPCClient{conn: c.conn, bearer: token}
}

// NewGRPCClient dials addr trusting pool and closes the connection when the
// test ends.
func NewGRPCClient(t *testing.T, addr string, pool *x509.CertPool) *GRPCClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &GRPCClient{conn: conn}
}

// Export sends m, which must be a traces, logs or metrics export request.
func (c *GRPCClient) Export(ctx context.Context, m Message, comp Compression) error {
	var opts []grpc.CallOption
	if comp == CompressionGzip {
		opts = append(opts, grpc.UseCompressor(grpcgzip.Name))
	}
	if c.bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.bearer)
	}
	var err error
	switch req := m.(type) {
	case ptraceotlp.ExportRequest:
		_, err = ptraceotlp.NewGRPCClient(c.conn).Export(ctx, req, opts...)
	case plogotlp.ExportRequest:
		_, err = plogotlp.NewGRPCClient(c.conn).Export(ctx, req, opts...)
	case pmetricotlp.ExportRequest:
		_, err = pmetricotlp.NewGRPCClient(c.conn).Export(ctx, req, opts...)
	default:
		return fmt.Errorf("not an OTLP export request: %T", m)
	}
	return err //nolint:wrapcheck // the caller inspects the gRPC status
}
