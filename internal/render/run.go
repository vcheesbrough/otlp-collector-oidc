package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/collector/otelcol"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// RenderedFile is the rendered configuration's name in the temporary
// directory, so "what am I running" is one cat away.
const RenderedFile = "otlp-collector-oidc.yaml"

// Command is the run subcommand: the image's command. set is the collector
// as main builds it, without configuration URIs; lookup reads the
// environment; tempDir is where the rendered configuration is written.
func Command(set otelcol.CollectorSettings, lookup Lookup, tempDir string) *cobra.Command {
	return &cobra.Command{
		Use:          "run",
		Short:        "Render the configuration from the environment and run the collector on it",
		Long:         "Render the shipped pipeline from the environment variables in docs/configuration.md, or take the file COLLECTOR_CONFIG names, and run the collector on it.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), set, lookup, tempDir)
		},
	}
}

// logsOnly is the one group read before anything else, so that choosing
// the configuration is itself logged as configured.
type logsOnly struct {
	Logs LogSettings `group:"Own logs"`
}

func run(ctx context.Context, set otelcol.CollectorSettings, lookup Lookup, tempDir string) error {
	var logs logsOnly
	if err := Load(lookup, &logs); err != nil {
		return err
	}
	logger, err := newLogger(logs.Logs)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }() // stdout may not support fsync

	path, err := configFile(logger, lookup, tempDir)
	if err != nil {
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
		return fmt.Errorf("running the collector: %w", err)
	}
	return nil
}

// configFile is the path of the configuration to run: COLLECTOR_CONFIG when
// set, otherwise the shipped pipeline rendered from the environment into
// tempDir.
func configFile(logger *zap.Logger, lookup Lookup, tempDir string) (string, error) {
	var src Source
	if err := Load(lookup, &src); err != nil {
		return "", err
	}
	if mounted := src.Config.CollectorConfig; mounted != "" {
		logger.Info("Running the mounted configuration; the shipped pipeline is not rendered",
			zap.String("source", "mounted"), zap.String("path", mounted))
		return mounted, nil
	}

	var s Settings
	if err := Load(lookup, &s); err != nil {
		return "", err
	}
	yaml, err := Render(s)
	if err != nil {
		return "", err
	}
	path := filepath.Join(tempDir, RenderedFile)
	if err := os.WriteFile(path, yaml, 0o600); err != nil {
		return "", fmt.Errorf("writing the rendered configuration: %w", err)
	}
	fields := append([]zap.Field{zap.String("source", "rendered"), zap.String("path", path)}, resolved(&s)...)
	logger.Info("Rendered the configuration from the environment", fields...)
	logger.Debug("Rendered configuration", zap.String("path", path), zap.String("yaml", string(yaml)))
	return path, nil
}

// resolved is every variable in s with the value it took, default or set.
// None of the variables rendered today is secret; one that is must be left
// out here.
func resolved(s *Settings) []zap.Field {
	var out []zap.Field
	for _, g := range groupsOf(reflect.ValueOf(s).Elem()) {
		for _, v := range g.variables {
			out = append(out, zap.String(v.name, display(v.field)))
		}
	}
	return out
}

func display(v reflect.Value) string {
	if list, ok := v.Interface().([]string); ok {
		return strings.Join(list, ",")
	}
	return fmt.Sprint(v.Interface())
}

// newLogger is the command's own logger, encoded like the collector's so
// that the two read as one stream.
func newLogger(s LogSettings) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(string(s.Level))
	if err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.Encoding = string(s.Format)
	cfg.Sampling = nil
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("building the logger: %w", err)
	}
	return logger, nil
}
