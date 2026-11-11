package auth

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

var (
	keyRE  = regexp.MustCompile(`^sy_[A-Za-z0-9_-]{43}$`)
	hashRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// mustGenerate returns a fresh key and its hash.
func mustGenerate(t testing.TB) (key, hash string) {
	t.Helper()
	key, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key, hash
}

func TestGenerateKey(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		key, hash := mustGenerate(t)
		if !keyRE.MatchString(key) {
			t.Fatalf("key %q does not match %s", key, keyRE)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "sy_"))
		if err != nil || len(raw) != 32 {
			t.Fatalf("key body decodes to %d bytes (err %v), want 32", len(raw), err)
		}
		if !hashRE.MatchString(hash) {
			t.Fatalf("hash %q does not match %s", hash, hashRE)
		}
		if hash != HashKey(key) {
			t.Fatalf("hash %q != HashKey(key) %q", hash, HashKey(key))
		}
		if seen[key] {
			t.Fatalf("GenerateKey returned %q twice", key)
		}
		seen[key] = true
	}
}

func TestHashKey(t *testing.T) {
	tests := []struct{ key, want string }{
		// FIPS 180-2 test vectors.
		{"", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := HashKey(tt.key); got != tt.want {
				t.Errorf("HashKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestStoreAuthenticate(t *testing.T) {
	k1, h1 := mustGenerate(t)
	k2, h2 := mustGenerate(t)
	k3, h3 := mustGenerate(t)
	unknown, _ := mustGenerate(t)
	// Keys whose hashes are registered but which Authenticate must reject
	// before hashing them.
	noPrefix := "xx_" + strings.TrimPrefix(k1, "sy_")
	maxLen := "sy_" + strings.Repeat("a", 253)
	tooLong := "sy_" + strings.Repeat("a", 254)
	upper, _ := mustGenerate(t)
	upper = "SY_" + strings.TrimPrefix(upper, "sy_")

	tenants := []config.Tenant{
		{ID: "acme", Keys: []config.APIKey{
			{ID: "acme-backend", Hash: h1, Tags: []string{"prod", "eu"}},
			{ID: "acme-ci", Hash: h2},
			{ID: "no-prefix", Hash: HashKey(noPrefix)},
			{ID: "max-len", Hash: HashKey(maxLen)},
			{ID: "too-long", Hash: HashKey(tooLong)},
			{ID: "upper", Hash: HashKey(upper)},
		}},
		{ID: "beta", Keys: []config.APIKey{{ID: "beta-1", Hash: h3}}},
		{ID: "empty"},
	}
	s := NewStore(tenants)
	if got := s.Len(); got != 7 {
		t.Errorf("Len() = %d, want 7", got)
	}

	tests := []struct {
		name string
		key  string
		want Principal
		ok   bool
	}{
		{"first key", k1, Principal{TenantID: "acme", KeyID: "acme-backend", Tags: []string{"prod", "eu"}}, true},
		{"second key", k2, Principal{TenantID: "acme", KeyID: "acme-ci"}, true},
		{"other tenant", k3, Principal{TenantID: "beta", KeyID: "beta-1"}, true},
		{"256 bytes", maxLen, Principal{TenantID: "acme", KeyID: "max-len"}, true},
		{"unknown key", unknown, Principal{}, false},
		{"empty", "", Principal{}, false},
		{"prefix only", "sy_", Principal{}, false},
		{"missing prefix", noPrefix, Principal{}, false},
		{"uppercase prefix", upper, Principal{}, false},
		{"longer than 256 bytes", tooLong, Principal{}, false},
		{"the hash is not the key", h1, Principal{}, false},
		{"key with a suffix", k1 + "x", Principal{}, false},
		{"key truncated", k1[:len(k1)-1], Principal{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := s.Authenticate(tt.key)
			if ok != tt.ok {
				t.Fatalf("Authenticate ok = %v, want %v", ok, tt.ok)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Principal (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("tags are copied from the config", func(t *testing.T) {
		tenants[0].Keys[0].Tags[0] = "mutated"
		p, _ := s.Authenticate(k1)
		if p.Tags[0] != "prod" {
			t.Errorf("Tags = %q after the config slice changed", p.Tags)
		}
	})
}

func TestNewStoreSkipsBadEntries(t *testing.T) {
	k1, h1 := mustGenerate(t)
	k2, h2 := mustGenerate(t)
	s := NewStore([]config.Tenant{
		{ID: "a", Keys: []config.APIKey{
			{ID: "first", Hash: h1},
			{ID: "duplicate", Hash: h1},
			{ID: "uppercase", Hash: "sha256:" + strings.ToUpper(strings.TrimPrefix(h2, "sha256:"))},
			{ID: "no-prefix", Hash: strings.TrimPrefix(h2, "sha256:")},
			{ID: "short", Hash: h2[:len(h2)-2]},
			{ID: "not-hex", Hash: "sha256:" + strings.Repeat("zz", 32)},
			{ID: "plaintext", Hash: k2},
			{ID: "empty"},
		}},
	})
	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
	if p, ok := s.Authenticate(k1); !ok || p.KeyID != "first" {
		t.Errorf("Authenticate(k1) = %+v, %v; want the first entry", p, ok)
	}
}

func TestNilAndEmptyStore(t *testing.T) {
	key, _ := mustGenerate(t)
	for name, s := range map[string]*Store{"nil": nil, "empty": NewStore(nil)} {
		t.Run(name, func(t *testing.T) {
			if s.Len() != 0 {
				t.Errorf("Len() = %d, want 0", s.Len())
			}
			if p, ok := s.Authenticate(key); ok {
				t.Errorf("Authenticate = %+v, true; want false", p)
			}
		})
	}
}

func TestStoreConcurrentUse(t *testing.T) {
	key, hash := mustGenerate(t)
	s := NewStore([]config.Tenant{{ID: "a", Keys: []config.APIKey{{ID: "k", Hash: hash}}}})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, ok := s.Authenticate(key); !ok {
					t.Error("Authenticate failed")
					return
				}
			}
		})
	}
	wg.Wait()
}

// A key generated here, written into a config file as its hash, loads and
// authenticates: the formats of GenerateKey, config validation and NewStore
// agree.
func TestGeneratedKeyThroughConfig(t *testing.T) {
	key, hash := mustGenerate(t)
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	yaml := "version: 1\ntenants:\n  - id: acme\n    keys:\n      - id: acme-backend\n        hash: " + hash + "\n        tags: [prod]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if w := snap.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
	s := NewStore(snap.Config().Tenants)
	p, ok := s.Authenticate(key)
	want := Principal{TenantID: "acme", KeyID: "acme-backend", Tags: []string{"prod"}}
	if !ok || !cmp.Equal(p, want) {
		t.Errorf("Authenticate = %+v, %v; want %+v, true", p, ok, want)
	}
}

func TestFromContext(t *testing.T) {
	if p, ok := FromContext(context.Background()); ok {
		t.Errorf("FromContext(empty) = %+v, true", p)
	}
	want := Principal{TenantID: "t", KeyID: "k"}
	p, ok := FromContext(newContext(context.Background(), want))
	if !ok || !cmp.Equal(p, want) {
		t.Errorf("FromContext = %+v, %v; want %+v, true", p, ok, want)
	}
}

func BenchmarkAuthenticate(b *testing.B) {
	key, hash := mustGenerate(b)
	tenants := []config.Tenant{{ID: "a", Keys: []config.APIKey{{ID: "k", Hash: hash}}}}
	for range 1000 {
		_, h := mustGenerate(b)
		tenants[0].Keys = append(tenants[0].Keys, config.APIKey{ID: "x", Hash: h})
	}
	s := NewStore(tenants)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := s.Authenticate(key); !ok {
			b.Fatal("Authenticate failed")
		}
	}
}
