package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"switchyard/internal/auth"
	"switchyard/internal/config"
)

// newMiniredis starts a miniredis server whose clock (TIME) reads t0, and
// returns it with a client.
func newMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(t0)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// advancer returns a function that moves mr's clock and its key TTLs on
// together.
func advancer(mr *miniredis.Miniredis) func(d time.Duration) {
	now := t0
	return func(d time.Duration) {
		now = now.Add(d)
		mr.SetTime(now)
		mr.FastForward(d)
	}
}

// newTestRedis returns a RedisLimiter on client that Close()s at the end of
// the test.
func newTestRedis(t *testing.T, client redis.UniversalClient, o RedisOptions) *RedisLimiter {
	t.Helper()
	r := NewRedis(client, o)
	t.Cleanup(r.Close)
	return r
}

func TestRedisContract(t *testing.T) {
	testContract(t, func(t *testing.T) harness {
		mr, client := newMiniredis(t)
		return harness{l: newTestRedis(t, client, RedisOptions{}), advance: advancer(mr)}
	})
}

// TestRedisShared checks that two limiters, as on two replicas, on one
// Redis share the request rate and the stream cap.
func TestRedisShared(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	advance := advancer(mr)
	other := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = other.Close() })
	replicas := []*RedisLimiter{newTestRedis(t, client, RedisOptions{}), newTestRedis(t, other, RedisOptions{})}
	lim := config.RateLimit{RequestsPerMinute: 60, Burst: 3}

	// Four requests at once (b+1) in all, alternating between replicas.
	for i := range 4 {
		if ok, _, err := replicas[i%2].Allow(ctx, "k1", lim); !ok || err != nil {
			t.Fatalf("request %d: %v, %v", i, ok, err)
		}
	}
	for i, r := range replicas {
		if ok, wait, err := r.Allow(ctx, "k1", lim); ok || wait != time.Second || err != nil {
			t.Errorf("replica %d over the shared limit: %v, %v, %v", i, ok, wait, err)
		}
	}
	advance(time.Second)
	if ok, _, _ := replicas[1].Allow(ctx, "k1", lim); !ok {
		t.Error("refused after one interval")
	}
	if ok, _, _ := replicas[0].Allow(ctx, "k1", lim); ok {
		t.Error("the other replica allowed a second request in the same interval")
	}

	release, ok, err := replicas[0].AcquireStream(ctx, "k1", 1)
	if !ok || err != nil {
		t.Fatalf("first stream: %v, %v", ok, err)
	}
	if _, ok, _ := replicas[1].AcquireStream(ctx, "k1", 1); ok {
		t.Fatal("the other replica opened a second stream under a cap of 1")
	}
	release()
	release2, ok, _ := replicas[1].AcquireStream(ctx, "k1", 1)
	if !ok {
		t.Fatal("the other replica's stream refused after the release")
	}
	release2()
}

// TestRedisKeys checks the keys, values and expiries the limiter writes.
func TestRedisKeys(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	r := newTestRedis(t, client, RedisOptions{LeaseTTL: 30 * time.Second})
	lim := config.RateLimit{RequestsPerMinute: 60, Burst: 2}
	for range 2 {
		if ok, _, err := r.Allow(ctx, "team:a/b", lim); !ok || err != nil {
			t.Fatalf("Allow: %v, %v", ok, err)
		}
	}
	const rateKey = "sy:rl:v1:team%3Aa%2Fb"
	got, err := mr.Get(rateKey)
	if err != nil {
		t.Fatalf("GET %s: %v (keys %v)", rateKey, err, mr.Keys())
	}
	// Two requests at t0 move the arrival time two intervals on, written
	// as an integer, and the key lives until then.
	if want := strconv.FormatInt(t0.Add(2*time.Second).UnixMicro(), 10); got != want {
		t.Errorf("TAT = %q, want %q", got, want)
	}
	if ttl := mr.TTL(rateKey); ttl != 2*time.Second {
		t.Errorf("TTL %s = %v, want 2s", rateKey, ttl)
	}

	release, ok, err := r.AcquireStream(ctx, "team:a/b", 2)
	if !ok || err != nil {
		t.Fatalf("AcquireStream: %v, %v", ok, err)
	}
	const streamKey = "sy:rs:v1:team%3Aa%2Fb"
	members, err := mr.ZMembers(streamKey)
	if err != nil || len(members) != 1 || len(members[0]) != 32 {
		t.Fatalf("ZMEMBERS %s = %v, %v; want one 32-hex-digit lease", streamKey, members, err)
	}
	score, _ := mr.ZScore(streamKey, members[0])
	if want := float64(t0.Add(30 * time.Second).UnixMilli()); score != want {
		t.Errorf("lease expiry = %v, want %v", score, want)
	}
	if ttl := mr.TTL(streamKey); ttl != 30*time.Second {
		t.Errorf("TTL %s = %v, want 30s", streamKey, ttl)
	}
	release()
	if mr.Exists(streamKey) {
		if m, _ := mr.ZMembers(streamKey); len(m) != 0 {
			t.Errorf("leases after release: %v", m)
		}
	}
}

