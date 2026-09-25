package oidcclientauth

import (
	"slices"
	"strings"
)

// authorizer applies the rules that follow verification: the token grants
// the required scope and carries every required claim.
type authorizer struct {
	scope  string
	claims []string
}

func (a authorizer) authorize(c claims) *rejectionError {
	if !grantsScope(c, a.scope) {
		return missingScope(a.scope)
	}
	for _, name := range a.claims {
		if !nonEmpty(c[name]) {
			return missingClaim(name)
		}
	}
	return nil
}

// grantsScope reads scope as a space-delimited string (RFC 9068) and scp as
// an array of strings or, as some providers issue it, a space-delimited
// string.
func grantsScope(c claims, scope string) bool {
	if s, ok := c["scope"].(string); ok && slices.Contains(strings.Fields(s), scope) {
		return true
	}
	switch scp := c["scp"].(type) {
	case string:
		return slices.Contains(strings.Fields(scp), scope)
	case []any:
		for _, v := range scp {
			if s, ok := v.(string); ok && s == scope {
				return true
			}
		}
	}
	return false
}

// nonEmpty is false for an absent claim, null, an empty string, and an empty
// array or object.
func nonEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}
