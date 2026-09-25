package otlpsingleport

import (
	"context"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"

	"github.com/vcheesbrough/otlp-collector-oidc/receiver/otlpsingleport/internal/metadata"
)

// NewFactory returns the factory for the otlpsingleport receiver.
//
// A configuration used by several pipelines yields one receiver, so the
// traces, logs and metrics pipelines share the one listener. The registry of
// those shared receivers belongs to this factory rather than the package.
func NewFactory() receiver.Factory {
	r := &registry{receivers: map[*Config]*sharedReceiver{}}
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithTraces(r.createTraces, metadata.TracesStability),
		receiver.WithLogs(r.createLogs, metadata.LogsStability),
		receiver.WithMetrics(r.createMetrics, metadata.MetricsStability),
	)
}

// registry holds the receiver shared by every pipeline that names one
// configuration.
type registry struct {
	mu        sync.Mutex
	receivers map[*Config]*sharedReceiver
}

func (r *registry) createTraces(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Traces) (receiver.Traces, error) {
	sr, err := r.load(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	sr.receiver.registerTraces(next)
	return sr, nil
}

func (r *registry) createLogs(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
	sr, err := r.load(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	sr.receiver.registerLogs(next)
	return sr, nil
}

func (r *registry) createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	sr, err := r.load(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	sr.receiver.registerMetrics(next)
	return sr, nil
}

// load returns the receiver for cfg, building it on first use.
func (r *registry) load(set receiver.Settings, cfg *Config) (*sharedReceiver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sr, ok := r.receivers[cfg]; ok {
		return sr, nil
	}
	rcv, err := newReceiver(set, cfg)
	if err != nil {
		return nil, err
	}
	sr := &sharedReceiver{receiver: rcv, release: func() { r.release(cfg) }}
	r.receivers[cfg] = sr
	return sr, nil
}

func (r *registry) release(cfg *Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.receivers, cfg)
}

// newReceiver wires one receiver: its gRPC server, the error handler the
// listener uses, and an ObsReport per transport.
func newReceiver(set receiver.Settings, cfg *Config) (*singlePort, error) {
	obsGRPC, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             set.ID,
		Transport:              "grpc",
		ReceiverCreateSettings: set,
	})
	if err != nil {
		return nil, err
	}
	obsHTTP, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             set.ID,
		Transport:              "http",
		ReceiverCreateSettings: set,
	})
	if err != nil {
		return nil, err
	}
	return newSinglePort(singlePortSettings{
		server:       cfg.ServerConfig,
		telemetry:    set.TelemetrySettings,
		grpcServer:   newGRPCServer(cfg.ServerConfig.MaxRequestBodySize),
		errorHandler: writeHandedStatus,
		obsGRPC:      obsGRPC,
		obsHTTP:      obsHTTP,
	}), nil
}

// sharedReceiver starts the receiver once however many pipelines use it, and
// stops it once when the first of them shuts it down.
type sharedReceiver struct {
	receiver *singlePort
	release  func()

	startOnce sync.Once
	startErr  error
	stopOnce  sync.Once
	stopErr   error
}

var (
	_ receiver.Traces  = (*sharedReceiver)(nil)
	_ receiver.Logs    = (*sharedReceiver)(nil)
	_ receiver.Metrics = (*sharedReceiver)(nil)
)

// Start starts the listener on the first call and returns that result after.
func (s *sharedReceiver) Start(ctx context.Context, host component.Host) error {
	s.startOnce.Do(func() {
		s.startErr = s.receiver.Start(ctx, host)
	})
	return s.startErr
}

// Shutdown stops the listener on the first call and returns that result after.
func (s *sharedReceiver) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.stopErr = s.receiver.Shutdown(ctx)
		s.release()
	})
	return s.stopErr
}
