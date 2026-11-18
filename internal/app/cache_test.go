package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"switchyard/internal/cache"
	"switchyard/internal/config"
)

func TestCachingEnabled(t *testing.T) {
	on := config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}}
	sem := config.CachePolicy{Semantic: config.SemanticCachePolicy{Enabled: true}}
	tests := []struct {
		name          string
		policies      []config.Policy
		any, semantic bool
	}{
		{"none", []config.Policy{{ID: "p"}}, false, false},
		{"exact", []config.Policy{{ID: "p"}, {ID: "q", Cache: on}}, true, false},
		{"semantic", []config.Policy{{ID: "p", Cache: sem}}, true, true},
		{"a rule's exact", []config.Policy{{ID: "p", Rules: []config.Rule{{Set: config.PolicyOverride{Cache: &on}}}}}, true, false},
		{"a rule's semantic", []config.Policy{{ID: "p", Rules: []config.Rule{{}, {Set: config.PolicyOverride{Cache: &sem}}}}}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Policies: tt.policies}
			if got := cachingEnabled(cfg, false); got != tt.any {
				t.Errorf("cachingEnabled = %v, want %v", got, tt.any)
			}
			if got := semanticEnabled(cfg); got != tt.semantic {
				t.Errorf("semanticEnabled = %v, want %v", got, tt.semantic)
			}
		})
	}
}

// TestCacheStorage: memory mode, or no policy caching, keeps the caches in
// process; redis mode puts entries in the cache instance and namespace
// versions in the state instance, and starts with the cache instance down.
func TestCacheStorage(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	exact := []config.Policy{{ID: "p", Cache: config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}}}}
	redisCfg := func(cacheURL string, policies []config.Policy) *config.Config {
		c := &config.Config{Policies: policies, Cache: config.CacheConfig{MemoryMaxEntries: 5}}
		c.Storage.Mode = "redis"
		c.Storage.Redis.CacheURL = config.NewSecret(cacheURL)
		return c
	}

	t.Run("memory", func(t *testing.T) {
		for _, cfg := range []*config.Config{{Policies: exact}, redisCfg("redis://127.0.0.1:1/0", nil)} {
			opts, client, err := newCacheStorage(cfg, nil, logger)
			if err != nil || client != nil {
				t.Fatalf("newCacheStorage: %v, client %v", err, client)
			}
			if _, ok := opts.Store.(*cache.MemoryStore); !ok {
				t.Errorf("store %T, want memory", opts.Store)
			}
			if _, ok := opts.Namespaces.(*cache.MemoryNamespaces); !ok {
				t.Errorf("namespaces %T, want memory", opts.Namespaces)
			}
		}
	})

	t.Run("redis", func(t *testing.T) {
		cacheRedis, stateRedis := miniredis.RunT(t), miniredis.RunT(t)
		state := redis.NewClient(&redis.Options{Addr: stateRedis.Addr()})
		t.Cleanup(func() { _ = state.Close() })
		opts, client, err := newCacheStorage(redisCfg("redis://"+cacheRedis.Addr()+"/0", exact), state, logger)
		if err != nil || client == nil {
			t.Fatalf("newCacheStorage: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if o := client.Options(); !o.ContextTimeoutEnabled || o.DialTimeout != cacheRedisDialTimeout || o.MaxRetries != 0 { // go-redis stores "no retries" (-1) as 0
			t.Errorf("cache client options: context timeouts %v, dial timeout %s, retries %d", o.ContextTimeoutEnabled, o.DialTimeout, o.MaxRetries)
		}
		ctx := context.Background()
		if err := opts.Store.Set(ctx, "k", &cache.Entry{Body: []byte(`{}`)}, time.Minute); err != nil {
			t.Fatal(err)
		}
		if !cacheRedis.Exists("k") || stateRedis.Exists("k") {
			t.Errorf("the entry is not in the cache instance alone")
		}
		if _, err := opts.Namespaces.Bump(ctx, "t1"); err != nil {
			t.Fatal(err)
		}
		if !stateRedis.Exists("sy:ns:v1:t1") || cacheRedis.Exists("sy:ns:v1:t1") {
			t.Errorf("the namespace version is not in the state instance alone")
		}
	})

	t.Run("cache instance down at startup", func(t *testing.T) {
		mr := miniredis.RunT(t)
		addr := mr.Addr()
		mr.Close()
		logs := newLogBuffer()
		_, client, err := newCacheStorage(redisCfg("redis://"+addr+"/0", exact), nil, slog.New(slog.NewJSONHandler(logs, nil)))
		if err != nil {
			t.Fatalf("newCacheStorage with the cache Redis down: %v", err)
		}
		_ = client.Close()
		if !strings.Contains(logs.String(), "the Redis cache instance is unreachable") {
			t.Errorf("no warning logged:\n%s", logs.String())
		}
	})

	t.Run("an invalid URL is never echoed", func(t *testing.T) {
		_, _, err := newCacheStorage(redisCfg("redis://:hunter2@host:notaport/0", exact), nil, logger)
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error %v", err)
		}
	})
}

// TestCacheRedisTimeouts: the cache client's socket timeouts never cut a
// cache call short of its context's deadline, so a configured
// cache.lookup_timeout or semantic_cache.lookup_timeout above 200 ms, and
// the 1 s write timeout, take effect (correctness review M4).
func TestCacheRedisTimeouts(t *testing.T) {
	exact := []config.Policy{{ID: "p", Cache: config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}}}}
	cfg := func(lookup, semantic time.Duration) *config.Config {
		c := &config.Config{Policies: exact}
		c.Cache.LookupTimeout, c.SemanticCache.LookupTimeout = config.Duration(lookup), config.Duration(semantic)
		return c
	}
	tests := []struct {
		name             string
		lookup, semantic time.Duration
		want             time.Duration
	}{
		{"defaults: the write timeout", 25 * time.Millisecond, 150 * time.Millisecond, time.Second + cacheRedisIOMargin},
		{"a long exact lookup", time.Second, 150 * time.Millisecond, time.Second + cacheRedisIOMargin},
		{"a long semantic lookup", 25 * time.Millisecond, 5 * time.Second, 5*time.Second + cacheRedisIOMargin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheRedisIOTimeout(cfg(tt.lookup, tt.semantic)); got != tt.want {
				t.Errorf("cacheRedisIOTimeout = %s, want %s", got, tt.want)
			}
		})
	}

	t.Run("a hung Redis takes the lookup timeout", func(t *testing.T) {
		const timeout = 400 * time.Millisecond
		c := cfg(timeout, 150*time.Millisecond)
		c.Storage.Mode = "redis"
		c.Storage.Redis.CacheURL = config.NewSecret("redis://" + blackhole(t) + "/0")
		opts, client, err := newCacheStorage(c, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if o := client.Options(); o.ReadTimeout != time.Second+cacheRedisIOMargin || o.WriteTimeout != o.ReadTimeout {
			t.Errorf("read timeout %s, write timeout %s", o.ReadTimeout, o.WriteTimeout)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		start := time.Now()
		_, err = opts.Store.Get(ctx, "k")
		took := time.Since(start)
		// The deadline cannot pass early; a 200 ms socket timeout would
		// have failed the call at about half of it.
		if err == nil || took < timeout {
			t.Errorf("Get on a hung Redis: %v after %s, want a failure after the %s lookup timeout", err, took, timeout)
		}
		if took > timeout+time.Second {
			t.Errorf("Get on a hung Redis took %s, want about %s", took, timeout)
		}
	})
}
