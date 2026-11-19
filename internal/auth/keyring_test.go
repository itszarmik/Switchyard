package auth

import (
	"sync"
	"testing"

	"switchyard/internal/config"
)

// TestStoreRateLimit: a key's effective rate limit is its own when set,
// else its tenant's.
func TestStoreRateLimit(t *testing.T) {
	k1, h1 := mustGenerate(t)
	k2, h2 := mustGenerate(t)
	k3, h3 := mustGenerate(t)
	tenantLim := config.RateLimit{RequestsPerMinute: 60, Burst: 10, MaxConcurrentStreams: 2}
	keyLim := config.RateLimit{RequestsPerMinute: 600, Burst: 100}
	s := NewStore([]config.Tenant{
		{ID: "t1", RateLimit: tenantLim, Keys: []config.APIKey{
			{ID: "inherits", Hash: h1},
			{ID: "overrides", Hash: h2, RateLimit: &keyLim},
		}},
		{ID: "t2", Keys: []config.APIKey{{ID: "unlimited", Hash: h3}}},
	})
	tests := []struct {
		key  string
		want config.RateLimit
	}{
		{k1, tenantLim},
		{k2, keyLim},
		{k3, config.RateLimit{}},
	}
	for _, tt := range tests {
		p, ok := s.Authenticate(tt.key)
		if !ok {
			t.Fatalf("the key with limit %+v did not authenticate", tt.want)
		}
		if p.RateLimit != tt.want {
			t.Errorf("%s: RateLimit = %+v, want %+v", p.KeyID, p.RateLimit, tt.want)
		}
	}
}

// TestKeyring: a swap takes effect for the next authentication, and
// concurrent swaps and lookups are safe.
func TestKeyring(t *testing.T) {
	oldKey, oldHash := mustGenerate(t)
	newKey, newHash := mustGenerate(t)
	before := NewStore([]config.Tenant{{ID: "t", Keys: []config.APIKey{{ID: "old", Hash: oldHash}}}})
	after := NewStore([]config.Tenant{{ID: "t", Keys: []config.APIKey{{ID: "new", Hash: newHash}}}})

	k := NewKeyring(before)
	if _, ok := k.Authenticate(oldKey); !ok {
		t.Fatal("the old key does not authenticate before the swap")
	}
	k.Swap(after)
	if k.Store() != after {
		t.Error("Store() is not the swapped-in store")
	}
	tests := []struct {
		key  string
		want bool
	}{{oldKey, false}, {newKey, true}}
	for _, tt := range tests {
		if _, ok := k.Authenticate(tt.key); ok != tt.want {
			t.Errorf("after the swap: Authenticate = %v, want %v", ok, tt.want)
		}
	}

	var empty Keyring
	if _, ok := empty.Authenticate(newKey); ok {
		t.Error("an empty Keyring authenticated a key")
	}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 200 {
				if (i+j)%2 == 0 {
					k.Swap(before)
				} else {
					k.Swap(after)
				}
				k.Authenticate(newKey)
			}
		})
	}
	wg.Wait()
}
