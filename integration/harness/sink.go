package harness

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Behaviour is how a Sink answers exports.
type Behaviour int

const (
	// BehaviourOK records the export and answers success.
	BehaviourOK Behaviour = iota
	// BehaviourPermanent answers InvalidArgument, which no exporter retries.
	BehaviourPermanent
	// BehaviourRetryable answers Unavailable with a RetryInfo delay, as a
	// throttling backend does.
	BehaviourRetryable
	// BehaviourDown closes the listener, so connections are refused.
	BehaviourDown
)

// SinkRetryDelay is the delay a retryable Sink asks the exporter to wait.
const SinkRetryDelay = 7 * time.Second

// Received is a snapshot of everything a Sink has accepted.
type Received struct {
	Traces  []ptrace.Traces
	Logs    []plog.Logs
	Metrics []pmetric.Metrics
}

// Sink is a fake OTLP/gRPC upstream on loopback, plaintext unless made with
// NewTLSSink.
type Sink struct {
	addr string
	tls  *tls.Config // nil: plaintext

	mu        sync.Mutex // guards everything below
	behaviour Behaviour
	received  Received
	server    *grpc.Server
}

// NewSink starts a sink answering BehaviourOK and stops it when the test
// ends.
func NewSink(t *testing.T) *Sink {
	t.Helper()
	return newSink(t, nil)
}

// NewTLSSink is a sink that serves TLS with cert: the collector verifies it
// only when it trusts cert, as through SSL_CERT_FILE.
func NewTLSSink(t *testing.T, cert Certificate) *Sink {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(cert.CertFile, cert.KeyFile)
	require.NoError(t, err)
	return newSink(t, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
}

func newSink(t *testing.T, tlsConfig *tls.Config) *Sink {
	t.Helper()
	s := &Sink{addr: freeAddr(t), tls: tlsConfig}
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

// Endpoint is the URL to set as OTEL_EXPORTER_OTLP_ENDPOINT.
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
		Traces:  append([]ptrace.Traces(nil), s.received.Traces...),
		Logs:    append([]plog.Logs(nil), s.received.Logs...),
		Metrics: append([]pmetric.Metrics(nil), s.received.Metrics...),
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
	var opts []grpc.ServerOption
	if s.tls != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(s.tls)))
	}
	server := grpc.NewServer(opts...)
	ptraceotlp.RegisterGRPCServer(server, &sinkTraces{sink: s})
	plogotlp.RegisterGRPCServer(server, &sinkLogs{sink: s})
	pmetricotlp.RegisterGRPCServer(server, &sinkMetrics{sink: s})
	s.mu.Lock()
	s.server = server
	s.mu.Unlock()
	go func() {
		// Serve returns when Stop is called by SetBehaviour or cleanup.
		_ = server.Serve(ln)
	}()
	return nil
}

// answer applies the current behaviour to one export and, on success, lets
// record keep it.
func (s *Sink) answer(record func(*Received)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.behaviour {
	case BehaviourOK:
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

type sinkTraces struct {
	ptraceotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkTraces) Export(_ context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	return ptraceotlp.NewExportResponse(), h.sink.answer(func(r *Received) {
		r.Traces = append(r.Traces, req.Traces())
	})
}

type sinkLogs struct {
	plogotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkLogs) Export(_ context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	return plogotlp.NewExportResponse(), h.sink.answer(func(r *Received) {
		r.Logs = append(r.Logs, req.Logs())
	})
}

type sinkMetrics struct {
	pmetricotlp.UnimplementedGRPCServer
	sink *Sink
}

func (h *sinkMetrics) Export(_ context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	return pmetricotlp.NewExportResponse(), h.sink.answer(func(r *Received) {
		r.Metrics = append(r.Metrics, req.Metrics())
	})
}
