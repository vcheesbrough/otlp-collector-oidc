package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Environment variables that point the harness at a prebuilt binary instead
// of building one.
const (
	EnvBinary  = "OTLP_COLLECTOR_OIDC_BIN"
	EnvVersion = "OTLP_COLLECTOR_OIDC_VERSION"
)

// modulePath is the Go module the binary is built from.
const modulePath = "github.com/vcheesbrough/otlp-collector-oidc"

// Binary is the collector executable under test and the version it was
// linked with.
type Binary struct {
	Path    string
	Version string
	// Root is the repository root, where the shipped configuration lives.
	Root string
}

// ShippedConfig is the path of the configuration the image ships.
func (b Binary) ShippedConfig() string {
	return filepath.Join(b.Root, "config", "collector.yaml")
}

// LocateOrBuild returns the binary named by EnvBinary and EnvVersion when
// both are set, and otherwise builds one into dir with coverage
// instrumentation, linked with the version scripts/version.sh reports.
func LocateOrBuild(ctx context.Context, dir string) (Binary, error) {
	root, err := moduleRoot(ctx)
	if err != nil {
		return Binary{}, err
	}
	if path, version := os.Getenv(EnvBinary), os.Getenv(EnvVersion); path != "" && version != "" {
		return Binary{Path: path, Version: version, Root: root}, nil
	}

	version, err := output(ctx, root, filepath.Join(root, "scripts", "version.sh"))
	if err != nil {
		return Binary{}, fmt.Errorf("computing version: %w", err)
	}
	path := filepath.Join(dir, "otlp-collector-oidc")
	// #nosec G204 -- every argument is fixed by the harness.
	cmd := exec.CommandContext(ctx, "go", "build",
		"-cover", "-coverpkg="+modulePath+"/...",
		"-ldflags", "-X "+modulePath+"/internal/build.version="+version,
		"-o", path, "./cmd/otlp-collector-oidc")
	cmd.Dir = root
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		return Binary{}, fmt.Errorf("building collector: %w\n%s", buildErr, out)
	}
	return Binary{Path: path, Version: version, Root: root}, nil
}

func moduleRoot(ctx context.Context) (string, error) {
	gomod, err := output(ctx, "", "go", "env", "GOMOD")
	if err != nil {
		return "", err
	}
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("not inside the otlp-collector-oidc module")
	}
	return filepath.Dir(gomod), nil
}

func output(ctx context.Context, dir, name string, args ...string) (string, error) {
	// #nosec G204 -- callers pass fixed commands.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}
