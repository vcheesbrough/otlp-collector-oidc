package oidcclientauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNewAuthData is a unit test only because nothing in the shipped
// pipeline reads the auth context yet, so its contents cannot be observed
// from outside the process. The identity-stamping card makes them sink
// observations and retires these cases.
func TestNewAuthData(t *testing.T) {
	defaults := createDefaultConfig().(*Config).ClaimAttributes
	full := claims{
		"sub":                "user-1",
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"name":               "Alice Example",
		"scope":              "telemetry:write",
	}

	tests := []struct {
		name      string
		claims    claims
		mapping   map[string]string
		resources map[string]string
		want      map[string]any
	}{
		{
			name:    "default mapping, every claim present",
			claims:  full,
			mapping: defaults,
			want: map[string]any{
				"user.id":        "user-1",
				"user.name":      "alice",
				"user.email":     "alice@example.com",
				"user.full_name": "Alice Example",
			},
		},
		{
			name:    "optional claims absent add no key",
			claims:  claims{"sub": "user-1", "preferred_username": "alice"},
			mapping: defaults,
			want:    map[string]any{"user.id": "user-1", "user.name": "alice"},
		},
		{
			name:    "empty and non-string claims add no key",
			claims:  claims{"sub": "user-1", "preferred_username": "alice", "email": "", "name": 42.0},
			mapping: defaults,
			want:    map[string]any{"user.id": "user-1", "user.name": "alice"},
		},
		{
			name:    "a custom mapping replaces the default",
			claims:  full,
			mapping: map[string]string{"sub": "enduser.id"},
			want:    map[string]any{"enduser.id": "user-1"},
		},
		{
			name:      "resource attributes are added whatever the token says",
			claims:    full,
			mapping:   map[string]string{"sub": "user.id"},
			resources: map[string]string{"deployment.environment.name": "prod", "telemetry_source": "client"},
			want: map[string]any{
				"user.id":                     "user-1",
				"deployment.environment.name": "prod",
				"telemetry_source":            "client",
			},
		},
		{
			name:      "an empty resource map adds no key",
			claims:    full,
			mapping:   map[string]string{"sub": "user.id"},
			resources: map[string]string{},
			want:      map[string]any{"user.id": "user-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newAuthData(tt.claims, tt.mapping, tt.resources)
			got := map[string]any{}
			for _, name := range d.GetAttributeNames() {
				got[name] = d.GetAttribute(name)
			}
			assert.Equal(t, tt.want, got, "claims %v, mapping %v, resources %v", tt.claims, tt.mapping, tt.resources)
			assert.IsNonDecreasing(t, d.GetAttributeNames(), "names are sorted")
			assert.Nil(t, d.GetAttribute("not.there"))
		})
	}
}
