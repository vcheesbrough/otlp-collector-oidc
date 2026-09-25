package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EnvCoverDir names the directory the instrumented binary writes coverage
// data to. Unset, the data goes to a temporary directory and is discarded.
const EnvCoverDir = "OTLP_COLLECTOR_OIDC_COVERDIR"

const (
	startTimeout = 30 * time.Second
	stopTimeout  = 20 * time.Second
)

// Options is one environment shape: the variables the process starts with
// and, optionally, a configuration other than the shipped one.
type Options struct {
	// Env is added to the variables the harness sets itself (LISTEN_ADDR,
	// HEALTH_ADDR) and may override them.
	Env map[string]string
	// ConfigYAML, when set, replaces the shipped configuration. It may use
	// ${env:LISTEN_ADDR} and ${env:HEALTH_ADDR} like the shipped one.
	ConfigYAML string
}

// Collector is one running collector process.
type Collector struct {
	// ListenAddr is the one OTLP listener, host:port.
	ListenAddr string
	// HealthAddr is the health_check endpoint, host:port.
	HealthAddr string
	// MetricsAddr is the collector's own Prometheus endpoint, host:port.
	MetricsAddr string

	cmd      *exec.Cmd
	output   *lockedBuffer
	exited   chan struct{}
	exitCode int
}

// Start runs the binary with opts, waits until its health endpoint answers,
// and stops it with SIGTERM when the test ends, asserting a clean exit.
func Start(t *testing.T, b Binary, opts Options) *Collector {
	t.Helper()

	c := &Collector{
		ListenAddr:  freeAddr(t),
		HealthAddr:  freeAddr(t),
		MetricsAddr: freeAddr(t),
		output:      &lockedBuffer{},
		exited:      make(chan struct{}),
	}
	_, metricsPort, err := net.SplitHostPort(c.MetricsAddr)
	require.NoError(t, err)

	configPath := b.ShippedConfig()
	if opts.ConfigYAML != "" {
		configPath = t.TempDir() + "/collector.yaml"
		require.NoError(t, os.WriteFile(configPath, []byte(opts.ConfigYAML), 0o600))
	}

	env := map[string]string{
		"LISTEN_ADDR": c.ListenAddr,
		"HEALTH_ADDR": c.HealthAddr,
		"GOCOVERDIR":  coverDir(t),
	}
	maps.Copy(env, opts.Env)

	// The own-metrics port is fixed in the shipped configuration; the
	// harness moves it so that shapes can run side by side.
	metricsOverride := "yaml:service::telemetry::metrics::readers: " +
		"[{pull: {exporter: {prometheus: {host: 127.0.0.1, port: " + metricsPort + "}}}}]"

	// Not CommandContext: the harness stops the process itself, with SIGTERM,
	// so that a clean shutdown is observable as exit code 0.
	// #nosec G204 -- the binary and arguments are the harness's own.
	c.cmd = exec.Command(b.Path, "--config", configPath, "--config", metricsOverride) //nolint:noctx // stopped by stop(), see above
	c.cmd.Env = envList(env)
	c.cmd.Stdout = c.output
	c.cmd.Stderr = c.output
	require.NoError(t, c.cmd.Start())

	go func() {
		defer close(c.exited)
		err := c.cmd.Wait()
		c.exitCode = exitCode(err)
	}()
	t.Cleanup(func() { c.stop(t) })

	require.Eventually(t, func() bool {
		select {
		case <-c.exited:
			return true
		default:
		}
		return c.healthy(t.Context())
	}, startTimeout, 25*time.Millisecond, "collector never became healthy")
	select {
	case <-c.exited:
		require.FailNow(t, "collector exited during start", "exit code %d\n%s", c.exitCode, c.Output())
	default:
	}
	return c
}

// PID is the process id, for inspecting what the process has open.
func (c *Collector) PID() int {
	return c.cmd.Process.Pid
}

// Output is everything the process has written to stdout and stderr so far.
func (c *Collector) Output() string {
	return c.output.String()
}

// Healthy reports whether the health endpoint answers 200.
func (c *Collector) Healthy(ctx context.Context) bool {
	return c.healthy(ctx)
}

func (c *Collector) healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.HealthAddr+"/", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *Collector) stop(t *testing.T) {
	t.Helper()
	select {
	case <-c.exited:
	default:
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.exited:
		case <-time.After(stopTimeout):
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	}
	assert.Zero(t, c.exitCode, "collector did not exit cleanly:\n%s", c.Output())
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func coverDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv(EnvCoverDir); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o750)) // #nosec G703 -- the tester names the directory
		return dir
	}
	return t.TempDir()
}

// freeAddr returns a loopback address with a port nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// lockedBuffer collects process output written from the exec goroutines
// while tests read it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffering output: %w", err)
	}
	return n, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// portOf returns the numeric port of a host:port address.
func portOf(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("splitting %q: %w", addr, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, fmt.Errorf("parsing port %q: %w", p, err)
	}
	return n, nil
}
