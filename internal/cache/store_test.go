package cache

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newMiniredis starts a miniredis server and returns it with a client
// built as the gateway builds its clients.
func newMiniredis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestMemoryStoreContract(t *testing.T) {
	testStoreContract(t, func(*testing.T) Store {
		return NewMemoryStore(100, (&fakeClock{now: created}).Now)
	})
}

func TestMemoryNamespacesContract(t *testing.T) {
	testNamespacesContract(t, func(*testing.T) Namespaces { return NewMemoryNamespaces() })
}

func TestRedisStoreContract(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store {
		_, client := newMiniredis(t)
		return NewRedisStore(client)
	})
}

func TestRedisNamespacesContract(t *testing.T) {
	testNamespacesContract(t, func(t *testing.T) Namespaces {
		_, client := newMiniredis(t)
		return NewRedisNamespaces(client)
	})
}

func TestMemoryStoreExpiry(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: created}
	s := NewMemoryStore(10, clock.Now)
	_ = s.Set(ctx, "short", &Entry{Body: []byte("a")}, time.Minute)
	_ = s.Set(ctx, "long", &Entry{Body: []byte("b")}, time.Hour)

	clock.Advance(time.Minute - time.Nanosecond)
	if e, _ := s.Get(ctx, "short"); e == nil {
		t.Fatal("entry expired before its ttl")
	}
	clock.Advance(time.Nanosecond)
	if e, _ := s.Get(ctx, "short"); e != nil {
		t.Error("entry served at its expiry")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d after the expired entry was read, want 1", s.Len())
	}
	if e, _ := s.Get(ctx, "long"); e == nil {
		t.Error("the longer entry expired too")
	}
	// Replacing an entry restarts its ttl.
	_ = s.Set(ctx, "long", &Entry{Body: []byte("c")}, time.Minute)
	clock.Advance(59 * time.Minute)
	if e, _ := s.Get(ctx, "long"); e != nil {
		t.Error("the replaced entry kept its old ttl")
	}
	if s.Evictions() != 0 {
		t.Errorf("Evictions = %d; expiry is not eviction", s.Evictions())
	}
}

func TestMemoryStoreSizeCap(t *testing.T) {
	tests := []struct {
		name          string
		max           int
		sets          []string
		gets          []string // read between the sets and the overflow
		overflow      []string
		wantKept      []string
		wantGone      []string
		wantEvictions int64
	}{
		{"under the cap", 3, []string{"a", "b"}, nil, nil, []string{"a", "b"}, nil, 0},
		{"oldest goes first", 2, []string{"a", "b"}, nil, []string{"c"}, []string{"b", "c"}, []string{"a"}, 1},
		{"a read refreshes", 2, []string{"a", "b"}, []string{"a"}, []string{"c"}, []string{"a", "c"}, []string{"b"}, 1},
		{"a replace refreshes", 2, []string{"a", "b", "a"}, nil, []string{"c"}, []string{"a", "c"}, []string{"b"}, 1},
		{"several evictions", 2, []string{"a", "b"}, nil, []string{"c", "d", "e"}, []string{"d", "e"}, []string{"a", "b", "c"}, 3},
		{"cap below 1 is 1", 0, []string{"a"}, nil, []string{"b"}, []string{"b"}, []string{"a"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := NewMemoryStore(tt.max, (&fakeClock{now: created}).Now)
			for _, k := range tt.sets {
				_ = s.Set(ctx, k, &Entry{Body: []byte(k)}, time.Hour)
			}
			for _, k := range tt.gets {
				_, _ = s.Get(ctx, k)
			}
			for _, k := range tt.overflow {
				_ = s.Set(ctx, k, &Entry{Body: []byte(k)}, time.Hour)
			}
			for _, k := range tt.wantKept {
				if e, _ := s.Get(ctx, k); e == nil || string(e.Body) != k {
					t.Errorf("%q was evicted", k)
				}
			}
			for _, k := range tt.wantGone {
				if e, _ := s.Get(ctx, k); e != nil {
					t.Errorf("%q was kept", k)
				}
			}
			if got := s.Evictions(); got != tt.wantEvictions {
				t.Errorf("Evictions = %d, want %d", got, tt.wantEvictions)
			}
		})
	}
}

