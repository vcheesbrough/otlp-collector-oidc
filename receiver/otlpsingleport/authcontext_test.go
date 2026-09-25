package otlpsingleport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension/extensionauth"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/vcheesbrough/otlp-collector-oidc/receiver/otlpsingleport/internal/metadata"
)

// TestAuthContextReachesHandlers proves that what the authenticator puts in
// the request context reaches the pipeline over both transports, including
// gRPC run through grpc.Server.ServeHTTP (DESIGN §10). It is a unit test
// because nothing in the shipped pipeline reads the auth context yet, so it
// cannot be observed from outside the process; the identity-stamping card
// makes it a sink observation.
func TestAuthContextReachesHandlers(t *testing.T) {
	authID := component.MustNewID("fakeauth")
	certFile, keyFile, pool := testCertificate(t)
	addr := freeAddr(t)
	cfg := createDefaultConfig().(*Config)
	cfg.ServerConfig.NetAddr.Endpoint = addr
	cfg.ServerConfig.TLS = configoptional.Some(configtls.ServerConfig{Config: configtls.Config{
		CertFile: certFile,
		KeyFile:  keyFile,
	}})
	cfg.ServerConfig.Auth = configoptional.Some(confighttp.AuthConfig{
		Config: configauth.Config{AuthenticatorID: authID},
	})

	got := make(chan client.AuthData, 2)
	next, err := consumer.NewTraces(func(ctx context.Context, _ ptrace.Traces) error {
		got <- client.FromContext(ctx).Auth
		return nil
	})
	require.NoError(t, err)
	rcv, err := NewFactory().CreateTraces(t.Context(), receivertest.NewNopSettings(metadata.Type), cfg, next)
	require.NoError(t, err)

	host := &authHost{extensions: map[component.ID]component.Component{authID: fakeAuthenticator{}}}
	require.NoError(t, rcv.Start(t.Context(), host))
	t.Cleanup(func() { require.NoError(t, rcv.Shutdown(context.Background())) })

	req := ptraceotlp.NewExportRequest()
	req.Traces().ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("span")
	tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	t.Run("http", func(t *testing.T) {
		body, err := req.MarshalProto()
		require.NoError(t, err)
		httpReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr+tracesPath, bytes.NewReader(body))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}).Do(httpReq)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assertAuth(t, got)
	})
	t.Run("grpc", func(t *testing.T) {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = ptraceotlp.NewGRPCClient(conn).Export(t.Context(), req)
		require.NoError(t, err)
		assertAuth(t, got)
	})
}

func assertAuth(t *testing.T, got <-chan client.AuthData) {
	t.Helper()
	select {
	case auth := <-got:
		require.NotNil(t, auth, "the handler's context carries no auth data")
		assert.Equal(t, "user-1", auth.GetAttribute("user.id"))
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the pipeline never received the request")
	}
}

// fakeAuthenticator accepts every request as user-1.
type fakeAuthenticator struct {
	component.StartFunc
	component.ShutdownFunc
}

var _ extensionauth.Server = fakeAuthenticator{}

func (fakeAuthenticator) Authenticate(ctx context.Context, _ map[string][]string) (context.Context, error) {
	info := client.FromContext(ctx)
	info.Auth = fakeAuthData{"user.id": "user-1"}
	return client.NewContext(ctx, info), nil
}

type fakeAuthData map[string]string

func (d fakeAuthData) GetAttribute(name string) any { return d[name] }

func (d fakeAuthData) GetAttributeNames() []string {
	names := make([]string, 0, len(d))
	for k := range d {
		names = append(names, k)
	}
	return names
}

// authHost is a host whose only extension is the authenticator.
type authHost struct {
	extensions map[component.ID]component.Component
}

var _ component.Host = (*authHost)(nil)

func (h *authHost) GetExtensions() map[component.ID]component.Component {
	return h.extensions
}

// testCertificate writes an ephemeral self-signed pair for 127.0.0.1; none
// is ever committed.
func testCertificate(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool = x509.NewCertPool()
	pool.AddCert(parsed)
	return certFile, keyFile, pool
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}
