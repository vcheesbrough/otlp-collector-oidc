package harness

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// Audience is the audience the harness's collectors are configured with and
// its tokens are issued for.
const Audience = "otlp-collector-oidc-test"

// Claims is a token's payload.
type Claims map[string]any

// signingKey is one key the issuer signs with, published in its JWKS or not.
type signingKey struct {
	kid     string
	private crypto.Signer
}

func (k signingKey) public() jose.JSONWebKey {
	return jose.JSONWebKey{Key: k.private.Public(), KeyID: k.kid, Use: "sig"}
}

// Issuer is a fake OIDC provider on loopback plain HTTP: a real discovery
// document and JWKS, and tokens signed with ephemeral RSA and EC keys
// generated for the run. It can start late, stop, and rotate its keys, and
// mints the deviant tokens the token profile forbids.
type Issuer struct {
	addr        string
	jwksFetches atomic.Int64

	mu        sync.Mutex // guards everything below
	server    *http.Server
	rsa       signingKey
	ec        signingKey
	published []jose.JSONWebKey
	nextKid   int
}

// NewIssuer returns an issuer with fresh keys that is not yet serving; Start
// makes it reachable. It stops when the test ends.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	i := &Issuer{addr: freeAddr(t)}
	i.rsa, i.ec = i.newRSA(t), i.newEC(t)
	i.published = []jose.JSONWebKey{i.rsa.public(), i.ec.public()}
	t.Cleanup(i.Stop)
	return i
}

// URL is the issuer identifier, the value of OIDC_ISSUER_URL and of iss.
func (i *Issuer) URL() string {
	return "http://" + i.addr
}

// JWKSFetches is how many times the JWKS has been served.
func (i *Issuer) JWKSFetches() int64 {
	return i.jwksFetches.Load()
}

// Start serves the discovery document and JWKS on the issuer's address.
func (i *Issuer) Start(t *testing.T) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", i.addr)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.serveDiscovery)
	mux.HandleFunc("GET /jwks", i.serveJWKS)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	i.mu.Lock()
	i.server = server
	i.mu.Unlock()
	go func() {
		// Serve returns when Stop closes the server.
		_ = server.Serve(ln)
	}()
}

// Stop closes the issuer's listener; Start serves again on the same address.
func (i *Issuer) Stop() {
	i.mu.Lock()
	server := i.server
	i.server = nil
	i.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

// Rotate publishes a new RSA and a new EC key alongside the old ones and
// signs with the new ones from now on, as a provider does during rotation.
func (i *Issuer) Rotate(t *testing.T) {
	t.Helper()
	rsaKey, ecKey := i.newRSA(t), i.newEC(t)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rsa, i.ec = rsaKey, ecKey
	i.published = append(i.published, rsaKey.public(), ecKey.public())
}

// Claims is a payload the collector accepts: this issuer, the harness's
// audience, the required scope and claims, the optional ones, and an hour to
// live.
func (i *Issuer) Claims() Claims {
	now := time.Now()
	return Claims{
		"iss":                i.URL(),
		"aud":                Audience,
		"sub":                "user-1",
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"name":               "Alice Example",
		"scope":              "openid profile telemetry:write",
		"iat":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
	}
}

// IDTokenClaims is the shape of an OIDC ID token for the same user: the same
// identity and audience, a nonce and an at_hash, and no scope.
func (i *Issuer) IDTokenClaims() Claims {
	c := i.Claims()
	delete(c, "scope")
	c["nonce"] = "n-0S6_WzA2Mj"
	c["at_hash"] = "77QmUPtjPfzWtF2AnpK9RQ"
	c["auth_time"] = time.Now().Unix()
	return c
}

// Sign signs claims with alg using the issuer's current key for it: RS* and
// PS* with its RSA key, ES256 with its EC key.
func (i *Issuer) Sign(t *testing.T, alg jose.SignatureAlgorithm, claims Claims) string {
	t.Helper()
	i.mu.Lock()
	key := i.rsa
	if strings.HasPrefix(string(alg), "ES") {
		key = i.ec
	}
	i.mu.Unlock()
	return sign(t, alg, key, claims)
}

// SignUnpublished signs claims with an RSA key whose kid the JWKS never
// publishes.
func (i *Issuer) SignUnpublished(t *testing.T, claims Claims) string {
	t.Helper()
	return sign(t, jose.RS256, i.newRSA(t), claims)
}

// SignHMACConfused signs claims with HS256, keyed by the issuer's published
// RSA public key and naming its kid: the algorithm-confusion attack a
// verifier that trusts alg would accept.
func (i *Issuer) SignHMACConfused(t *testing.T, claims Claims) string {
	t.Helper()
	i.mu.Lock()
	key := i.rsa
	i.mu.Unlock()
	pub, err := x509.MarshalPKIXPublicKey(key.private.Public())
	require.NoError(t, err)
	signingInput := encodeSegment(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": key.kid}) + "." + encodeSegment(t, claims)
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Unsigned is claims as a JWS with alg none and an empty signature.
func (i *Issuer) Unsigned(t *testing.T, claims Claims) string {
	t.Helper()
	i.mu.Lock()
	kid := i.rsa.kid
	i.mu.Unlock()
	return encodeSegment(t, map[string]any{"alg": "none", "typ": "JWT", "kid": kid}) + "." + encodeSegment(t, claims) + "."
}

// Tamper replaces the payload of a signed token, keeping its header and
// signature, so the signature no longer verifies.
func Tamper(t *testing.T, token string, claims Claims) string {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	return parts[0] + "." + encodeSegment(t, claims) + "." + parts[2]
}

func (i *Issuer) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	base := i.URL()
	writeJSON(w, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"jwks_uri":                              base + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "telemetry:write"},
	})
}

func (i *Issuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	i.jwksFetches.Add(1)
	i.mu.Lock()
	set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), i.published...)}
	i.mu.Unlock()
	writeJSON(w, set)
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (i *Issuer) newRSA(t *testing.T) signingKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return signingKey{kid: i.kid("rsa"), private: key}
}

func (i *Issuer) newEC(t *testing.T) signingKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return signingKey{kid: i.kid("ec"), private: key}
}

func (i *Issuer) kid(kind string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.nextKid++
	return kind + "-" + strconv.Itoa(i.nextKid)
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key signingKey, claims Claims) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: key.private, KeyID: key.kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := signer.Sign(payload)
	require.NoError(t, err)
	token, err := jws.CompactSerialize()
	require.NoError(t, err)
	return token
}

func encodeSegment(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}