// TestMemoryStoreByteCap: beyond its entry cap, the store holds at most
// maxBytes (MemoryMaxBytes), evicting the least recently used entries; an
// entry alone over the cap is kept.
func TestMemoryStoreByteCap(t *testing.T) {
	ctx := context.Background()
	body := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }
	// Each entry is its 1-byte key, its body and the fixed overhead.
	size := func(n int) int64 { return int64(1 + n + entryOverheadBytes) }
	tests := []struct {
		name     string
		maxBytes int64
		sets     []int // body sizes of keys a, b, c, ...
		gets     []string
		wantKept []string
		wantGone []string
	}{
		{"under the cap", 3 * size(100), []int{100, 100, 100}, nil, []string{"a", "b", "c"}, nil},
		{"the oldest goes first", 2 * size(100), []int{100, 100, 100}, nil, []string{"b", "c"}, []string{"a"}},
		{"a read refreshes", 2 * size(100), []int{100, 100, 100}, []string{"a"}, []string{"a", "c"}, []string{"b"}},
		{"a large entry evicts several", 3 * size(100), []int{100, 100, 100, 250}, nil, []string{"c", "d"}, []string{"a", "b"}},
		{"an entry alone over the cap is kept", size(10), []int{10, 500}, nil, []string{"b"}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewMemoryStore(1000, (&fakeClock{now: created}).Now)
			if s.maxBytes != MemoryMaxBytes {
				t.Fatalf("maxBytes %d, want MemoryMaxBytes", s.maxBytes)
			}
			s.maxBytes = tt.maxBytes
			for i, n := range tt.sets {
				k := string(rune('a' + i))
				if i == len(tt.sets)-1 {
					for _, g := range tt.gets {
						_, _ = s.Get(ctx, g)
					}
				}
				if err := s.Set(ctx, k, &Entry{Body: body(n)}, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			for _, k := range tt.wantKept {
				if e, _ := s.Get(ctx, k); e == nil {
					t.Errorf("%q was evicted", k)
				}
			}
			for _, k := range tt.wantGone {
				if e, _ := s.Get(ctx, k); e != nil {
					t.Errorf("%q was kept", k)
				}
			}
			if got, n := s.Evictions(), int64(len(tt.wantGone)); got != n {
				t.Errorf("Evictions = %d, want %d", got, n)
			}
			var want int64
			for _, k := range tt.wantKept {
				want += size(tt.sets[k[0]-'a'])
			}
			if got := s.Bytes(); got != want {
				t.Errorf("Bytes = %d, want %d", got, want)
			}
		})
	}
	t.Run("a replace counts the new size", func(t *testing.T) {
		s := NewMemoryStore(10, (&fakeClock{now: created}).Now)
		_ = s.Set(ctx, "a", &Entry{Body: body(100), Model: "m", DroppedParams: []string{"n"}}, time.Hour)
		_ = s.Set(ctx, "a", &Entry{Body: body(10)}, time.Hour)
		if got := s.Bytes(); got != size(10) {
			t.Errorf("Bytes = %d, want %d", got, size(10))
		}
	})
}

