// Package integration is the primary test tier: the built collector runs as a
// subprocess and every scenario drives it from outside, over HTTP and gRPC on
// its one port, observing the result at a fake upstream, on its own metrics,
// on its health endpoint and in its output.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/vcheesbrough/otlp-collector-oidc/integration/harness"
)

// binary is built once for the whole run; every environment shape runs it.
var binary harness.Binary

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "otlp-collector-oidc-integration")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)

	binary, err = harness.LocateOrBuild(context.Background(), dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
