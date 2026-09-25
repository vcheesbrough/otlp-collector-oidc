// Package harness drives the built collector from outside: it builds or
// locates the binary, runs it as a subprocess with an environment, and
// observes it only through its external surface — the one listener, the
// upstream it forwards to, :8888, the health endpoint and its output.
//
// Each piece is its own type: Binary and Collector run the process,
// Certificate is an ephemeral TLS pair, Sink is a fake upstream, HTTPClient
// and GRPCClient speak OTLP to the listener, and Scrape and ListeningPorts
// read what the process exposes.
package harness
