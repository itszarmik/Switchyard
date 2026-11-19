// Package auth authenticates gateway API keys.
//
// A gateway key is "sy_" followed by the unpadded base64url encoding of 32
// random bytes. Only its SHA-256 hash is ever stored: config holds
// "sha256:<hex>" (see HashKey), and a Store indexes those hashes in memory.
// Because the keys are 256-bit random values rather than human-chosen
// passwords, a fast hash is safe, and authentication is one SHA-256 and one
// map lookup (DESIGN §17).
//
// Middleware reads "Authorization: Bearer <key>", rejects anything else with
// the OpenAI-shaped 401 invalid_api_key error, and on success stores the
// caller's Principal in the request context. Neither the key nor its hash is
// ever logged or echoed.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"switchyard/internal/config"
)

const (
	// keyPrefix starts every gateway key, so secret scanners can find them.
	keyPrefix = "sy_"
	// keyBytes is the number of random bytes in a key.
	keyBytes = 32
	// maxKeyLen bounds a presented key; longer ones are rejected unhashed.
	maxKeyLen = 256
	// hashPrefix starts every stored key hash.
	hashPrefix = "sha256:"
)

// Principal identifies an authenticated caller.
type Principal struct {
	// TenantID is the id of the tenant that owns the key.
	TenantID string
	// KeyID is the id of the key, never the key itself.
	KeyID string
	// Tags are the key's tags from config. The slice is shared by every
	// request made with the key; callers must not modify it.
	Tags []string
	// RateLimit is the key's effective rate limit: the key's own
	// rate_limit when it sets one, else its tenant's. The zero value means
	// no limit. It comes from the Store the request authenticated against,
	// so a reload changes it for the requests that follow.
	RateLimit config.RateLimit
}

// GenerateKey returns a new random gateway key and its hash. The key is
// "sy_" followed by the unpadded base64url encoding of 32 bytes from
// crypto/rand, and hash is HashKey(key).
func GenerateKey() (key, hash string, err error) {
	var b [keyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("auth: generate key: %w", err)
	}
	key = keyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return key, HashKey(key), nil
}

// HashKey returns the stored form of key: "sha256:" followed by the
// lowercase hex SHA-256 of the key's bytes.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hashPrefix + hex.EncodeToString(sum[:])
}

// Store is an immutable in-memory index of API keys by hash. It is safe for
// concurrent use.
type Store struct {
	byHash map[[sha256.Size]byte]Principal
}

// NewStore indexes the keys of tenants, which config validation has already
// checked. A key whose hash is not "sha256:" plus 64 lowercase hex digits is
// skipped, so it can never authenticate; if two keys share a hash, the first
// wins.
func NewStore(tenants []config.Tenant) *Store {
	s := &Store{byHash: make(map[[sha256.Size]byte]Principal)}
	for _, t := range tenants {
		for _, k := range t.Keys {
			sum, ok := parseHash(k.Hash)
			if !ok {
				continue
			}
			if _, dup := s.byHash[sum]; dup {
				continue
			}
			lim := t.RateLimit
			if k.RateLimit != nil {
				lim = *k.RateLimit
			}
			s.byHash[sum] = Principal{TenantID: t.ID, KeyID: k.ID, Tags: slices.Clip(slices.Clone(k.Tags)), RateLimit: lim}
		}
	}
	return s
}

// parseHash decodes a stored hash, which must be exactly "sha256:" and 64
// lowercase hex digits, the form config validation accepts.
func parseHash(h string) (sum [sha256.Size]byte, ok bool) {
	hexPart, found := strings.CutPrefix(h, hashPrefix)
	if !found || len(hexPart) != hex.EncodedLen(sha256.Size) {
		return sum, false
	}
	if _, err := hex.Decode(sum[:], []byte(hexPart)); err != nil {
		return sum, false
	}
	// hex.Decode also accepts upper case.
	if hex.EncodeToString(sum[:]) != hexPart {
		return sum, false
	}
	return sum, true
}

// Authenticate returns the Principal that owns key. A key longer than 256
// bytes, or one that does not start with "sy_", is rejected without being
// hashed. A nil Store authenticates nothing.
func (s *Store) Authenticate(key string) (Principal, bool) {
	if s == nil || len(key) > maxKeyLen || !strings.HasPrefix(key, keyPrefix) {
		return Principal{}, false
	}
	// The map is keyed by the hash, so no comparison involves the secret
	// key itself; the lookup reveals nothing useful about stored keys.
	p, ok := s.byHash[sha256.Sum256([]byte(key))]
	return p, ok
}

// Len returns the number of keys in the store.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byHash)
}

// Authenticator authenticates a presented gateway key. *Store and
// *Keyring implement it.
type Authenticator interface {
	// Authenticate returns the Principal that owns key, or false.
	Authenticate(key string) (Principal, bool)
}

// Keyring holds the Store in force and replaces it atomically, for config
// reloads: each request authenticates against the Store in force when it
// arrives, and a key removed by a reload stops authenticating at once. It
// is safe for concurrent use.
type Keyring struct {
	cur atomic.Pointer[Store]
}

// NewKeyring returns a Keyring that holds s.
func NewKeyring(s *Store) *Keyring {
	k := &Keyring{}
	k.cur.Store(s)
	return k
}

// Swap makes s the Store in force.
func (k *Keyring) Swap(s *Store) { k.cur.Store(s) }

// Store returns the Store in force.
func (k *Keyring) Store() *Store { return k.cur.Load() }

// Authenticate authenticates key against the Store in force.
func (k *Keyring) Authenticate(key string) (Principal, bool) { return k.cur.Load().Authenticate(key) }

// principalKey is the context key for the authenticated Principal.
type principalKey struct{}

// newContext returns a copy of ctx that carries p.
func newContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// WithPrincipal returns a copy of ctx that carries p, as Middleware does
// for an authenticated request. It is for callers that authenticate
// requests some other way, and for tests.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return newContext(ctx, p)
}

// FromContext returns the Principal that Middleware stored in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
