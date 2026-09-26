package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// HEALTH_ADDR, SELF_METRICS_ADDR) and may override them. Nothing else
	// from the test's own environment reaches the process.
	Env map[string]string
	// ConfigYAML, when set, is mounted as COLLECTOR_CONFIG in place of the
	// shipped pipeline. Besides the product's variables it may read
	// ${env:HARNESS_METRICS_HOST} and ${env:HARNESS_METRICS_PORT}, the
	// own-metrics address split as the Prometheus reader wants it.
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
	stdout   *lockedBuffer
	stderr   *lockedBuffer
	exited   chan struct{}
	exitCode int
}

// Start runs the binary with opts, waits until its health endpoint answers,
// and stops it with SIGTERM when the test ends, asserting a clean exit.
func Start(t *testing.T, b Binary, opts Options) *Collector {
	t.Helper()
	for attempt := 1; ; attempt++ {
		c := launch(t, b, opts)
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
			// freeAddr releases the ports it finds, so a process started in
			// parallel can take one first: try again on fresh ports.
			if attempt < startAttempts && strings.Contains(c.Output(), "address already in use") {
				continue
			}
			require.FailNow(t, "collector exited during start", "exit code %d\n%s", c.exitCode, c.Output())
		default:
		}
		t.Cleanup(func() { c.stop(t) })
		return c
	}
}

// startAttempts bounds the relaunches after a port race.
const startAttempts = 3

// Refused is a collector that would not start: its exit code, everything it
// wrote, and what it wrote to stderr alone.
type Refused struct {
	ExitCode int
	Output   string
	Stderr   string
}

// RunToExit runs the binary with opts for a shape that must refuse to start,
// and returns once it exits. It fails the test if the process becomes healthy
// or outlives startTimeout.
func RunToExit(t *testing.T, b Binary, opts Options) Refused {
	t.Helper()
	c := launch(t, b, opts)
	deadline := time.After(startTimeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.exited:
			return Refused{ExitCode: c.exitCode, Output: c.Output(), Stderr: c.stderr.String()}
		case <-deadline:
			_ = c.cmd.Process.Kill()
			<-c.exited
			require.FailNow(t, "collector did not exit", "%s", c.Output())
		case <-tick.C:
			if c.healthy(t.Context()) {
				c.stop(t)
				require.FailNow(t, "collector started, want a refusal", "%s", c.Output())
			}
		}
	}
}

// launch starts the process for opts without waiting for anything.
func launch(t *testing.T, b Binary, opts Options) *Collector {
	t.Helper()
	c := &Collector{
		ListenAddr:  freeAddr(t),
		HealthAddr:  freeAddr(t),
		MetricsAddr: freeAddr(t),
		output:      &lockedBuffer{},
		stdout:      &lockedBuffer{},
		stderr:      &lockedBuffer{},
		exited:      make(chan struct{}),
	}
	metricsHost, metricsPort, err := net.SplitHostPort(c.MetricsAddr)
	require.NoError(t, err)

	env := map[string]string{
		"LISTEN_ADDR":       c.ListenAddr,
		"HEALTH_ADDR":       c.HealthAddr,
		"SELF_METRICS_ADDR": c.MetricsAddr,
		// The rendered configuration goes to the temporary directory; one
		// per process, so shapes running side by side do not share it.
		"TMPDIR":     t.TempDir(),
		"GOCOVERDIR": coverDir(t),
	}
	if opts.ConfigYAML != "" {
		path := filepath.Join(t.TempDir(), "collector.yaml")
		require.NoError(t, os.WriteFile(path, []byte(opts.ConfigYAML), 0o600))
		env["COLLECTOR_CONFIG"] = path
		env["HARNESS_METRICS_HOST"] = metricsHost
		env["HARNESS_METRICS_PORT"] = metricsPort
	}
	maps.Copy(env, opts.Env)

	// Not CommandContext: the harness stops the process itself, with SIGTERM,
	// so that a clean shutdown is observable as exit code 0.
	// #nosec G204 -- the binary and arguments are the harness's own.
	c.cmd = exec.Command(b.Path, "run") //nolint:noctx // stopped by stop(), see above
	c.cmd.Env = envList(env)
	c.cmd.Stdout = io.MultiWriter(c.output, c.stdout)
	c.cmd.Stderr = io.MultiWriter(c.output, c.stderr)
	require.NoError(t, c.cmd.Start())

	go func() {
		defer close(c.exited)
		err := c.cmd.Wait()
		c.exitCode = exitCode(err)
	}()
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

// Stderr is what the process has written to stderr alone.
func (c *Collector) Stderr() string {
	return c.stderr.String()
}

// Stop sends SIGTERM now, rather than when the test ends, waits for the exit
// and asserts it was clean. Stopping twice is harmless.
func (c *Collector) Stop(t *testing.T) {
	t.Helper()
	c.stop(t)
}

// Stdout is what the process has written to stdout alone: its log lines.
func (c *Collector) Stdout() string {
	return c.stdout.String()
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

// handedOut is every port freeAddr has returned in this test binary. The
// port is released before the process binds it, and the kernel readily
// offers a just-released port again, so without this two processes started
// in parallel could be given the same one. Test-harness state, shared by
// every parallel test by design.
var handedOut sync.Map

// freeAddr returns a loopback address with a port nothing is listening on
// and no other caller in this run has been given.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	for {
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())
		if _, taken := handedOut.LoadOrStore(addr, true); !taken {
			return addr
		}
	}
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
