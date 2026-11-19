package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"switchyard/internal/config"
)

// keyPrefix starts every key of the Redis limiter.
const keyPrefix = "sy:"

// Lease settings of RedisLimiter.
const (
	// DefaultLeaseTTL is how long a stream lease lives without a
	// heartbeat. A heartbeat renews it every third of that.
	DefaultLeaseTTL = 60 * time.Second
	// minLeaseTTL is the shortest lease TTL.
	minLeaseTTL = time.Second
	// leaseCallTimeout bounds each heartbeat and release.
	leaseCallTimeout = 500 * time.Millisecond
)

// gcraScript is Allow, atomically. KEYS[1] holds the key's theoretical
// arrival time (TAT) in microseconds since the Unix epoch. ARGV[1] is the
// emission interval and ARGV[2] the tolerance, in microseconds. The clock
// is the Redis server's (TIME), so every replica uses the same one.
//
// It returns {1, 0} when the request is allowed, having moved the TAT and
// set the key to expire when the TAT passes (at most the burst window,
// tolerance + interval, from now); or {0, wait}, having changed nothing
// but the clamp below, where wait is the microseconds until a request
// would be allowed.
//
// A TAT more than tolerance + interval ahead, which no request under the
// current limit can leave, was left by a slower limit before a reload; it
// is clamped to that, and stored even when the request is refused, so a
// raised limit applies within one new interval.
//
// Lua numbers are doubles, exact for integers below 2^53: the time in
// microseconds is about 2^51 today. string.format('%d') writes the TAT
// without the exponent that tostring would use for a number that large.
const gcraScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local tat = tonumber(redis.call('GET', KEYS[1]) or '0')
if tat < now then
  tat = now
end
if tat > now + tolerance + interval then
  tat = now + tolerance + interval
  redis.call('SET', KEYS[1], string.format('%d', tat), 'PX', string.format('%d', math.ceil((tat - now) / 1000)))
end
local wait = tat - tolerance - now
if wait > 0 then
  return {0, wait}
end
local new = tat + interval
redis.call('SET', KEYS[1], string.format('%d', new), 'PX', string.format('%d', math.ceil((new - now) / 1000)))
return {1, 0}
`

// acquireScript takes a stream lease. KEYS[1] is the key's lease set: a
// sorted set whose members are lease IDs, scored by their expiry in
// milliseconds since the Unix epoch on the Redis server's clock. ARGV[1]
// is the cap, ARGV[2] the lease TTL in milliseconds, and ARGV[3] the new
// lease's ID.
//
// It drops the expired leases, and adds the new one if fewer than the cap
// remain, returning 1; otherwise it returns 0. The set expires one TTL
// after its last write, by when every lease in it has expired.
const acquireScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', string.format('%d', now))
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], string.format('%d', now + tonumber(ARGV[2])), ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`

// heartbeatScript renews a lease. KEYS[1] is the lease set, ARGV[1] the
// lease TTL in milliseconds and ARGV[2] the lease ID. It returns 1, or 0
// when the lease has expired or is gone, in which case it is not revived:
// its slot may have been taken since.
const heartbeatScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local score = redis.call('ZSCORE', KEYS[1], ARGV[2])
if not score or tonumber(score) <= now then
  return 0