// TestRedisLeaseExpiry checks that a lease whose holder stops renewing it,
// as when its replica crashes, frees its slot after the lease TTL, and not
// before.
func TestRedisLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	advance := advancer(mr)
	r := newTestRedis(t, client, RedisOptions{LeaseTTL: 60 * time.Second})
	key := r.streamKey("k1")
	// A crashed replica's lease: taken, but with nobody to renew or
	// release it.
	if ok, err := r.take(ctx, key, "crashed", 1); !ok || err != nil {
		t.Fatalf("take: %v, %v", ok, err)
	}
	advance(60*time.Second - time.Millisecond)
	if _, ok, _ := r.AcquireStream(ctx, "k1", 1); ok {
		t.Fatal("slot free before the lease expired")
	}
	advance(time.Millisecond)
	release, ok, err := r.AcquireStream(ctx, "k1", 1)
	if !ok || err != nil {
		t.Fatalf("slot still taken after the lease expired: %v, %v", ok, err)
	}
	defer release()
	if m, _ := mr.ZMembers(key); slices.Contains(m, "crashed") {
		t.Errorf("the expired lease is still in the set: %v", m)
	}
}

// manualTicker replaces a RedisLimiter's tickers with one unbuffered
// channel the test sends ticks on. A send returns once a lease goroutine
// has received the tick; the next send returns only after that goroutine
// has finished the heartbeat and is waiting again.
func manualTicker(r *RedisLimiter) chan time.Time {
	ch := make(chan time.Time)
	r.newTicker = func(time.Duration) (<-chan time.Time, func()) { return ch, func() {} }
	return ch
}

// TestRedisHeartbeat checks that a held lease is renewed for another TTL
// at every tick, and stays held past its first expiry.
func TestRedisHeartbeat(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	advance := advancer(mr)
	r := newTestRedis(t, client, RedisOptions{LeaseTTL: 30 * time.Second})
	tick := manualTicker(r)
	release, ok, err := r.AcquireStream(ctx, "k1", 1)
	if !ok || err != nil {
		t.Fatalf("AcquireStream: %v, %v", ok, err)
	}
	key := r.streamKey("k1")
	lease := func() string {
		m, _ := mr.ZMembers(key)
		if len(m) != 1 {
			t.Fatalf("leases = %v, want 1", m)
		}
		return m[0]
	}
	id := lease()

	// Three heartbeats, 20 s apart, keep the lease 30 s beyond each.
	for i := range 3 {
		advance(20 * time.Second)
		tick <- time.Time{}
		tick <- time.Time{} // returns once the first heartbeat is done
		score, _ := mr.ZScore(key, id)
		if want := float64(t0.Add(time.Duration(i+1)*20*time.Second + 30*time.Second).UnixMilli()); score != want {
			t.Fatalf("heartbeat %d: expiry %v, want %v", i, score, want)
		}
		if ttl := mr.TTL(key); ttl != 30*time.Second {
			t.Fatalf("heartbeat %d: set TTL %v, want 30s", i, ttl)
		}
	}
	if _, ok, _ := r.AcquireStream(ctx, "k1", 1); ok {
		t.Fatal("a renewed lease's slot was given away")
	}
	release()
	if m, _ := mr.ZMembers(key); len(m) != 0 {
		t.Errorf("leases after release: %v", m)
	}
}

// TestRedisHeartbeatLost checks that a heartbeat does not revive a lease
// that expired, for example while Redis was unreachable, since its slot
// may have been taken since.
func TestRedisHeartbeatLost(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	r := newTestRedis(t, client, RedisOptions{LeaseTTL: 30 * time.Second})
	key := r.streamKey("k1")
	if ok, err := r.take(ctx, key, "old", 1); !ok || err != nil {
		t.Fatalf("take: %v, %v", ok, err)
	}
	mr.SetTime(t0.Add(30 * time.Second))
	if alive, err := r.renew(key, "old"); alive || err != nil {
		t.Errorf("renew of an expired lease = %v, %v; want false, nil", alive, err)
	}
	if score, _ := mr.ZScore(key, "old"); score != float64(t0.Add(30*time.Second).UnixMilli()) {
		t.Errorf("the expired lease was renewed to %v", score)
	}
	if alive, err := r.renew(key, "gone"); alive || err != nil {
		t.Errorf("renew of a missing lease = %v, %v", alive, err)
	}
	if m, _ := mr.ZMembers(key); slices.Contains(m, "gone") {
		t.Errorf("a missing lease was added: %v", m)
	}
}

