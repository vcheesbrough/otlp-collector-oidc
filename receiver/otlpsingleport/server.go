package otlpsingleport

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// OTLP/HTTP paths, fixed by the OTLP specification.
const (
	tracesPath  = "/v1/traces"
	logsPath    = "/v1/logs"
	metricsPath = "/v1/metrics"
)

// singlePortSettings is what the factory hands a singlePort: the listener's
// configuration and every collaborator it would otherwise have to build.
type singlePortSettings struct {
	server       confighttp.ServerConfig
	telemetry    component.TelemetrySettings
	grpcServer   *grpc.Server
	errorHandler func(w http.ResponseWriter, r *http.Request, msg string, httpStatus int)
	obsGRPC      *receiverhelper.ObsReport
	obsHTTP      *receiverhelper.ObsReport
}

// singlePort owns the one listener, the gRPC server inside it and the table
// of OTLP/HTTP routes. Per-signal state lives in the signal exporters.
type singlePort struct {
	settings singlePortSettings

	// routes maps an OTLP/HTTP path to its signal's handler. It is written
	// only while pipelines register, before Start, and read only after.
	routes map[string]http.Handler

	httpServer *http.Server
	serveWG    sync.WaitGroup
}

func newSinglePort(set singlePortSettings) *singlePort {
	return &singlePort{settings: set, routes: map[string]http.Handler{}}
}

func (s *singlePort) registerTraces(next consumer.Traces) {
	ptraceotlp.RegisterGRPCServer(s.settings.grpcServer, &tracesExporter{next: next, obs: s.settings.obsGRPC})
	h := &tracesExporter{next: next, obs: s.settings.obsHTTP}
	s.routes[tracesPath] = exportHandler(ptraceotlp.NewExportRequest, h.Export)
}

func (s *singlePort) registerLogs(next consumer.Logs) {
	plogotlp.RegisterGRPCServer(s.settings.grpcServer, &logsExporter{next: next, obs: s.settings.obsGRPC})
	h := &logsExporter{next: next, obs: s.settings.obsHTTP}
	s.routes[logsPath] = exportHandler(plogotlp.NewExportRequest, h.Export)
}

func (s *singlePort) registerMetrics(next consumer.Metrics) {
	pmetricotlp.RegisterGRPCServer(s.settings.grpcServer, &metricsExporter{next: next, obs: s.settings.obsGRPC})
	h := &metricsExporter{next: next, obs: s.settings.obsHTTP}
	s.routes[metricsPath] = exportHandler(pmetricotlp.NewExportRequest, h.Export)
}

// ServeHTTP dispatches one request: gRPC to the gRPC server, a registered
// OTLP/HTTP path to its signal's handler, anything else 404. A signal with no
// pipeline has no route, so it answers 404 over HTTP and Unimplemented over
// gRPC.
func (s *singlePort) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isGRPC(r) {
		s.settings.grpcServer.ServeHTTP(w, r)
		return
	}
	if h, ok := s.routes[r.URL.Path]; ok {
		h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// Start opens the listener and serves on it until Shutdown.
func (s *singlePort) Start(ctx context.Context, host component.Host) error {
	srv, err := s.settings.server.ToServer(ctx, host.GetExtensions(), s.settings.telemetry, s,
		confighttp.WithErrorHandler(s.settings.errorHandler))
	if err != nil {
		return err
	}
	ln, err := s.settings.server.ToListener(ctx)
	if err != nil {
		return err
	}
	s.httpServer = srv
	s.settings.telemetry.Logger.Info("Starting OTLP single-port server",
		zap.String("endpoint", ln.Addr().String()))

	s.serveWG.Go(func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(serveErr))
		}
	})
	return nil
}

// Shutdown stops accepting, lets in-flight requests finish within ctx, and
// then releases the gRPC server.
func (s *singlePort) Shutdown(ctx context.Context) error {
	var err error
	if s.httpServer != nil {
		err = s.httpServer.Shutdown(ctx)
	}
	s.settings.grpcServer.Stop()
	s.serveWG.Wait()
	return err
}
