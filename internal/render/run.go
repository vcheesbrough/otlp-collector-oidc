package render

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	otelconf "go.opentelemetry.io/contrib/otelconf/v0.3.0"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/vcheesbrough/otlp-collector-oidc/internal/build"
	"github.com/vcheesbrough/otlp-collector-oidc/internal/upstream"
)

// RenderedFile is the rendered configuration's name in the temporary
// directory, so "what am I running" is one cat away.
const RenderedFile = "otlp-collector-oidc.yaml"

// Command is the run subcommand: the image's command. set is the collector
// as main builds it, without configuration URIs; lookup reads the
// environment and unset removes a variable from it; tempDir is where the
// rendered configuration is written.
func Command(set otelcol.CollectorSettings, lookup Lookup, unset func(string) error, tempDir string) *cobra.Command {
	return &cobra.Command{
		Use:          "run",
		Short:        "Render the configuration from the environment and run the collector on it",
		Long:         "Render the shipped pipeline from the environment variables in docs/configuration.md, or take the file COLLECTOR_CONFIG names, and run the collector on it.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), set, lookup, unset, tempDir)
		},
	}
}

// logsOnly is the group a mounted configuration still reads: the run
// command's own lines are logged as configured either way.
type logsOnly struct {
	Logs LogSettings `group:"Own logs"`
}

func run(ctx context.Context, set otelcol.CollectorSettings, lookup Lookup, unset func(string) error, tempDir string) error {
	// Everything is read before anything is acted on, so every missing or
	// malformed variable is reported at once.
	var src Source
	var s Settings
	var logs LogSettings
	errs := Load(lookup, &src)
	mounted := src.Config.CollectorConfig
	if mounted != "" {
		var l logsOnly
		errs = errors.Join(errs, Load(lookup, &l))
		logs = l.Logs
	} else {
		errs = errors.Join(errs, Load(lookup, &s))
		logs = s.Logs
	}
	if errs != nil {
		return errs
	}

	var own *Settings // nil: the run command's lines go to stdout only
	if mounted == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("choosing the instance id: %w", err)
		}
		s.InstanceID = id.String()
		own = &s
		// The OpenTelemetry SDK behind the collector's own telemetry reads
		// these itself, as defaults, and would apply them a second time and
		// differently (a base CA over a plaintext LOGS override, say). They
		// are rendered already, so nothing else may see them. A mounted
		// configuration may read them with ${env:...}, so they stay there.
		for _, name := range sdkVariables() {
			if err := unset(name); err != nil {
				return fmt.Errorf("unsetting %s: %w", name, err)
			}
		}
	}
	logger, stop, err := newLogger(ctx, logs, own)
	if err != nil {
		return err
	}
	defer stop()
	if own != nil && s.Own.ResourceAttributes.Has(serviceVersionKey) {
		logger.Warn("Ignoring service.version in OTEL_RESOURCE_ATTRIBUTES: the version is the build's",
			zap.String("version", build.Version()))
	}

	path := mounted
	if mounted != "" {
		logger.Info("Running the mounted configuration; the shipped pipeline is not rendered",
			zap.String("source", "mounted"), zap.String("path", mounted))
	} else if path, err = writeRendered(logger, &s, tempDir); err != nil {
		return err
	}
	// Rendered or mounted, the collector reads the file through the same
	// provider, as it would with --config.
	set.ConfigProviderSettings.ResolverSettings.URIs = []string{"file:" + path}
	set.ConfigProviderSettings.ResolverSettings.DefaultScheme = "env"
	col, err := otelcol.NewCollector(set)
	if err != nil {
		return fmt.Errorf("creating the collector: %w", err)
	}
	if err := col.Run(ctx); err != nil {
		if ownLogsUndelivered(col, err) {
			// The collector ran and stopped; only its last own lines could
			// not reach an unreachable upstream. Not a failure of the
			// process, and stdout has them unless LOG_OUTPUT=otlp.
			// Always on stderr: with LOG_OUTPUT=otlp the usual logger would
			// send this only to the upstream it is about.
			stderrLogger(logs).Warn("Own logs not delivered at shutdown: the logs upstream is unreachable", zap.Error(err))
			return nil
		}
		return fmt.Errorf("running the collector: %w", err)
	}
	return nil
}

// sdkVariables is every variable the SDK reads that the renderer has
// already interpreted.
func sdkVariables() []string {
	return append(upstream.Names(), "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES")
}

// ownLogsUndelivered reports whether err is the collector's shutdown failing
// only to flush its own logs, after it had run: every failure it joins is
// the service's logger. The collector marks each failure by its text alone,
// hence the prefixes, from otelcol/collector.go (shutdown) and
// service/service.go (Shutdown) at v0.161.0; a lockstep bump re-checks them,
// and TestOwnLogsUpstreamDown fails if they change.
func ownLogsUndelivered(col *otelcol.Collector, err error) bool {
	return col.GetState() == otelcol.StateClosed && onlyLoggerShutdown(err)
}