// TestRedisClose checks that Close removes the leases still held and makes
// AcquireStream fail, while a release after Close is harmless.
func TestRedisClose(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	r := NewRedis(client, RedisOptions{})
	release, ok, err := r.AcquireStream(ctx, "k1", 5)
	if !ok || err != nil {
		t.Fatalf("AcquireStream: %v, %v", ok, err)
	}
	r.Close()
	if m, _ := mr.ZMembers(r.streamKey("k1")); len(m) != 0 {
		t.Errorf("leases after Close: %v", m)
	}
	release()
	r.Close()
	if _, _, err := r.AcquireStream(ctx, "k1", 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("AcquireStream after Close: %v", err)
	}
	if ok, _, err := r.Allow(ctx, "k1", config.RateLimit{RequestsPerMinute: 1}); !ok || err != nil {
		t.Errorf("Allow after Close: %v, %v", ok, err)
	}
}

// TestRedisUnavailable checks that a Redis that is down fails every call
// with ErrUnavailable, and a canceled context with its own error.
func TestRedisUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	// No retries, so that each call fails at once.
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = client.Close() })
	r := newTestRedis(t, client, RedisOptions{})
	mr.Close()
	ctx := context.Background()
	lim := config.RateLimit{RequestsPerMinute: 60}
	if _, _, err := r.Allow(ctx, "k1", lim); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Allow: %v", err)
	}
	if release, _, err := r.AcquireStream(ctx, "k1", 1); !errors.Is(err, ErrUnavailable) || release != nil {
		t.Errorf("AcquireStream: %v, nil release %v", err, release == nil)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := r.Allow(canceled, "k1", lim); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("Allow with a canceled context: %v", err)
	}
	// Nothing is asked of Redis without a limit.
	if ok, _, err := r.Allow(ctx, "k1", config.RateLimit{}); !ok || err != nil {
		t.Errorf("Allow without a rate: %v, %v", ok, err)
	}
	if release, ok, err := r.AcquireStream(ctx, "k1", 0); !ok || err != nil || release == nil {
		t.Errorf("AcquireStream without a cap: %v, %v", ok, err)
	}
}

// TestRedisTwoReplicasHTTP checks the P7 criterion end to end: two
// Enforcers, as on two replicas, each with its own RedisLimiter and Redis
// client on one Redis, enforce one request rate and one stream cap per key
// between them.
func TestRedisTwoReplicasHTTP(t *testing.T) {
	mr, _ := newMiniredis(t)
	cfg := limitsConfig(config.RateLimit{RequestsPerMinute: 60, Burst: 1, MaxConcurrentStreams: 1}, nil, config.RateLimitFailClosed)
	limits := NewLimits(cfg)
	var replicas []*Enforcer
	for range 2 {
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		e, err := NewEnforcer(EnforcerOptions{
			Limiter:    NewCircuit(newTestRedis(t, client, RedisOptions{}), CircuitOptions{}),
			Limits:     func() *Limits { return limits },
			Registerer: prometheus.NewRegistry(),
		})
		if err != nil {
			t.Fatalf("NewEnforcer: %v", err)
		}
		replicas = append(replicas, e)
	}
	var ctxs []context.Context
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxs = append(ctxs, r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	codes := make([]int, 4)
	for i := range codes {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{TenantID: "t", KeyID: "limited", RateLimit: keyLimit(cfg, "limited")}))
		w := httptest.NewRecorder()
		replicas[i%2].Middleware(handler).ServeHTTP(w, r)
		codes[i] = w.Code
	}
	if want := []int{204, 204, 429, 429}; !slices.Equal(codes, want) {
		t.Errorf("statuses = %v, want %v", codes, want)
	}
	release, rej := replicas[0].AcquireStream(ctxs[0])
	if rej != nil {
		t.Fatalf("first stream: %v", rej)
	}
	if _, rej := replicas[1].AcquireStream(ctxs[1]); rej == nil || rej.Status != http.StatusTooManyRequests {
		t.Errorf("second stream on the other replica: %v", rej)
	}
	release()
	release2, rej := replicas[1].AcquireStream(ctxs[1])
	if rej != nil {
		t.Errorf("stream after the release: %v", rej)
	}
	release2()
}
