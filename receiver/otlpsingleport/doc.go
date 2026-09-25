// Package otlpsingleport is an OTLP receiver that serves OTLP/gRPC and
// OTLP/HTTP on one TLS listener, telling the two apart per request.
//
//go:generate go tool mdatagen metadata.yaml
package otlpsingleport
