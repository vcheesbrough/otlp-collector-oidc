package oidcclientauth

import (
	"maps"
	"slices"

	"go.opentelemetry.io/collector/client"
)

// authData is what an authenticated request carries in its client.Info: the
// mapped claims and the deployment's static attributes, which a pipeline
// reads as auth.<name>.
type authData struct {
	attributes map[string]string
}

var _ client.AuthData = authData{}

// newAuthData maps each claim in claimAttributes that is a non-empty string
// to its attribute, skipping the others, and adds every resourceAttributes
// entry. Validate guarantees the two never name the same attribute.
func newAuthData(c claims, claimAttributes, resourceAttributes map[string]string) authData {
	attrs := make(map[string]string, len(claimAttributes)+len(resourceAttributes))
	for claim, attr := range claimAttributes {
		if v, ok := c[claim].(string); ok && v != "" {
			attrs[attr] = v
		}
	}
	maps.Copy(attrs, resourceAttributes)
	return authData{attributes: attrs}
}

// GetAttribute returns the named attribute, or nil when it is absent.
func (d authData) GetAttribute(name string) any {
	if v, ok := d.attributes[name]; ok {
		return v
	}
	return nil
}

// GetAttributeNames returns every attribute name, sorted.
func (d authData) GetAttributeNames() []string {
	return slices.Sorted(maps.Keys(d.attributes))
}
