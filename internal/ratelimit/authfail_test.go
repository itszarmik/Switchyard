package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newAuthFailures returns an AuthFailures under a limit that perMinute
// holds, on clock, tracking at most maxTracked addresses.
func newAuthFailures(t *testing.T, perMinute *atomic.Int64, clock *fakeClock, maxTracked int) *AuthFailures {
	t.Helper()
	a, err := NewAuthFailures(AuthFailuresOptions{
		PerMinute:  func() int { return int(perMinute.Load()) },
		Now:        clock.Now,
		MaxTracked: maxTracked,
		Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewAuthFailures: %v", err)
	}
	return a
}

func perMinute(n int) *atomic.Int64 {
	var v atomic.Int64
	v.Store(int64(n))
	return &v
}

// fail makes a failed attempt from ip and reports whether it was let
// through.
func fail(a *AuthFailures, ip string) bool {
	ok, _ := a.Attempt(ip, true)
	return ok
}

func TestAuthFailures(t *testing.T) {
	clock := newFakeClock()
	limit := perMinute(30) // T = 2 s
	a := newAuthFailures(t, limit, clock, 0)
	const ip = "192.0.2.1"
	refused := func(failed bool, want time.Duration) {
		t.Helper()
		if ok, wait := a.Attempt(ip, failed); ok || wait != want {
			t.Fatalf("Attempt(failed %v): %v, %v; want refused for %v", failed, ok, wait, want)
		}
	}

	// Valid attempts are never charged, however many.
	for i := range 100 {
		if ok, _ := a.Attempt(ip, false); !ok {
			t.Fatalf("valid attempt %d refused", i)
		}
	}
	// 30 failures at once are let through; then every attempt is refused
	// for one interval, a valid one too.
	for i := range 30 {
		if !fail(a, ip) {
			t.Fatalf("refused after %d failures", i)
		}
	}
	refused(true, 2*time.Second)
	refused(false, 2*time.Second)
	if !fail(a, "192.0.2.2") {
		t.Error("another address is refused")
	}
	clock.Advance(1500 * time.Millisecond)
	refused(true, 500*time.Millisecond)
	clock.Advance(500 * time.Millisecond)
	// One more failure locks it out again for one interval; refused
	// attempts are not charged, so they cannot lock it out longer.
	if !fail(a, ip) {
		t.Fatal("still refused after one interval")
	}
	for range 100 {
		refused(true, 2*time.Second)
	}

	// After a minute every failure has aged out.
	clock.Advance(time.Minute)
	for i := range 30 {
		if !fail(a, ip) {
			t.Fatalf("refused after %d new failures", i)
		}
	}
	refused(true, 2*time.Second)

	// A reload that turns the limit off allows everything at once, and
	// records nothing.
	limit.Store(-1)
	if !fail(a, ip) {
		t.Error("refused with the limit off")
	}
	limit.Store(30)
	refused(true, 2*time.Second)
	if got := testutil.ToFloat64(a.limited); got != 105 {
		t.Errorf("limited = %v, want 105", got)
	}
}

// TestAuthFailuresConcurrent: concurrent failed attempts from one address
// are let through only up to the limit, since each is checked and charged
// in one step, and concurrent valid ones are all let through.
func TestAuthFailuresConcurrent(t *testing.T) {
	tests := []struct {
		perMinute, attempts int
		failed              bool
		want                int
	}{
		{5, 500, true, 5},
		{30, 200, true, 30},
		{1, 50, true, 1},
		{5, 500, false, 500},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d a minute, failed %v", tt.perMinute, tt.failed), func(t *testing.T) {
			a := newAuthFailures(t, perMinute(tt.perMinute), newFakeClock(), 0)
			start := make(chan struct{})
			var through atomic.Int64
			var wg sync.WaitGroup
			for range tt.attempts {
				wg.Go(func() {
					<-start
					if ok, _ := a.Attempt("198.51.100.7", tt.failed); ok {
						through.Add(1)
					}
				})
			}
			close(start)
			wg.Wait()
			if got := through.Load(); got != int64(tt.want) {
				t.Errorf("%d of %d concurrent attempts let through, want %d", got, tt.attempts, tt.want)
			}
		})
	}
}

