package oidcclientauth

import "errors"

// ErrNotReady is Authenticate's answer while the provider's keys have not
// loaded. It refuses nothing about the token: the receiver matches its text
// and answers 503 with Retry-After, so a client backs off and retries rather
// than treating it as its own fault.
var ErrNotReady = errors.New("not ready")

// reason is why a request was refused. Its String is the reason label of the
// rejection counter and the reason field of the warning, so it is a closed
// set: a new reason is a new constant, and the exhaustive linter fails every
// switch that misses it.
type reason int

const (
	// reasonNoToken: no Authorization: Bearer header.
	reasonNoToken reason = iota
	// reasonInvalidToken: the token breaks a header or registered-claim rule.
	reasonInvalidToken
	// reasonMissingScope: the token does not grant the required scope.
	reasonMissingScope
	// reasonMissingClaim: a required claim is absent or empty.
	reasonMissingClaim
	// reasonNotReady: the provider's keys have not loaded yet.
	reasonNotReady
)

func (r reason) String() string {
	switch r {
	case reasonNoToken:
		return "no_token"
	case reasonInvalidToken:
		return "invalid_token"
	case reasonMissingScope:
		return "missing_scope"
	case reasonMissingClaim:
		return "missing_claim"
	case reasonNotReady:
		return "not_ready"
	default:
		return "unknown"
	}
}

// fault is which rule an invalid token breaks. Its String is the text after
// "invalid token: " in the 401 body, which docs/token-profile.md documents
// rule by rule.
type fault int

const (
	// faultMalformed: not a JWS in compact serialisation with a JSON payload.
	faultMalformed fault = iota
	// faultAlgorithm: alg is not on the allowlist (none, HS*, PS*, ...).
	faultAlgorithm
	// faultUnknownKey: kid matches no key in the JWKS, even after a refresh.
	faultUnknownKey
	// faultSignature: the signature does not verify with the kid's key.
	faultSignature
	// faultIssuer: iss is not exactly the configured issuer.
	faultIssuer
	// faultAudience: aud does not contain the configured audience.
	faultAudience
	// faultNoExpiry: exp is absent.
	faultNoExpiry
	// faultExpired: exp is past, beyond the clock skew.
	faultExpired
	// faultNotYetValid: nbf is ahead, beyond the clock skew.
	faultNotYetValid
	// faultIssuedInFuture: iat is ahead, beyond the clock skew.
	faultIssuedInFuture
)

func (f fault) String() string {
	switch f {
	case faultMalformed:
		return "malformed"
	case faultAlgorithm:
		return "unsupported alg"
	case faultUnknownKey:
		return "unknown kid"
	case faultSignature:
		return "bad signature"
	case faultIssuer:
		return "wrong iss"
	case faultAudience:
		return "wrong aud"
	case faultNoExpiry:
		return "missing exp"
	case faultExpired:
		return "expired"
	case faultNotYetValid:
		return "not yet valid"
	case faultIssuedInFuture:
		return "issued in the future"
	default:
		return "unknown"
	}
}

// rejectionError is a refusal of the token. Its text is the 401 body, the
// contract docs/token-profile.md states, and it never contains the token.
type rejectionError struct {
	reason reason
	// detail completes the text: the fault, the scope or the claim name.
	detail string
}

func (e *rejectionError) Error() string {
	switch e.reason {
	case reasonNoToken:
		return "no token"
	case reasonInvalidToken:
		return "invalid token: " + e.detail
	case reasonMissingScope:
		return "missing scope: " + e.detail
	case reasonMissingClaim:
		return "missing claim: " + e.detail
	case reasonNotReady:
		return ErrNotReady.Error()
	default:
		return "refused"
	}
}

func noToken() *rejectionError {
	return &rejectionError{reason: reasonNoToken}
}

func invalidToken(f fault) *rejectionError {
	return &rejectionError{reason: reasonInvalidToken, detail: f.String()}
}

func missingScope(scope string) *rejectionError {
	return &rejectionError{reason: reasonMissingScope, detail: scope}
}

func missingClaim(name string) *rejectionError {
	return &rejectionError{reason: reasonMissingClaim, detail: name}
}
