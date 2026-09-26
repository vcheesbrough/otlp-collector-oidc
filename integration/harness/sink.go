package harness

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Behaviour is how a Sink answers exports.
type Behaviour int

const (
	// BehaviourOK records the export and answers success.
	BehaviourOK Behaviour = iota
	// BehaviourPermanent answers InvalidArgument (HTTP 400), which no
	// exporter retries.
	BehaviourPermanent
	// BehaviourRetryable answers Unavailable (HTTP 503) with a retry delay,
	// as a throttling backend does.
	BehaviourRetryable
	// BehaviourDown closes the listener, so connections are refused.
	BehaviourDown
)

// SinkRetryDelay is the delay a retryable Sink asks the exporter to wait.
const SinkRetryDelay = 7 * time.Second

// Transport is how a Sink receives OTLP.
type Transport int

const (
	// TransportGRPC serves the three OTLP/gRPC services.
	TransportGRPC Transport = iota
	// TransportHTTP serves OTLP/HTTP protobuf on any path ending in
	// /v1/<signal>, so a backend's own prefix (Loki's /otlp) works too.
	TransportHTTP
)

// SinkOptions shape a Sink beyond NewSink's plaintext gRPC.
type SinkOptions struct {
	Transport Transport
	// Cert, when set, is served over TLS: the collector verifies it only
	// when it trusts it.
	Cert *Certificate
	// ClientCAs, when set with Cert, is the only pool a client certificate
	// is accepted from, and one is required: mTLS.
	ClientCAs *x509.CertPool
}

// Request is what a Sink saw of one export besides its payload.
type Request struct {
	// Path is the HTTP path; over gRPC, the signal's name.
	Path string
	// Header is the HTTP headers, or the gRPC metadata, keys lower-cased.
	Header map[string][]string
}

// Received is a snapshot of everything a Sink has accepted.
type Received struct {
	Traces   []ptrace.Traces
	Logs     []plog.Logs
	Metrics  []pmetric.Metrics
	Requests []Request
}

// stopper is a running server, gRPC or HTTP.
type stopper interface {
	Stop()
}

// Sink is a fake OTLP upstream on loopback.
type Sink struct {
	addr      string
	transport Transport
	tls       *tls.Config // nil: plaintext

	mu        sync.Mutex // guards everything below
	behaviour Behaviour
	received  Received
	server    stopper
}

// NewSink starts a plaintext gRPC sink answering BehaviourOK and stops it
// when the test ends.
func NewSink(t *testing.T) *Sink {
	t.Helper()
	return NewSinkWith(t, SinkOptions{})
}

// NewTLSSink is a gRPC sink that serves TLS with cert.
func NewTLSSink(t *testing.T, cert Certificate) *Sink {
	t.Helper()
	return NewSinkWith(t, SinkOptions{Cert: &cert})
}

// NewSinkWith starts a sink shaped by opts.
func NewSinkWith(t *testing.T, opts SinkOptions) *Sink {
	t.Helper()
	s := &Sink{addr: freeAddr(t), transport: opts.Transport}
	if opts.Cert != nil {
		pair, err := tls.LoadX509KeyPair(opts.Cert.CertFile, opts.Cert.KeyFile)
		require.NoError(t, err)
		s.tls = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		if opts.ClientCAs != nil {
			s.tls.ClientCAs = opts.ClientCAs
			s.tls.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	require.NoError(t, s.serve(t.Context()))
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.server != nil {
			s.server.Stop()
		}
	})
	return s
}

// Endpoint is the URL to set as OTEL_EXPORTER_OTLP_ENDPOINT: for HTTP, a base
// the exporter appends /v1/<signal> to.
func (s *Sink) Endpoint() string {
	if s.tls != nil {
		return "https://" + s.addr
	}
	return "http://" + s.addr
}

// Received returns what the sink has accepted so far.
func (s *Sink) Received() Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Received{
		Traces:   append([]ptrace.Traces(nil), s.received.Traces...),
		Logs:     append([]plog.Logs(nil), s.received.Logs...),
		Metrics:  append([]pmetric.Metrics(nil), s.received.Metrics...),
		Requests: append([]Request(nil), s.received.Requests...),
	}
}

// SetBehaviour changes how the sink answers from now on. Leaving
// BehaviourDown listens again on the same address.
func (s *Sink) SetBehaviour(t *testing.T, b Behaviour) {
	t.Helper()
	s.mu.Lock()
	was := s.behaviour
	s.behaviour = b
	server := s.server
	if b == BehaviourDown {
		s.server = nil
	}
	s.mu.Unlock()

	switch {
	case b == BehaviourDown && server != nil:
		server.Stop()
	case b != BehaviourDown && was == BehaviourDown:
		require.NoError(t, s.serve(t.Context()))
	}
}

func (s *Sink) serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("sink listen: %w", err)
	}
	var server stopper
	switch s.transport {
	case TransportGRPC:
		server = s.serveGRPC(ln)
	case TransportHTTP:
		server = s.serveHTTP(ln)
	}
	s.mu.Lock()
	s.server = server
	s.mu.Unlock()
	return nil
}

