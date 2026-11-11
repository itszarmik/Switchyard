//go:build integration

package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRedisStoreIntegration runs the Store contract against the real Redis
// named by SWITCHYARD_TEST_REDIS_URL (for example the compose service
// redis-state: redis://localhost:6380/15). Every subtest uses its own random
// key prefix, and deletes its keys afterwards, so it can share the server
// with other tests.
func TestRedisStoreIntegration(t *testing.T) {
	url := os.Getenv("SWITCHYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SWITCHYARD_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("SWITCHYARD_TEST_REDIS_URL is not a valid Redis URL") // never echo it: it may hold a password
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis is not reachable: %v", err)
	}

	testContract(t, func(t *testing.T) Store {
		var b [8]byte
		_, _ = rand.Read(b[:])
		prefix := "sy:b:test-" + hex.EncodeToString(b[:]) + ":"
		t.Cleanup(func() { deletePrefix(t, client, prefix) })
		return newRedisStore(client, prefix)
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
