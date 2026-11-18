package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore is a Store in the cache Redis instance, which should run with
// maxmemory and allkeys-lru (DESIGN §11.5). Each entry is one string key,
// the exact key, holding MarshalEntry's encoding, set with an expiry.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore returns a Store over client, the cache instance. The
// client must be built with ContextTimeoutEnabled set, so that every call
// honors its context's deadline: the gateway bounds lookups with
// cache.lookup_timeout, and a hung Redis must not hold a request longer.
func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

// Get returns the entry under key, or nil on a miss. An entry of another
// encoding version is a miss too; one that does not decode is an error,
// which callers treat as a miss.
func (s *RedisStore) Get(ctx context.Context, key string) (*Entry, error) {
	data, err := s.client.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("cache: redis get: %w", err)
	}
	e, err := UnmarshalEntry(data)
	switch {
	case errors.Is(err, ErrEntryVersion):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return e, nil
}

// Set stores e under key with SET … PX ttl. A ttl below a millisecond is
// rounded up to one; a ttl that is not positive is an error.
func (s *RedisStore) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cache: redis set: ttl %s is not positive", ttl)
	}
	ttl = max(ttl, time.Millisecond)
	if err := s.client.Do(ctx, "SET", key, MarshalEntry(e), "PX", ttl.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}

// RedisNamespaces holds namespace versions in the state Redis instance,
// which runs with noeviction alongside the budgets: an evicted version
// would bring purged entries back. Each version is the key
// sy:ns:v1:<tenant>, with no expiry; a missing key is version 0.
type RedisNamespaces struct {
	client *redis.Client
}

// NewRedisNamespaces returns Namespaces over client, the state instance.
// As for NewRedisStore, the client must be built with
// ContextTimeoutEnabled set.
func NewRedisNamespaces(client *redis.Client) *RedisNamespaces {
	return &RedisNamespaces{client: client}
}

// Version returns tenant's namespace version, 0 if it was never bumped.
func (n *RedisNamespaces) Version(ctx context.Context, tenant string) (int64, error) {
	s, err := n.client.Get(ctx, namespaceKey(tenant)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("cache: redis namespace version: %w", err)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cache: redis namespace version is not an integer: %w", err)
	}
	return v, nil
}

// Bump increments tenant's namespace version with INCR and returns the new
// version.
func (n *RedisNamespaces) Bump(ctx context.Context, tenant string) (int64, error) {
	v, err := n.client.Incr(ctx, namespaceKey(tenant)).Result()
	if err != nil {
		return 0, fmt.Errorf("cache: redis namespace bump: %w", err)
	}
	return v, nil
}

// Compile-time checks.
var (
	_ Store      = (*RedisStore)(nil)
	_ Namespaces = (*RedisNamespaces)(nil)
)