func onlyLoggerShutdown(err error) bool {
	// This node only, not errors.As: the walk reads each level's prefix, and
	// errors.As would find a join nested deeper, inside the logger's error.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyLoggerShutdown(e) {
				return false
			}
		}
		return len(joined.Unwrap()) > 0
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "failed to shutdown logger: "):
		return true
	case strings.HasPrefix(msg, "failed to shutdown service after error: "):
		if inner := errors.Unwrap(err); inner != nil {
			return onlyLoggerShutdown(inner)
		}
	}
	return false
}

// writeRendered renders s into tempDir and returns the file's path.
func writeRendered(logger *zap.Logger, s *Settings, tempDir string) (string, error) {
	yaml, err := Render(*s)
	if err != nil {
		return "", err
	}
	path := filepath.Join(tempDir, RenderedFile)
	if err := writeExclusive(path, yaml); err != nil {
		return "", fmt.Errorf("writing the rendered configuration: %w", err)
	}
	fields := append([]zap.Field{zap.String("source", "rendered"), zap.String("path", path)}, resolved(s)...)
	logger.Info("Rendered the configuration from the environment", fields...)
	// The file holds the upstream headers, which may be credentials; the
	// logged copy does not.
	shown := *s
	shown.Upstream = s.Upstream.redacted()
	if logged, err := Render(shown); err == nil {
		logger.Debug("Rendered configuration", zap.String("path", path), zap.String("yaml", string(logged)))
	}
	return path, nil
}

// writeExclusive writes data to a new file at path that only this user can
// read. The name is fixed and the directory may be shared, so whatever is
// already there is removed and the file is created with O_EXCL, which also
// refuses a symlink: an entry planted in between makes the create fail,
// refusing startup, and is never written through, read or kept.
func writeExclusive(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the previous file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the fixed name in the temporary directory
	if err != nil {
		return fmt.Errorf("creating the file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the file: %w", err)
	}
	return nil
}

// resolved is every variable in s with the value it took, default or set,
// and each signal's upstream as resolved. None of the tagged variables is
// secret; the upstream's headers are named, never valued.
func resolved(s *Settings) []zap.Field {
	var out []zap.Field
	for _, g := range groupsOf(reflect.ValueOf(s).Elem()) {
		for _, v := range g.variables {
			if v.field.IsValid() {
				out = append(out, zap.String(v.name, display(v.field)))
			}
		}
	}
	return append(out, s.Upstream.logFields()...)
}

func display(v reflect.Value) string {
	if list, ok := v.Interface().([]string); ok {
		return strings.Join(list, ",")
	}
	return fmt.Sprint(v.Interface())
}

// newLogger is the command's own logger, encoded like the collector's so
// that the two read as one stream. With own set, its lines also follow
// LOG_OUTPUT: exported through the same processors and resource the
// collector is rendered with, and kept off stdout for otlp. stop flushes
// the export, briefly: an unreachable upstream never holds the exit.
func newLogger(ctx context.Context, s LogSettings, own *Settings) (*zap.Logger, func(), error) {
	logger, err := stdoutLogger(s)
	if err != nil {
		return nil, nil, err
	}
	sync := func() { _ = logger.Sync() } // stdout may not support fsync
	if own == nil || !own.Own.Output.Upstream() {
		return logger, sync, nil
	}
	cfg, err := own.sdkConfig()
	if err != nil {
		return nil, nil, err
	}
	sdk, err := otelconf.NewSDK(otelconf.WithContext(ctx), otelconf.WithOpenTelemetryConfiguration(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("building the own-logs exporter: %w", err)
	}
	level := logger.Level()
	otlp, err := zapcore.NewIncreaseLevelCore(otelzap.NewCore(build.Command, otelzap.WithLoggerProvider(sdk.LoggerProvider())), level)
	if err != nil {
		return nil, nil, fmt.Errorf("building the own-logs core: %w", err)
	}
	logger = logger.WithOptions(zap.WrapCore(func(stdout zapcore.Core) zapcore.Core {
		if own.Own.Output.Stdout() {
			return zapcore.NewTee(stdout, otlp)
		}
		return otlp
	}))
	return logger, func() {
		sync()
		// The collector's own shutdown has the same bound on its exporter.
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := sdk.Shutdown(flush); err != nil {
			stderrLogger(s).Warn("The run command's own lines not delivered at exit", zap.Error(err))
		}
	}, nil
}

// stderrLogger is the command's logger on stderr alone, for what must be seen
// whatever LOG_OUTPUT says. Built on demand; a build failure yields a no-op.
func stderrLogger(s LogSettings) *zap.Logger {
	logger, err := buildLogger(s, "stderr")
	if err != nil {
		return zap.NewNop()
	}
	return logger
}

func stdoutLogger(s LogSettings) (*zap.Logger, error) {
	return buildLogger(s, "stdout")
}

func buildLogger(s LogSettings, output string) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(string(s.Level))
	if err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.Encoding = string(s.Format)
	cfg.Sampling = nil
	cfg.OutputPaths = []string{output}
	cfg.ErrorOutputPaths = []string{"stderr"}
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("building the logger: %w", err)
	}
	return logger, nil
}