func TestRedisStoreExpiry(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	s := NewRedisStore(client)
	if err := s.Set(ctx, "k", &Entry{Body: []byte("x")}, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("k"); ttl != 1500*time.Millisecond {
		t.Errorf("TTL = %s, want 1.5s", ttl)
	}
	mr.FastForward(1499 * time.Millisecond)
	if e, _ := s.Get(ctx, "k"); e == nil {
		t.Fatal("entry expired before its ttl")
	}
	mr.FastForward(time.Millisecond)
	if e, err := s.Get(ctx, "k"); e != nil || err != nil {
		t.Errorf("Get after expiry = %v, %v; want a miss", e, err)
	}
	// Sub-millisecond ttls are rounded up rather than rejected by Redis.
	if err := s.Set(ctx, "tiny", &Entry{Body: []byte("x")}, time.Microsecond); err != nil {
		t.Errorf("Set(1µs): %v", err)
	}
}

func TestRedisNamespacesKeys(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	n := NewRedisNamespaces(client)
	if _, err := n.Bump(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got, err := mr.Get("sy:ns:v1:acme"); err != nil || got != "1" {
		t.Errorf("sy:ns:v1:acme = %q, %v; want 1", got, err)
	}
	if ttl := mr.TTL("sy:ns:v1:acme"); ttl != 0 {
		t.Errorf("the version key has ttl %s, want none", ttl)
	}
	_ = mr.Set("sy:ns:v1:bad", "not a number")
	if _, err := n.Version(ctx, "bad"); err == nil {
		t.Error("Version accepted a non-integer version")
	}
}

func TestRedisStoreBadEntry(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t)
	s := NewRedisStore(client)
	for _, v := range []string{"", "\x02\xff"} {
		_ = mr.Set("k", v)
		if e, err := s.Get(ctx, "k"); err == nil || e != nil {
			t.Errorf("Get(%q) = %v, %v; want an error", v, e, err)
		}
	}
	// An entry of another encoding version, such as version 1 from before
	// DroppedParams, or a future one, is a plain miss.
	for _, v := range []string{"\x01\x01d\x01m\x00{}", "\x03future-format"} {
		_ = mr.Set("k", v)
		if e, err := s.Get(ctx, "k"); err != nil || e != nil {
			t.Errorf("Get(%q) = %v, %v; want a miss", v, e, err)
		}
	}
}

// TestRedisFailOpen checks that a Redis that is down, or that accepts
// connections and never answers, fails each call with an error once the
// context's deadline passes, rather than hanging the request.
func TestRedisFailOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var conns sync.WaitGroup
	release := make(chan struct{})
	conns.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Go(func() {
				<-release // never answer
				_ = c.Close()
			})
		}
	})
	t.Cleanup(func() {
		close(release)
		_ = ln.Close()
		conns.Wait()
	})

	mr, client := newMiniredis(t)
	mr.Close() // nothing listens there any more

	hung := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), ContextTimeoutEnabled: true, MaxRetries: -1})
	t.Cleanup(func() { _ = hung.Close() })

	for name, c := range map[string]*redis.Client{"down": client, "hung": hung} {
		t.Run(name, func(t *testing.T) {
			calls := map[string]func(ctx context.Context) error{
				"get": func(ctx context.Context) error { _, err := NewRedisStore(c).Get(ctx, "k"); return err },
				"set": func(ctx context.Context) error {
					return NewRedisStore(c).Set(ctx, "k", &Entry{Body: []byte("x")}, time.Hour)
				},
				"version": func(ctx context.Context) error { _, err := NewRedisNamespaces(c).Version(ctx, "a"); return err },
				"bump":    func(ctx context.Context) error { _, err := NewRedisNamespaces(c).Bump(ctx, "a"); return err },
			}
			for call, fn := range calls {
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				start := time.Now()
				err := fn(ctx)
				elapsed := time.Since(start)
				cancel()
				if err == nil {
					t.Errorf("%s succeeded", call)
				}
				// Generous: the point is that the call is bounded by the
				// deadline, not the server.
				if elapsed > 5*time.Second {
					t.Errorf("%s took %s with a 50ms deadline", call, elapsed)
				}
			}
		})
	}
}

func TestEntryEncoding(t *testing.T) {
	tests := []struct {
		name string
		e    Entry
	}{
		{"full", Entry{Body: []byte(`{"id":"x"}`), Deployment: "gpt-4o@openai", Model: "gpt-4o", Created: created}},
		{"zero time", Entry{Body: []byte("b"), Deployment: "d", Model: "m"}},
		{"empty", Entry{}},
		{"before 1970", Entry{Body: []byte("b"), Created: time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC)}},
		{"long names", Entry{Body: bytes.Repeat([]byte{0, 1, 2}, 1000), Deployment: string(bytes.Repeat([]byte("d"), 300)), Model: "m"}},
		{"dropped parameters", Entry{Body: []byte("b"), Deployment: "d", Model: "m", DroppedParams: []string{"logprobs", "n"}, Created: created}},
		{"an empty dropped parameter", Entry{Body: []byte("b"), DroppedParams: []string{""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := MarshalEntry(&tt.e)
			if data[0] != entryVersion {
				t.Errorf("version byte %d", data[0])
			}
			got, err := UnmarshalEntry(data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Body, tt.e.Body) || got.Deployment != tt.e.Deployment || got.Model != tt.e.Model ||
				!got.Created.Equal(tt.e.Created) || !slices.Equal(got.DroppedParams, tt.e.DroppedParams) {
				t.Errorf("round trip = %+v, want %+v", got, tt.e)
			}
		})
	}

	for _, e := range []Entry{tests[0].e, tests[5].e} {
		full := MarshalEntry(&e)
		for i := range len(full) - len(e.Body) {
			if _, err := UnmarshalEntry(full[:i]); !errors.Is(err, errBadEntry) {
				t.Errorf("%+v truncated to %d bytes: err = %v, want errBadEntry", e, i, err)
			}
		}
	}
	for name, data := range map[string]string{
		"overlong length":            "\x02\x80\x80\x80\x80\x80\x80\x80\x80\x80\x01",
		"more parameters than bytes": "\x02\x00\x00\x05\x00",
	} {
		if _, err := UnmarshalEntry([]byte(data)); !errors.Is(err, errBadEntry) {
			t.Errorf("%s: err = %v, want errBadEntry", name, err)
		}
	}
	for _, v := range []byte{1, 3} {
		if _, err := UnmarshalEntry([]byte{v, 0, 0, 0, 0}); !errors.Is(err, ErrEntryVersion) || errors.Is(err, errBadEntry) {
			t.Errorf("version %d: err = %v, want ErrEntryVersion", v, err)
		}
	}
}
