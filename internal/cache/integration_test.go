//go:build integration

package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// integrationClient returns a client for the real Redis named by
// SWITCHYARD_TEST_REDIS_URL (for example the compose service redis-state),
// built as the gateway builds its clients, or skips the test.
func integrationClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("SWITCHYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SWITCHYARD_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("SWITCHYARD_TEST_REDIS_URL is not a valid Redis URL") // never echo it: it may hold a password
	}
	opts.ContextTimeoutEnabled = true
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis is not reachable: %v", err)
	}
	return client
}

// randomPrefix returns a prefix unique to one subtest.
func randomPrefix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "test-" + hex.EncodeToString(b[:])
}

// prefixStore namespaces a Store's keys, so subtests can share a server.
type prefixStore struct {
	Store
	prefix string
}

func (s prefixStore) Get(ctx context.Context, key string) (*Entry, error) {
	return s.Store.Get(ctx, s.prefix+key)
}

func (s prefixStore) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	return s.Store.Set(ctx, s.prefix+key, e, ttl)
}

// prefixNamespaces namespaces the tenants of a Namespaces.
type prefixNamespaces struct {
	Namespaces
	prefix string
}

func (n prefixNamespaces) Version(ctx context.Context, tenant string) (int64, error) {
	return n.Namespaces.Version(ctx, n.prefix+tenant)
}

func (n prefixNamespaces) Bump(ctx context.Context, tenant string) (int64, error) {
	return n.Namespaces.Bump(ctx, n.prefix+tenant)
}

// deleteMatching deletes every key matching pattern.
func deleteMatching(t *testing.T, client *redis.Client, pattern string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	iter := client.Scan(ctx, 0, pattern, 1000).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Errorf("scanning test keys: %v", err)
		return
	}
	if len(keys) > 0 {
		if err := client.Del(ctx, keys...).Err(); err != nil {
			t.Errorf("deleting test keys: %v", err)
		}
	}
}

// TestRedisIntegration runs the Store and Namespaces contracts, and the
// expiry and key layout checks, against real Redis.
func TestRedisIntegration(t *testing.T) {
	client := integrationClient(t)

	t.Run("store contract", func(t *testing.T) {
		testStoreContract(t, func(t *testing.T) Store {
			prefix := "sy:x:v1:" + randomPrefix() + ":"
			t.Cleanup(func() { deleteMatching(t, client, prefix+"*") })
			return prefixStore{Store: NewRedisStore(client), prefix: prefix}
		})
	})

	t.Run("namespaces contract", func(t *testing.T) {
		testNamespacesContract(t, func(t *testing.T) Namespaces {
			prefix := randomPrefix() + "-"
			t.Cleanup(func() { deleteMatching(t, client, "sy:ns:v1:"+prefix+"*") })
			return prefixNamespaces{Namespaces: NewRedisNamespaces(client), prefix: prefix}
		})
	})

	t.Run("expiry", func(t *testing.T) {
		ctx := context.Background()
		key := "sy:x:v1:" + randomPrefix() + ":k"
		t.Cleanup(func() { deleteMatching(t, client, key) })
		s := NewRedisStore(client)
		if err := s.Set(ctx, key, &Entry{Body: []byte("x")}, 90*time.Second); err != nil {
			t.Fatal(err)
		}
		ttl, err := client.PTTL(ctx, key).Result()
		if err != nil || ttl <= 89*time.Second || ttl > 90*time.Second {
			t.Errorf("PTTL = %s, %v; want just under 90s", ttl, err)
		}
		// An expired key reads as a miss: PEXPIREAT in the past deletes it.
		if err := client.PExpireAt(ctx, key, time.Unix(1, 0)).Err(); err != nil {
			t.Fatal(err)
		}
		if e, err := s.Get(ctx, key); e != nil || err != nil {
			t.Errorf("Get after expiry = %v, %v; want a miss", e, err)
		}
	})

	t.Run("namespace key has no ttl", func(t *testing.T) {
		ctx := context.Background()
		tenant := randomPrefix()
		t.Cleanup(func() { deleteMatching(t, client, "sy:ns:v1:"+tenant) })
		if _, err := NewRedisNamespaces(client).Bump(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		if ttl, err := client.PTTL(ctx, "sy:ns:v1:"+tenant).Result(); err != nil || ttl != -1 {
			t.Errorf("PTTL = %s, %v; want -1 (no expiry)", ttl, err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if _, err := NewRedisStore(client).Get(ctx, "sy:x:v1:"+randomPrefix()); err == nil {
			t.Error("Get succeeded after its deadline")
		}
	})
}
