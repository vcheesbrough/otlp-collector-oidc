package harness

import "crypto/x509"

// Frontend is where clients reach the collector: its one listener directly,
// or a proxy in front of it. Scenarios take their clients from a Frontend,
// so the same table runs either way.
type Frontend interface {
	// BaseURL is what OTLP/HTTP paths (/v1/<signal>) are appended to,
	// including any mount prefix.
	BaseURL() string
	// GRPCTarget is the host:port a gRPC client dials; gRPC method paths are
	// never prefixed.
	GRPCTarget() string
	// Pool trusts the certificate the frontend serves.
	Pool() *x509.CertPool
}

// Direct is the collector's own listener as a Frontend.
type Direct struct {
	addr string
	pool *x509.CertPool
}

var _ Frontend = Direct{}

// DirectTo is the listener at addr, serving a certificate pool trusts.
func DirectTo(addr string, pool *x509.CertPool) Direct {
	return Direct{addr: addr, pool: pool}
}

// BaseURL is https://addr.
func (d Direct) BaseURL() string { return "https://" + d.addr }

// GRPCTarget is addr.
func (d Direct) GRPCTarget() string { return d.addr }

// Pool trusts the listener's certificate.
func (d Direct) Pool() *x509.CertPool { return d.pool }
