package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/vcheesbrough/otlp-collector-oidc/extension/oidcclientauth"
)

// The renderer's golden files live in internal/render, which cannot import
// this package's components; so the collector's own validation of them runs
// here. What the rendered values do is the integration suite's to prove.

func goldens(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "render", "testdata", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	return files
}

func collectorFor(t *testing.T, file string) otelcol.CollectorSettings {
	t.Helper()
	abs, err := filepath.Abs(file)
	require.NoError(t, err)
	set := settings()
	set.ConfigProviderSettings.ResolverSettings.URIs = []string{"file:" + abs}
	set.ConfigProviderSettings.ResolverSettings.DefaultScheme = "env"
	return set
}

func TestGoldenConfigsValidate(t *testing.T) {
	for _, file := range goldens(t) {
		t.Run(filepath.Base(file), func(t *testing.T) {
			col, err := otelcol.NewCollector(collectorFor(t, file))
			require.NoError(t, err)
			assert.NoError(t, col.DryRun(t.Context()))
		})
	}
}

// TestHostileValuesArriveLiterally proves the renderer's quoting against the
// collector's own resolver: quotes, newlines and ${...} in a variable reach
// the component as the string the deployer set, not as YAML or a reference.
func TestHostileValuesArriveLiterally(t *testing.T) {
	file := filepath.Join("..", "..", "internal", "render", "testdata", "hostile.yaml")
	set := collectorFor(t, file)
	provider, err := otelcol.NewConfigProvider(set.ConfigProviderSettings)
	require.NoError(t, err)
	factories, err := components()
	require.NoError(t, err)
	cfg, err := provider.Get(t.Context(), factories)
	require.NoError(t, err)

	auth, ok := cfg.Extensions[component.MustNewID("oidcclientauth")].(*oidcclientauth.Config)
	require.True(t, ok)
	assert.Equal(t, "https://idp.example.com/?a=${env:HOME}", auth.IssuerURL)
	assert.Equal(t, "a\"b\nexporters: {}", auth.Audience)
	assert.Equal(t, "$${x}$", auth.RequiredScope)
	assert.Equal(t, []string{"sub", "'quoted'", "#hash"}, auth.RequiredClaims)
}
