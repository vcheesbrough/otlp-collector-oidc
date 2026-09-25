// Package build carries the identity the binary was linked with.
package build

// version is set at link time with
// -ldflags "-X github.com/vcheesbrough/otlp-collector-oidc/internal/build.version=<v>".
// It is the one package variable the standards allow: the linker writes it
// once and nothing assigns it at run time.
var version = "0.0.0-dev+unknown"

// Version returns the semantic version the binary was built as: the release
// tag without its "v" for a tagged build, "<next>-dev+<sha>" otherwise.
func Version() string {
	return version
}