func (s *Sink) serveGRPC(ln net.Listener) stopper {
	var opts []grpc.ServerOption
	if s.tls != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(s.tls)))
	}
	server := grpc.NewServer(opts...)
	ptraceotlp.RegisterGRPCServer(server, &sinkTraces{sink: s})
	plogotlp.RegisterGRPCServer(server, &sinkLogs{sink: s})
	pmetricotlp.RegisterGRPCServer(server, &sinkMetrics{sink: s})
	go func() {
		// Serve returns when Stop is called by SetBehaviour or cleanup.
		_ = server.Serve(ln)
	}()
	return server
}

// httpServer is an *http.Server stopped the way a gRPC one is.
type httpServer struct{ *http.Server }

func (h httpServer) Stop() { _ = h.Close() }

func (s *Sink) serveHTTP(ln net.Listener) stopper {
	server := &http.Server{Handler: http.HandlerFunc(s.handleHTTP), ReadHeaderTimeout: 5 * time.Second}
	if s.tls != nil {
		server.TLSConfig = s.tls
		ln = tls.NewListener(ln, s.tls)
	}
	go func() {
		// Serve returns when Close is called by SetBehaviour or cleanup.
		_ = server.Serve(ln)
	}()
	return httpServer{server}
}

func (s *Sink) handleHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := Request{Path: r.URL.Path, Header: lowerKeys(r.Header)}
	var record func(*Received)
	var response interface{ MarshalProto() ([]byte, error) }
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/traces"):
		msg := ptraceotlp.NewExportRequest()
		err = msg.UnmarshalProto(body)
		record = func(rc *Received) { rc.Traces = append(rc.Traces, msg.Traces()) }
		response = ptraceotlp.NewExportResponse()
	case strings.HasSuffix(r.URL.Path, "/v1/logs"):
		msg := plogotlp.NewExportRequest()
		err = msg.UnmarshalProto(body)
		record = func(rc *Received) { rc.Logs = append(rc.Logs, msg.Logs()) }
		response = plogotlp.NewExportResponse()
	case strings.HasSuffix(r.URL.Path, "/v1/metrics"):
		msg := pmetricotlp.NewExportRequest()
		err = msg.UnmarshalProto(body)
		record = func(rc *Received) { rc.Metrics = append(rc.Metrics, msg.Metrics()) }
		response = pmetricotlp.NewExportResponse()
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "undecodable export: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.answer(req, record); err != nil {
		st, _ := status.FromError(err)
		if st.Code() == codes.Unavailable {
			w.Header().Set("Retry-After", strconv.Itoa(int(SinkRetryDelay/time.Second)))
			http.Error(w, st.Message(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, st.Message(), http.StatusBadRequest)
		return
	}
	out, err := response.MarshalProto()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

// readBody is the request body, gunzipped when the exporter compressed it.
func readBody(r *http.Request) ([]byte, error) {
	var body io.Reader = r.Body
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = zr.Close() }()
		body = zr
	default:
		return nil, errors.New("unsupported Content-Encoding " + enc)
	}
	b, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the body: %w", err)
	}
	return b, nil
}

func lowerKeys(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = append([]string(nil), v...)
	}
	return out
}

// answer applies the current behaviour to one export and, on success, keeps
// the request and lets record keep its payload.
func (s *Sink) answer(req Request, record func(*Received)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.behaviour {
	case BehaviourOK:
		s.received.Requests = append(s.received.Requests, req)
		record(&s.received)
		return nil
	case BehaviourPermanent:
		return status.Error(codes.InvalidArgument, "sink told to fail permanently")
	case BehaviourRetryable:
		st, err := status.New(codes.Unavailable, "sink told to fail retryably").
			WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(SinkRetryDelay)})
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		return st.Err()
	case BehaviourDown:
		// Unreachable while the listener is closed; answer as a dying server.
		return status.Error(codes.Unavailable, "sink is down")
	default:
		return status.Errorf(codes.Internal, "unknown sink behaviour %d", s.behaviour)
	}
}

// grpcRequest is what the sink records of a gRPC export.
func grpcRequest(ctx context.Context, method string) Request {
	md, _ := metadata.FromIncomingContext(ctx)
	return Request{Path: method, Header: lowerKeys(md)}
}

type sinkTraces struct {
	ptraceotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkTraces) Export(ctx context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	return ptraceotlp.NewExportResponse(), h.sink.answer(grpcRequest(ctx, "traces"), func(r *Received) {
		r.Traces = append(r.Traces, req.Traces())
	})
}

type sinkLogs struct {
	plogotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkLogs) Export(ctx context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	return plogotlp.NewExportResponse(), h.sink.answer(grpcRequest(ctx, "logs"), func(r *Received) {
		r.Logs = append(r.Logs, req.Logs())
	})
}

type sinkMetrics struct {
	pmetricotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkMetrics) Export(ctx context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	return pmetricotlp.NewExportResponse(), h.sink.answer(grpcRequest(ctx, "metrics"), func(r *Received) {
		r.Metrics = append(r.Metrics, req.Metrics())
	})
}