end
redis.call('ZADD', KEYS[1], string.format('%d', now + tonumber(ARGV[1])), ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return 1
`

// errClosed is returned by AcquireStream after Close.
var errClosed = fmt.Errorf("ratelimit acquire: %w: the limiter is closed", ErrUnavailable)

// RedisOptions configures NewRedis. Zero values take the defaults.
type RedisOptions struct {
	// LeaseTTL is how long a stream lease lives without a heartbeat
	// (DefaultLeaseTTL; at least a second). Every lease is renewed every
	// LeaseTTL/3 until it is released, so a replica that dies leaks its
	// slots for at most LeaseTTL.
	LeaseTTL time.Duration
}

// RedisLimiter is a Limiter in Redis, shared by every replica.
//
// The request rate of a key is one GCRA arrival time, at
// sy:rl:v1:{escaped key ID}, which expires when it no longer matters. Its
// streams are the sorted set sy:rs:v1:{escaped key ID} of leases, each
// scored by its expiry. Each decision is one Lua script on one key, timed
// by the Redis server's clock, so it is atomic and replicas agree on it.
//
// Every lease has a goroutine that renews it until it is released, and
// then removes it. Close releases the leases still held.
type RedisLimiter struct {
	client    redis.UniversalClient
	prefix    string
	leaseTTL  time.Duration
	gcra      *redis.Script
	acquire   *redis.Script
	heartbeat *redis.Script
	// newTicker returns the channel of a ticker of period d and its stop
	// function; tests replace it.
	newTicker func(d time.Duration) (<-chan time.Time, func())

	closing chan struct{}
	mu      sync.Mutex // guards closed and wg.Add against Close
	closed  bool
	wg      sync.WaitGroup
}

var _ Limiter = (*RedisLimiter)(nil)

// NewRedis returns a Limiter that keeps its state in client, which should
// be the non-evicting state instance (DESIGN §11.5). It needs Redis 5 or
// later, for TIME in scripts. Each script uses one key, so a Redis Cluster
// works too.
func NewRedis(client redis.UniversalClient, o RedisOptions) *RedisLimiter {
	return newRedis(client, o, keyPrefix)
}

// newRedis returns a RedisLimiter whose keys start with prefix, so tests
// sharing a server do not collide.
func newRedis(client redis.UniversalClient, o RedisOptions, prefix string) *RedisLimiter {
	switch {
	case o.LeaseTTL <= 0:
		o.LeaseTTL = DefaultLeaseTTL
	case o.LeaseTTL < minLeaseTTL:
		o.LeaseTTL = minLeaseTTL
	}
	return &RedisLimiter{
		client:    client,
		prefix:    prefix,
		leaseTTL:  o.LeaseTTL,
		gcra:      redis.NewScript(gcraScript),
		acquire:   redis.NewScript(acquireScript),
		heartbeat: redis.NewScript(heartbeatScript),
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		closing: make(chan struct{}),
	}
}

// rateKey returns the GCRA key of keyID.
func (r *RedisLimiter) rateKey(keyID string) string {
	return r.prefix + "rl:v1:" + url.QueryEscape(keyID)
}

// streamKey returns the lease set of keyID.
func (r *RedisLimiter) streamKey(keyID string) string {
	return r.prefix + "rs:v1:" + url.QueryEscape(keyID)
}

// Allow implements Limiter.
func (r *RedisLimiter) Allow(ctx context.Context, keyID string, lim config.RateLimit) (bool, time.Duration, error) {
	if lim.RequestsPerMinute <= 0 {
		return true, 0, nil
	}
	interval, tolerance := gcra(lim)
	res, err := r.gcra.Run(ctx, r.client, []string{r.rateKey(keyID)},
		interval.Microseconds(), tolerance.Microseconds()).Int64Slice()
	switch {
	case err != nil:
		return false, 0, unavailable(ctx, "allow", err)
	case len(res) != 2 || res[0] < 0 || res[0] > 1 || res[1] < 0:
		return false, 0, unavailable(ctx, "allow", errors.New("unexpected script result"))
	case res[0] == 1:
		return true, 0, nil
	}
	return false, time.Duration(res[1]) * time.Microsecond, nil
}

// AcquireStream implements Limiter. The lease is renewed every LeaseTTL/3
// until release, which removes it before returning, so that a client that
// opens a stream as soon as its last one ended finds the slot free. If
// Redis does not answer within half a second, release gives up and the
// lease expires on its own.
func (r *RedisLimiter) AcquireStream(ctx context.Context, keyID string, maxStreams int) (func(), bool, error) {
	if maxStreams <= 0 {
		return noRelease, true, nil
	}
	if !r.track() {
		return nil, false, errClosed
	}
	lease, err := newLeaseID()
	if err != nil {
		r.wg.Done()
		return nil, false, fmt.Errorf("ratelimit acquire: %w", err)
	}
	key := r.streamKey(keyID)
	ok, err := r.take(ctx, key, lease, maxStreams)
	if err != nil || !ok {
		r.wg.Done()
		return nil, false, err
	}
	done, removed := make(chan struct{}), make(chan struct{})
	go r.hold(key, lease, done, removed)
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-removed
		})
	}, true, nil
}

// track counts a lease goroutine about to start, unless the limiter is
// closed.
func (r *RedisLimiter) track() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.wg.Add(1)
	return true
}

// take runs acquireScript for lease in the lease set key, under the cap
// maxStreams.
func (r *RedisLimiter) take(ctx context.Context, key, lease string, maxStreams int) (bool, error) {
	n, err := r.acquire.Run(ctx, r.client, []string{key}, maxStreams, r.leaseTTL.Milliseconds(), lease).Int64()
	switch {
	case err != nil:
		return false, unavailable(ctx, "acquire", err)
	case n != 0 && n != 1:
		return false, unavailable(ctx, "acquire", errors.New("unexpected script result"))
	}
	return n == 1, nil
}

// hold renews lease until done is closed or the limiter is closed, then
// removes it and closes removed. Once a heartbeat finds the lease expired,
// it stops renewing it.
func (r *RedisLimiter) hold(key, lease string, done <-chan struct{}, removed chan<- struct{}) {
	defer r.wg.Done()
	defer close(removed)
	tick, stop := r.newTicker(r.leaseTTL / 3)
	defer stop()
	for {
		select {
		case <-done:
			r.remove(key, lease)
			return
		case <-r.closing:
			r.remove(key, lease)
			return
		case <-tick:
			// A failed heartbeat is retried at the next tick; the lease
			// outlives two missed ones.
			if alive, err := r.renew(key, lease); err == nil && !alive {
				tick = nil
			}
		}
	}
}

// renew runs heartbeatScript for lease and reports whether it was still
// held.
func (r *RedisLimiter) renew(key, lease string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseCallTimeout)
	defer cancel()
	n, err := r.heartbeat.Run(ctx, r.client, []string{key}, r.leaseTTL.Milliseconds(), lease).Int64()
	if err != nil {
		return false, unavailable(ctx, "heartbeat", err)
	}
	return n == 1, nil
}

// remove deletes lease from the lease set key. If Redis is down, the lease
// expires on its own.
func (r *RedisLimiter) remove(key, lease string) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseCallTimeout)
	defer cancel()
	_ = r.client.ZRem(ctx, key, lease).Err()
}

// Close releases every lease still held, and waits until they are removed
// or their removal has timed out. AcquireStream fails after Close; Allow
// still works. Call it before closing the Redis client.
func (r *RedisLimiter) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.closing)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// newLeaseID returns a random lease ID: 16 bytes from crypto/rand in hex.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
