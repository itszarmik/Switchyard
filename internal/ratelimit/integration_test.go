//go:build integration

package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"switchyard/internal/config"
)

// TestRedisIntegration runs the limiter against the real Redis named by
// SWITCHYARD_TEST_REDIS_URL (for example the compose service redis-state:
// redis://localhost:6380/15), as two replicas would: two limiters, each
// with its own client. Every subtest uses its own random key prefix and
// deletes its keys afterwards, so it can share the server with other
// tests. The Redis server's clock runs in real time, so the limits are
// chosen to leave seconds of slack.
func TestRedisIntegration(t *testing.T) {
	url := os.Getenv("SWITCHYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SWITCHYARD_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("SWITCHYARD_TEST_REDIS_URL is not a valid Redis URL") // never echo it: it may hold a password
	}
	newClient := func(t *testing.T) *redis.Client {
		c := redis.NewClient(opts)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	admin := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis is not reachable: %v", err)
	}
	// replicas returns two limiters sharing a fresh key prefix.
	replicas := func(t *testing.T) (a, b *RedisLimiter, prefix string) {
		var r [8]byte
		_, _ = rand.Read(r[:])
		prefix = "sy:test-" + hex.EncodeToString(r[:]) + ":"
		t.Cleanup(func() { deletePrefix(t, admin, prefix) })
		a = newRedis(newClient(t), RedisOptions{LeaseTTL: 30 * time.Second}, prefix)
		b = newRedis(newClient(t), RedisOptions{LeaseTTL: 30 * time.Second}, prefix)
		t.Cleanup(a.Close)
		t.Cleanup(b.Close)
		return a, b, prefix
	}

	t.Run("request rate is shared", func(t *testing.T) {
		a, b, prefix := replicas(t)
		lim := config.RateLimit{RequestsPerMinute: 6, Burst: 2} // T = 10 s: three at once, then one per 10 s
		for i := range 3 {
			r := []*RedisLimiter{a, b}[i%2]
			if ok, _, err := r.Allow(ctx, "k1", lim); !ok || err != nil {
				t.Fatalf("request %d: %v, %v", i, ok, err)
			}
		}
		for i, r := range []*RedisLimiter{a, b} {
			ok, wait, err := r.Allow(ctx, "k1", lim)
			if ok || err != nil || wait <= 0 || wait > 10*time.Second {
				t.Errorf("replica %d over the shared limit: %v, %v, %v", i, ok, wait, err)
			}
		}
		if ok, _, err := b.Allow(ctx, "k2", lim); !ok || err != nil {
			t.Errorf("another key: %v, %v", ok, err)
		}
		// The TAT is stored as an integer, and the key expires within
		// the burst window.
		key := prefix + "rl:v1:k1"
		v, err := admin.Get(ctx, key).Result()
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			t.Errorf("TAT %q is not an integer", v)
		}
		if ttl := admin.PTTL(ctx, key).Val(); ttl <= 20*time.Second || ttl > 30*time.Second {
			t.Errorf("PTTL = %v, want in (20s, 30s]", ttl)
		}
	})

	t.Run("stream cap is shared", func(t *testing.T) {
		a, b, _ := replicas(t)
		r1, ok, err := a.AcquireStream(ctx, "k1", 2)
		if !ok || err != nil {
			t.Fatalf("first stream: %v, %v", ok, err)
		}
		r2, ok, err := b.AcquireStream(ctx, "k1", 2)
		if !ok || err != nil {
			t.Fatalf("second stream: %v, %v", ok, err)
		}
		if _, ok, _ := a.AcquireStream(ctx, "k1", 2); ok {
			t.Fatal("third stream allowed under a shared cap of 2")
		}
		r1()
		r1()
		r3, ok, _ := b.AcquireStream(ctx, "k1", 2)
		if !ok {
			t.Fatal("stream refused after a release")
		}
		if _, ok, _ := a.AcquireStream(ctx, "k1", 2); ok {
			t.Fatal("a double release freed a second slot")
		}
		r2()
		r3()
	})

	t.Run("a crashed replica's expired lease frees its slot", func(t *testing.T) {
		a, b, _ := replicas(t)
		key := a.streamKey("k1")
		// The lease of a replica that died a minute ago, past its expiry.
		expired := float64(time.Now().Add(-time.Minute).UnixMilli())
		if err := admin.ZAdd(ctx, key, redis.Z{Score: expired, Member: "crashed"}).Err(); err != nil {
			t.Fatalf("ZADD: %v", err)
		}
		release, ok, err := b.AcquireStream(ctx, "k1", 1)
		if !ok || err != nil {
			t.Fatalf("slot still taken by an expired lease: %v, %v", ok, err)
		}
		defer release()
		if _, err := admin.ZScore(ctx, key, "crashed").Result(); !errors.Is(err, redis.Nil) {
			t.Errorf("the expired lease is still there: %v", err)
		}
		// A live lease is renewed, not revived once expired.
		members := admin.ZRange(ctx, key, 0, -1).Val()
		if len(members) != 1 {
			t.Fatalf("leases = %v", members)
		}
		before := admin.ZScore(ctx, key, members[0]).Val()
		if alive, err := b.renew(key, members[0]); !alive || err != nil {
			t.Errorf("renew: %v, %v", alive, err)
		}
		if after := admin.ZScore(ctx, key, members[0]).Val(); after < before {
			t.Errorf("renewed expiry %v before the old one %v", after, before)
		}
		if alive, err := a.renew(key, "crashed"); alive || err != nil {
			t.Errorf("renew of a removed lease: %v, %v", alive, err)
		}
	})
}

// deletePrefix deletes every key starting with prefix.
func deletePrefix(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	iter := client.Scan(ctx, 0, prefix+"*", 1000).Iterator()
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
