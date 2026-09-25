package oidcclientauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/go-jose/go-jose/v4"
)

// maxJWKSBytes bounds a JWKS response; a real one is a few kilobytes.
const maxJWKSBytes = 1 << 20

var (
	errUnknownKey   = errors.New("no key in the JWKS has the token's kid")
	errBadSignature = errors.New("signature does not verify")
)

// keySet is the provider's JWKS, cached. A token whose kid is not cached
// triggers one refresh, which is how a rotated-in key is found; concurrent
// refreshes coalesce into one fetch.
type keySet struct {
	client *http.Client
	url    string

	mu         sync.RWMutex // guards keys and generation
	keys       []jose.JSONWebKey
	generation uint64

	// refreshing serialises fetches, so requests that miss together wait for
	// one fetch rather than each making their own.
	refreshing sync.Mutex
}

func newKeySet(client *http.Client, jwksURL string) *keySet {
	return &keySet{client: client, url: jwksURL}
}

// VerifySignature returns the payload of jws if its signature verifies with
// the JWKS key its kid names, refreshing the JWKS once when that kid is not
// cached. It returns errUnknownKey or errBadSignature otherwise.
func (k *keySet) VerifySignature(ctx context.Context, jws *jose.JSONWebSignature) ([]byte, error) {
	if len(jws.Signatures) != 1 {
		return nil, errBadSignature
	}
	header := jws.Signatures[0].Header
	key, generation := k.lookup(header.KeyID, header.Algorithm)
	if key == nil {
		key = k.refreshFor(ctx, header.KeyID, header.Algorithm, generation)
	}
	if key == nil {
		return nil, errUnknownKey
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return nil, errBadSignature
	}
	return payload, nil
}

// lookup finds the signing key for kid and alg in the cache, and returns the
// cache generation it looked in.
func (k *keySet) lookup(kid, alg string) (*jose.JSONWebKey, uint64) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return findKey(k.keys, kid, alg), k.generation
}

// refreshFor refreshes the cache unless another request already did since
// generation, then looks again. A failed fetch leaves the cache as it was and
// the kid unknown.
func (k *keySet) refreshFor(ctx context.Context, kid, alg string, generation uint64) *jose.JSONWebKey {
	k.refreshing.Lock()
	defer k.refreshing.Unlock()
	k.mu.RLock()
	stale := k.generation == generation
	k.mu.RUnlock()
	if stale {
		if err := k.refresh(ctx); err != nil {
			return nil
		}
	}
	key, _ := k.lookup(kid, alg)
	return key
}

// refresh fetches the JWKS and replaces the cache with it.
func (k *keySet) refresh(ctx context.Context) error {
	keys, err := k.fetch(ctx)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = keys
	k.generation++
	return nil
}

// size is how many keys the cache holds.
func (k *keySet) size() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.keys)
}

func (k *keySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building JWKS request: %w", err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching JWKS: %s answered %s", k.url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading JWKS: %w", err)
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("JWKS is larger than %d bytes", maxJWKSBytes)
	}
	return parseJWKS(body)
}

// parseJWKS reads a JWKS, keeping every public signing key it can parse. A
// key of a type this build does not know is skipped rather than failing the
// whole set, so a provider that adds one does not lock every client out.
func parseJWKS(body []byte) ([]jose.JSONWebKey, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding JWKS: %w", err)
	}
	keys := make([]jose.JSONWebKey, 0, len(raw.Keys))
	for _, r := range raw.Keys {
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(r); err != nil {
			continue
		}
		if !key.IsPublic() || key.Use == "enc" {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no public signing key")
	}
	return keys, nil
}

// findKey returns the key whose kid is kid and whose alg, when the JWKS
// states one, is alg. The token profile requires a kid, so an empty one
// matches nothing.
func findKey(keys []jose.JSONWebKey, kid, alg string) *jose.JSONWebKey {
	if kid == "" {
		return nil
	}
	for i := range keys {
		if keys[i].KeyID == kid && (keys[i].Algorithm == "" || keys[i].Algorithm == alg) {
			return &keys[i]
		}
	}
	return nil
}
