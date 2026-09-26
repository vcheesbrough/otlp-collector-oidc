package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TraefikImage is the proxy the behind-Traefik tier runs, pinned.
const TraefikImage = "traefik:v3.7.13"

// Traefik is a real Traefik container in front of a collector, configured as
// docs/proxies/traefik.md says: TLS terminated with its own certificate, one
// service on the collector's port over HTTPS accepting the container's
// certificate, HTTP/2 to it, the OTLP/HTTP and gRPC paths routed, rate
// limited by source address.
type Traefik struct {
	addr   string
	prefix string
	cert   Certificate
}

var _ Frontend = (*Traefik)(nil)

// StartTraefik runs Traefik in front of the collector listening at backend.
// With prefix empty, one router takes /v1/ and the gRPC paths; with a
// prefix such as /otlp, OTLP/HTTP is mounted under it and stripped, and the
// gRPC router is separate and never stripped. The container shares the
// host's network, so it reaches the collector on loopback; it is removed
// when the test ends.
func StartTraefik(t *testing.T, backend, prefix string) *Traefik {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		require.FailNow(t, "the behind-Traefik tier needs docker", "%v", err)
	}
	tr := &Traefik{addr: freeAddr(t), prefix: prefix, cert: NewCertificate(t)}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.yml"), []byte(tr.dynamicConfig(backend)), 0o600))
	for _, f := range []struct{ from, to string }{{tr.cert.CertFile, "cert.pem"}, {tr.cert.KeyFile, "key.pem"}} {
		b, err := os.ReadFile(f.from) // #nosec G304 -- the test's own certificate
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, f.to), b, 0o644)) // #nosec G306 G703 -- the harness's own fixed names, read by the container's user
	}
	require.NoError(t, os.Chmod(dir, 0o755)) // #nosec G302 -- listed by the container's user

	name := fmt.Sprintf("otlp-collector-oidc-traefik-%d", time.Now().UnixNano())
	// #nosec G204 -- the harness's own arguments
	run := exec.CommandContext(t.Context(), "docker", "run", "-d", "--rm", "--name", name,
		"--network", "host", "-v", dir+":/etc/traefik/dynamic:ro", TraefikImage,
		"--entrypoints.otlp.address="+tr.addr,
		"--entrypoints.otlp.http.tls=true",
		"--providers.file.directory=/etc/traefik/dynamic",
		"--log.level=ERROR")
	out, err := run.CombinedOutput()
	require.NoError(t, err, "docker run: %s", out)
	t.Cleanup(func() {
		// #nosec G204 -- the harness's own container
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "rm", "-f", name).Run()
	})

	// Ready once it answers TLS with its own certificate; the router may
	// still be loading, so wait for the collector's own answer to a GET.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: tr.cert.Pool, MinVersion: tls.VersionTLS12},
	}}
	routed := func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tr.BaseURL()+SignalTraces.Path(), http.NoBody)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode != http.StatusNotFound && resp.StatusCode < 500
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.Now().Add(60 * time.Second); !routed(); <-tick.C {
		if time.Now().After(deadline) {
			// The logs are read now, when they say why.
			require.FailNow(t, "Traefik never routed to the collector", "%s", logs(t, name))
		}
	}
	return tr
}

// dynamicConfig is Traefik's file-provider configuration, the same shape as
// the label block in examples/compose-behind-traefik.
func (tr *Traefik) dynamicConfig(backend string) string {
	var routers string
	if tr.prefix == "" {
		routers = `
    otlp:
      rule: "PathPrefix(` + "`/v1/`" + `) || PathPrefix(` + "`/opentelemetry.proto.collector`" + `)"
      entryPoints: [otlp]
      service: collector
      middlewares: [rate-limit]
      tls: {}`
	} else {
		routers = `
    otlp-http:
      rule: "PathPrefix(` + "`" + tr.prefix + "/v1/`" + `)"
      entryPoints: [otlp]
      service: collector
      middlewares: [otlp-strip, rate-limit]
      tls: {}
    otlp-grpc:
      rule: "PathPrefix(` + "`/opentelemetry.proto.collector`" + `)"
      entryPoints: [otlp]
      service: collector
      middlewares: [rate-limit]
      tls: {}`
	}
	return `tls:
  certificates:
    - certFile: /etc/traefik/dynamic/cert.pem
      keyFile: /etc/traefik/dynamic/key.pem
  stores:
    default:
      defaultCertificate:
        certFile: /etc/traefik/dynamic/cert.pem
        keyFile: /etc/traefik/dynamic/key.pem
http:
  routers:` + routers + `
  middlewares:
    otlp-strip:
      stripPrefix:
        prefixes: ["` + tr.prefix + `"]
    rate-limit:
      rateLimit:
        average: 1000
        burst: 2000
  serversTransports:
    collector:
      # The container's certificate is self-signed per image build and
      # identifies nothing; the hop is inside the deployment's network.
      insecureSkipVerify: true
  services:
    collector:
      loadBalancer:
        serversTransport: collector
        servers:
          - url: "https://` + backend + `"
`
}

// BaseURL is Traefik's address, with the mount prefix when there is one.
func (tr *Traefik) BaseURL() string { return "https://" + tr.addr + tr.prefix }

// GRPCTarget is Traefik's address: gRPC method paths are never prefixed.
func (tr *Traefik) GRPCTarget() string { return tr.addr }

// Pool trusts Traefik's certificate.
func (tr *Traefik) Pool() *x509.CertPool { return tr.cert.Pool }

func logs(t *testing.T, name string) string {
	t.Helper()
	// #nosec G204 -- the harness's own container
	out, _ := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "logs", name).CombinedOutput()
	return strings.TrimSpace(string(out))
}