// TestAuthFailuresTableFull checks that the table never holds more than
// MaxTracked addresses, that a full table is swept of aged-out addresses
// at most once a second, and that new addresses go untracked, so never
// refused, while it stays full.
func TestAuthFailuresTableFull(t *testing.T) {
	clock := newFakeClock()
	a := newAuthFailures(t, perMinute(1), clock, 2) // one failure locks out for a minute
	for _, ip := range []string{"a", "b"} {
		fail(a, ip)
	}
	fail(a, "c")
	if !fail(a, "c") {
		t.Error("an address over the table's size was tracked")
	}
	if fail(a, "a") {
		t.Error("a tracked address was dropped")
	}
	// Once a and b age out, a sweep makes room; but only once a second.
	clock.Advance(time.Minute)
	fail(a, "c") // sweeps a and b
	if fail(a, "c") {
		t.Error("c not tracked after the sweep")
	}
	fail(a, "d")
	clock.Advance(time.Minute)
	fail(a, "e") // c and d have aged out: a sweep makes room
	if fail(a, "e") {
		t.Error("e not tracked after the sweep")
	}
	a.mu.Lock()
	n := len(a.tats)
	a.mu.Unlock()
	if n > 2 {
		t.Errorf("%d addresses tracked, want at most 2", n)
	}
}

func TestAuthFailuresSweepRateLimited(t *testing.T) {
	clock := newFakeClock()
	a := newAuthFailures(t, perMinute(60_000), clock, 1) // a failure counts for 1 ms
	tracked := func() []string {
		a.mu.Lock()
		defer a.mu.Unlock()
		var out []string
		for ip := range a.tats {
			out = append(out, ip)
		}
		return out
	}
	fail(a, "a")
	clock.Advance(time.Second)
	fail(a, "b") // the table is full: a sweep drops a
	if got := tracked(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("tracked %v, want [b]", got)
	}
	// b has aged out, but the last sweep was half a second ago: c goes
	// untracked.
	clock.Advance(500 * time.Millisecond)
	fail(a, "c")
	if got := tracked(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("tracked %v, want [b]: swept twice within a second", got)
	}
	clock.Advance(500 * time.Millisecond)
	fail(a, "c")
	if got := tracked(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("tracked %v, want [c]", got)
	}
}

func TestCheck(t *testing.T) {
	clock := newFakeClock()
	a := newAuthFailures(t, perMinute(2), clock, 0)
	check := func(authenticated bool) (*httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.7:5555"
		w := httptest.NewRecorder()
		return w, a.Check(w, r, authenticated)
	}
	steps := []struct {
		authenticated, want bool
	}{
		{true, true}, {true, true}, {true, true}, // valid keys never count
		{false, true}, {false, true}, // two failures
		{true, false}, {false, false}, // then everything is refused
	}
	for i, s := range steps {
		w, ok := check(s.authenticated)
		if ok != s.want {
			t.Fatalf("step %d (authenticated %v): %v, want %v", i, s.authenticated, ok, s.want)
		}
		if ok && w.Body.Len() != 0 {
			t.Errorf("step %d: Check wrote %q to a request it let through", i, w.Body)
		}
		if !ok {
			msg := checkRejection(t, w, http.StatusTooManyRequests, "requests", CodeRateLimited, "30")
			if !strings.Contains(msg, "failed authentication") {
				t.Errorf("message = %q", msg)
			}
			if w.Header().Get("Connection") != "close" {
				t.Error("the refused request's body was not abandoned")
			}
		}
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct{ remote, want string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:ffff::1]:443", "2001:db8:1:2::/64"},
		{"[::1]:80", "::/64"},
		{"192.0.2.9", "192.0.2.9"},
		{"not an address", "not an address"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if got := ClientIP(r); got != tt.want {
			t.Errorf("ClientIP(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestNewAuthFailuresErrors(t *testing.T) {
	if _, err := NewAuthFailures(AuthFailuresOptions{Registerer: prometheus.NewRegistry()}); err == nil {
		t.Error("no PerMinute accepted")
	}
	if _, err := NewAuthFailures(AuthFailuresOptions{PerMinute: func() int { return 1 }}); err == nil {
		t.Error("no Registerer accepted")
	}
}
