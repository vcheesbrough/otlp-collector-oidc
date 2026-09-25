package otlpsingleport

import (
	"errors"
	"math"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
)

// defaultMaxRequestBodySize bounds the decompressed body of one request.
const defaultMaxRequestBodySize = 4 << 20 // 4 MiB

// Config is the YAML shape of the receiver: one confighttp server whose
// listener carries both OTLP/gRPC and OTLP/HTTP.
type Config struct {
	// ServerConfig is the one listener. Its TLS, authenticator, body cap,
	// decompression and CORS settings apply to both protocols.
	ServerConfig confighttp.ServerConfig `mapstructure:",squash"`

	// prevent unkeyed literal initialization
	_ struct{}
}

var _ component.Config = (*Config)(nil)

// ErrNoEndpoint is returned by Validate when no listen address is set.
var ErrNoEndpoint = errors.New("endpoint must be set")

// ErrBodySizeOutOfRange is returned by Validate when max_request_body_size is
// not a positive number that fits the gRPC message-size limit.
var ErrBodySizeOutOfRange = errors.New("max_request_body_size must be between 1 and 2147483647")

// Validate fails fast on a configuration the receiver cannot serve.
func (c *Config) Validate() error {
	if c.ServerConfig.NetAddr.Endpoint == "" {
		return ErrNoEndpoint
	}
	// The same cap bounds a gRPC message, whose limit is an int32.
	if c.ServerConfig.MaxRequestBodySize <= 0 || c.ServerConfig.MaxRequestBodySize > math.MaxInt32 {
		return ErrBodySizeOutOfRange
	}
	return nil
}

func createDefaultConfig() component.Config {
	server := confighttp.NewDefaultServerConfig()
	server.NetAddr.Endpoint = "localhost:4318"
	server.MaxRequestBodySize = defaultMaxRequestBodySize
	return &Config{ServerConfig: server}
}
