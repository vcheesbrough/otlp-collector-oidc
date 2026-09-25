package oidcclientauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// keySource verifies a JWS signature against the provider's keys.
type keySource interface {
	VerifySignature(ctx context.Context, jws *jose.JSONWebSignature) ([]byte, error)
}

// claims is a verified token's payload.
type claims map[string]any

// verifier turns a compact JWS into verified claims under the token
// profile's header and registered-claim rules.
type verifier struct {
	issuer     string
	audience   string
	algorithms []jose.SignatureAlgorithm
	skew       time.Duration
	now        func() time.Time
}

// verify checks, in order: the header's alg against the allowlist, the
// signature against the kid's key, then iss, aud, exp, nbf and iat.
func (v *verifier) verify(ctx context.Context, keys keySource, token string) (claims, *rejectionError) {
	alg, ok := headerAlgorithm(token)
	if !ok {
		return nil, invalidToken(faultMalformed)
	}
	if !slices.Contains(v.algorithms, alg) {
		return nil, invalidToken(faultAlgorithm)
	}
	jws, err := jose.ParseSignedCompact(token, v.algorithms)
	if err != nil {
		return nil, invalidToken(faultMalformed)
	}
	payload, err := keys.VerifySignature(ctx, jws)
	switch {
	case errors.Is(err, errUnknownKey):
		return nil, invalidToken(faultUnknownKey)
	case err != nil:
		return nil, invalidToken(faultSignature)
	}
	c, ok := decodeClaims(payload)
	if !ok {
		return nil, invalidToken(faultMalformed)
	}
	if f, ok := v.registered(c); !ok {
		return nil, invalidToken(f)
	}
	return c, nil
}

// registered applies the registered-claim rules and names the first broken.
func (v *verifier) registered(c claims) (fault, bool) {
	if iss, _ := c["iss"].(string); iss != v.issuer {
		return faultIssuer, false
	}
	if !audienceContains(c["aud"], v.audience) {
		return faultAudience, false
	}
	now := v.now()
	exp, present, ok := numericDate(c, "exp")
	switch {
	case !ok:
		return faultMalformed, false
	case !present:
		return faultNoExpiry, false
	case !now.Before(exp.Add(v.skew)):
		return faultExpired, false
	}
	if nbf, present, ok := numericDate(c, "nbf"); !ok {
		return faultMalformed, false
	} else if present && now.Add(v.skew).Before(nbf) {
		return faultNotYetValid, false
	}
	if iat, present, ok := numericDate(c, "iat"); !ok {
		return faultMalformed, false
	} else if present && iat.After(now.Add(v.skew)) {
		return faultIssuedInFuture, false
	}
	return 0, true
}

// headerAlgorithm reads alg from a compact JWS's protected header, before
// anything else is parsed, so an alg outside the allowlist is reported as
// such rather than as a malformed token.
func headerAlgorithm(token string) (jose.SignatureAlgorithm, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.Alg == "" {
		return "", false
	}
	return jose.SignatureAlgorithm(header.Alg), true
}

func decodeClaims(payload []byte) (claims, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	var c claims
	if err := dec.Decode(&c); err != nil || c == nil {
		return nil, false
	}
	return c, true
}

// audienceContains accepts aud as a string or an array of strings.
func audienceContains(aud any, audience string) bool {
	switch a := aud.(type) {
	case string:
		return a == audience
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok && s == audience {
				return true
			}
		}
	}
	return false
}

// numericDate reads a NumericDate claim: seconds since the epoch, possibly
// fractional. ok is false when the claim is present but not a number.
func numericDate(c claims, name string) (t time.Time, present, ok bool) {
	v, present := c[name]
	if !present {
		return time.Time{}, false, true
	}
	f, isNumber := v.(float64)
	if !isNumber {
		return time.Time{}, true, false
	}
	// Some thirty million years either way: past any real date, and small
	// enough that converting to int64 seconds is defined.
	const limit = 1e15
	f = max(min(f, limit), -limit)
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*float64(time.Second))), true, true
}
