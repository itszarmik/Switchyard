package health

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/internal/providers"
)

// clock is a manual clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// recorder is an Observer that records every event.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}
func (r *recorder) Transition(d string, to State) { r.add(d + "->" + to.String()) }
func (r *recorder) Cooldown(d string)             { r.add("cooldown " + d) }
func (r *recorder) Saturated(p string)            { r.add("saturated " + p) }
func (r *recorder) BudgetExhausted(d string)      { r.add("budget " + d) }

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func testConfig() Config {
	return Config{
		ConsecutiveFailures: 3, FailureRatio: 0.5, MinRequests: 10, Window: 30 * time.Second,
		OpenFor: 10 * time.Second, HalfOpenProbes: 2,
		CooldownDefault: time.Second, CooldownMax: 5 * time.Second,
		BudgetRatio: 0.2, BudgetMin: 2,
	}
}

func newTracker(cfg Config) (*Tracker, *clock, *recorder) {
	c, r := newClock(), &recorder{}
	return New(Options{Config: cfg, Now: c.Now, Observer: r}), c, r
}

// attempt admits and ends one attempt with kind, failing the test if it
// is not admitted.
func attempt(t *testing.T, d *Deployment, kind providers.Kind) {
	t.Helper()
	probe, reason := d.Admit(false)
	if reason != "" {
		t.Fatalf("Admit refused: %s", reason)
	}
	d.Done(probe, Result{Kind: kind})
}

func TestBreakerConsecutiveFailures(t *testing.T) {
	tr, clk, rec := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{})
	attempt(t, d, providers.KindServerError)
	attempt(t, d, providers.KindTimeout)
	attempt(t, d, "") // a success resets the run
	attempt(t, d, providers.KindServerError)
	attempt(t, d, providers.KindTransport)
	if d.State() != StateClosed {
		t.Fatalf("state %s after 2 failures in a row", d.State())
	}
	attempt(t, d, providers.KindAuth)
	if d.State() != StateOpen {
		t.Fatalf("state %s after 3 failures in a row, want open", d.State())
	}
	if r := d.Skip(); r != "circuit open" {
		t.Errorf("Skip = %q", r)
	}
	if _, r := d.Admit(false); r != "circuit open" {
		t.Errorf("Admit = %q", r)
	}

	clk.Advance(10*time.Second - time.Millisecond)
	if d.State() != StateOpen {
		t.Fatal("half-open before open_for")
	}
	clk.Advance(time.Millisecond)
	if d.Skip() != "" || d.State() != StateHalfOpen {
		t.Fatalf("state %s after open_for", d.State())
	}
	// Two probes at a time; the third request is skipped.
	p1, r1 := d.Admit(false)
	p2, r2 := d.Admit(false)
	_, r3 := d.Admit(false)
	if !p1.Probe() || !p2.Probe() || r1 != "" || r2 != "" || r3 != "circuit half-open, probes in flight" {
		t.Fatalf("probes %v %v, reasons %q %q %q", p1.Probe(), p2.Probe(), r1, r2, r3)
	}
	if r := d.Skip(); r != "circuit half-open, probes in flight" {
		t.Errorf("Skip = %q", r)
	}
	d.Done(p1, Result{}) // one success closes it
	if d.State() != StateClosed {
		t.Fatalf("state %s after a successful probe", d.State())
	}
	d.Done(p2, Result{Kind: providers.KindServerError}) // the other probe's late failure is an ordinary one
	if d.State() != StateClosed {
		t.Fatalf("state %s after one failure while closed", d.State())
	}
	want := []string{"m@a->open", "m@a->half_open", "m@a->closed"}
	if got := rec.list(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events %v, want %v", got, want)
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	tr, clk, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{})
	for range 3 {
		attempt(t, d, providers.KindServerError)
	}
	clk.Advance(10 * time.Second)
	probe, _ := d.Admit(false)
	d.Done(probe, Result{Kind: providers.KindTimeout})
	if d.State() != StateOpen {
		t.Fatalf("state %s after a failed probe, want open", d.State())
	}
	clk.Advance(5 * time.Second)
	if d.State() != StateOpen {
		t.Fatal("a re-opened breaker must wait open_for again")
	}
	clk.Advance(5 * time.Second)
	if d.State() != StateHalfOpen {
		t.Fatalf("state %s", d.State())
	}
	// A probe that ends with a neutral outcome frees its slot.
	for range 5 {
		probe, r := d.Admit(false)
		if r != "" {
			t.Fatalf("Admit refused: %s", r)
		}
		d.Done(probe, Result{Kind: providers.KindClientError})
	}
	if d.State() != StateHalfOpen {
		t.Fatalf("state %s after neutral probes", d.State())
	}
}

func TestBreakerFailureRatio(t *testing.T) {
	tr, clk, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{})
	// Alternate failures and successes: never 3 in a row. The ratio needs
	// 10 requests in the window.
	for i := range 9 {
		kind := providers.KindServerError
		if i%2 == 1 {
			kind = ""
		}
		attempt(t, d, kind)
	}
	if d.State() != StateClosed {
		t.Fatalf("opened after 9 requests")
	}
	attempt(t, d, "") // 10 requests, 5 failures: not yet evaluated on success
	if d.State() != StateClosed {
		t.Fatal("a success opened the breaker")
	}
	attempt(t, d, providers.KindServerError) // 11 requests, 6 failures
	if d.State() != StateOpen {
		t.Fatalf("state %s at a 6/11 failure ratio, want open", d.State())
	}

	// Old requests leave the window.
	tr2, clk2, _ := newTracker(testConfig())
	d2 := tr2.Deployment("m@b", "b", LatencyHint{})
	for i := range 10 {
		kind := providers.KindServerError
		if i%2 == 1 {
			kind = ""
		}
		attempt(t, d2, kind)
	}
	clk2.Advance(31 * time.Second)
	attempt(t, d2, providers.KindServerError)
	if d2.State() != StateClosed {
		t.Fatalf("state %s: requests older than the window counted", d2.State())
	}
	_ = clk
}

func TestNonBreakerKinds(t *testing.T) {
	for _, kind := range []providers.Kind{
		providers.KindRateLimited, providers.KindNotFound, providers.KindContextLength,
		providers.KindContentFilter, providers.KindClientError, providers.KindCanceled,
	} {
		t.Run(string(kind), func(t *testing.T) {
			tr, _, _ := newTracker(testConfig())
			d := tr.Deployment("m@a", "a", LatencyHint{})
			for range 20 {
				probe, _ := d.Admit(true) // a retry ignores a cooldown
				d.Done(probe, Result{Kind: kind})
			}
			if d.State() != StateClosed {
				t.Errorf("%s opened the breaker", kind)
			}
		})
	}
}

func TestCooldown(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
		reason     string
	}{
		{"no Retry-After uses the default", 0, time.Second, "rate-limit cooldown 1s"},
		{"Retry-After", 4 * time.Second, 4 * time.Second, "rate-limit cooldown 4s"},
		{"capped", time.Minute, 5 * time.Second, "rate-limit cooldown 5s"},
		{"sub-second", 300 * time.Millisecond, 300 * time.Millisecond, "rate-limit cooldown 300ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, clk, rec := newTracker(testConfig())
			d := tr.Deployment("m@a", "a", LatencyHint{})
			probe, _ := d.Admit(false)
			d.Done(probe, Result{Kind: providers.KindRateLimited, RetryAfter: tt.retryAfter})
			if r := d.Skip(); r != tt.reason {
				t.Errorf("Skip = %q, want %q", r, tt.reason)
			}
			if _, r := d.Admit(false); r != tt.reason {
				t.Errorf("Admit = %q, want %q", r, tt.reason)
			}
			if _, r := d.Admit(true); r != "" {
				t.Errorf("Admit(retry) = %q, want admitted", r)
			}
			clk.Advance(tt.want - time.Millisecond)
			if d.Skip() == "" {
				t.Error("cooldown ended early")
			}
			clk.Advance(time.Millisecond)
			if r := d.Skip(); r != "" {
				t.Errorf("Skip after the cooldown = %q", r)
			}
			if got := rec.list(); len(got) != 1 || got[0] != "cooldown m@a" {
				t.Errorf("events %v", got)
			}
		})
	}
}

func TestProviderConcurrency(t *testing.T) {
	tr, _, rec := newTracker(testConfig())
	p := tr.Provider("gpu", 2)
	if p.IsSaturated() || !p.Acquire() || !p.Acquire() {
		t.Fatal("could not take two slots")
	}
	for range 10 {
		if !p.IsSaturated() {
			t.Fatal("not saturated at the limit")
		}
	}
	if p.Acquire() {
		t.Fatal("a third slot was available")
	}
	if p.InFlight() != 2 || p.Max() != 2 {
		t.Errorf("in flight %d of %d", p.InFlight(), p.Max())
	}
	p.Release()
	if !p.Acquire() {
		t.Fatal("a released slot was not available")
	}
	p.Release()
	p.Release()
	if p.InFlight() != 0 {
		t.Errorf("in flight %d after releasing all", p.InFlight())
	}
	// Only the refused Acquire counts; IsSaturated has no side effects.
	if got := rec.list(); len(got) != 1 || got[0] != "saturated gpu" {
		t.Errorf("events %v, want one saturation", got)
	}

	unlimited := tr.Provider("cloud", 0)
	for range 1000 {
		if !unlimited.Acquire() {
			t.Fatal("an unlimited provider refused a slot")
		}
	}
	if tr.Provider("gpu", 2) != p {
		t.Error("Provider returned a new handle for the same id")
	}
}

func TestProviderConcurrencyRace(t *testing.T) {
	tr, _, _ := newTracker(testConfig())
	p := tr.Provider("gpu", 5)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		current int
		peak    int
	)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if !p.Acquire() {
					continue
				}
				mu.Lock()
				current++
				peak = max(peak, current)
				mu.Unlock()
				mu.Lock()
				current--
				mu.Unlock()
				p.Release()
			}
		}()
	}
	wg.Wait()
	if peak > 5 || p.InFlight() != 0 {
		t.Errorf("peak %d, in flight %d", peak, p.InFlight())
	}
}

func TestRetryBudget(t *testing.T) {
	tr, clk, rec := newTracker(testConfig()) // ratio 0.2, min 2
	d := tr.Deployment("m@a", "a", LatencyHint{})
	// tries makes three retries and returns how many were allowed.
	tries := func() int {
		n := 0
		for range 3 {
			if d.TryRetry() {
				n++
			}
		}
		return n
	}
	// With no requests, the minimum still allows 2 retries.
	if n := tries(); n != 2 {
		t.Fatalf("the minimum allowed %d retries, want 2", n)
	}
	// 20 requests allow 4 retries in all.
	for range 20 {
		d.startRequest()
	}
	if n := tries(); n != 2 {
		t.Fatalf("20 requests allowed %d more retries, want 2 (4 in all)", n)
	}
	// The window slides: once the bucket sliding out has left it too,
	// 11 s on, nothing is left.
	clk.Advance(BudgetWindow + BudgetWindow/windowBuckets)
	if n := tries(); n != 2 {
		t.Fatalf("after the window %d retries were allowed, want 2", n)
	}
	n := 0
	for _, e := range rec.list() {
		if e == "budget m@a" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d budget exhaustion events, want 3", n)
	}
}

func TestLatencyEstimates(t *testing.T) {
	tr, _, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{TTFT: 800 * time.Millisecond, TokensPerSec: 50})
	if d.EstimatedTTFT() != 800*time.Millisecond || d.EstimatedTokensPerSec() != 50 {
		t.Fatalf("estimates without samples %v %v, want the hint", d.EstimatedTTFT(), d.EstimatedTokensPerSec())
	}
	for range 19 {
		d.Done(Admission{}, Result{TTFT: 100 * time.Millisecond, TokensPerSec: 200})
	}
	if d.EstimatedTTFT() != 800*time.Millisecond {
		t.Fatalf("19 samples replaced the hint: %v", d.EstimatedTTFT())
	}
	d.Done(Admission{}, Result{TTFT: 100 * time.Millisecond, TokensPerSec: 200})
	if d.EstimatedTTFT() != 100*time.Millisecond || d.EstimatedTokensPerSec() != 200 {
		t.Fatalf("estimates after 20 samples %v %v", d.EstimatedTTFT(), d.EstimatedTokensPerSec())
	}
	d.Done(Admission{}, Result{TTFT: 200 * time.Millisecond, TokensPerSec: 0}) // unknown speed is not a sample
	if got := d.EstimatedTTFT(); got != 120*time.Millisecond {
		t.Errorf("EWMA = %v, want 120ms", got)
	}
	s := tr.Snapshot().Providers[0].Deployments[0].Latency
	if s.TTFTSamples != 21 || s.TPSSamples != 20 || s.TokensPerSec != 200 {
		t.Errorf("latency snapshot %+v", s)
	}

	// Without a hint, the measurements are used at once.
	d2 := tr.Deployment("m@b", "b", LatencyHint{})
	d2.Done(Admission{}, Result{TTFT: time.Second, TokensPerSec: 10})
	if d2.EstimatedTTFT() != time.Second || d2.EstimatedTokensPerSec() != 10 {
		t.Errorf("estimates %v %v", d2.EstimatedTTFT(), d2.EstimatedTokensPerSec())
	}
}

func TestStateSurvivesNewHandles(t *testing.T) {
	tr, _, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{})
	for range 3 {
		attempt(t, d, providers.KindServerError)
	}
	// A reload asks for the deployment again, with a new hint.
	again := tr.Deployment("m@a", "a", LatencyHint{TTFT: time.Second})
	if again != d || again.State() != StateOpen {
		t.Errorf("a new handle lost the breaker state")
	}
}

func TestSnapshot(t *testing.T) {
	tr, clk, _ := newTracker(testConfig())
	pb := tr.Provider("b", 4)
	tr.Provider("a", 0)
	d1 := tr.Deployment("x@b", "b", LatencyHint{TTFT: 500 * time.Millisecond})
	d2 := tr.Deployment("m@b", "b", LatencyHint{})
	tr.Deployment("m@a", "a", LatencyHint{})
	for range 3 {
		attempt(t, d1, providers.KindServerError)
	}
	probe, _ := d2.Admit(false)
	d2.Done(probe, Result{Kind: providers.KindRateLimited, RetryAfter: 2 * time.Second})
	_ = d2.TryRetry()
	pb.Acquire()

	s := tr.Snapshot()
	if len(s.Providers) != 2 || s.Providers[0].ID != "a" || s.Providers[1].ID != "b" {
		t.Fatalf("providers %+v", s.Providers)
	}
	b := s.Providers[1]
	if b.InFlight != 1 || b.MaxConcurrency != 4 || len(b.Deployments) != 2 ||
		b.Deployments[0].ID != "m@b" || b.Deployments[1].ID != "x@b" {
		t.Fatalf("provider b %+v", b)
	}
	m, x := b.Deployments[0], b.Deployments[1]
	if x.Breaker.State != "open" || x.Breaker.NextTransition == nil ||
		!x.Breaker.NextTransition.Equal(clk.Now().Add(10*time.Second)) || x.Latency.TTFTMs != 500 {
		t.Errorf("x@b %+v", x)
	}
	if m.CooldownUntil == nil || !m.CooldownUntil.Equal(clk.Now().Add(2*time.Second)) ||
		m.Breaker.State != "closed" || m.RetryBudget.Used != 1 || m.RetryBudget.Requests != 1 || m.RetryBudget.Allowed != 2 {
		t.Errorf("m@b %+v", m)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Second)
	if got := tr.Snapshot().Providers[1].Deployments[1].Breaker; got.State != "half_open" || got.NextTransition != nil {
		t.Errorf("breaker after open_for %+v", got)
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{StateClosed: "closed", StateHalfOpen: "half_open", StateOpen: "open"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q", s, s.String())
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	tr, _, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{})
	p := tr.Provider("a", 3)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				d.startRequest()
				if p.Acquire() {
					probe, r := d.Admit(j%7 == 0)
					if r == "" {
						kind := providers.Kind("")
						if (i+j)%3 == 0 {
							kind = providers.KindServerError
						}
						d.Done(probe, Result{Kind: kind})
					}
					p.Release()
				}
				_ = d.TryRetry()
				d.Done(Admission{}, Result{TTFT: time.Millisecond, TokensPerSec: 10})
				_ = tr.Snapshot()
			}
		}()
	}
	wg.Wait()
	if p.InFlight() != 0 {
		t.Errorf("in flight %d", p.InFlight())
	}
}

func TestHotPathDoesNotAllocate(t *testing.T) {
	tr, _, _ := newTracker(testConfig())
	d := tr.Deployment("m@a", "a", LatencyHint{TTFT: time.Second})
	p := tr.Provider("a", 100)
	allocs := testing.AllocsPerRun(1000, func() {
		if d.Skip() != "" || p.IsSaturated() || !p.Acquire() {
			panic("unhealthy")
		}
		probe, _ := d.Admit(false)
		_ = d.TryRetry()
		d.Done(probe, Result{TTFT: time.Millisecond, TokensPerSec: 50})
		p.Release()
	})
	if allocs != 0 {
		t.Errorf("%v allocations per request", allocs)
	}
}

// TestStaleProbe checks that a probe admitted in an earlier half-open
// episode, which ends after the breaker re-opened and went half-open
// again, neither frees a probe slot of the new episode nor moves its
// breaker.
func TestStaleProbe(t *testing.T) {
	cfg := testConfig() // HalfOpenProbes 2, ConsecutiveFailures 3
	tests := []struct {
		name string
		kind providers.Kind
	}{
		{name: "neutral", kind: providers.KindClientError},
		{name: "success", kind: ""},
		{name: "failure", kind: providers.KindServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, clk, _ := newTracker(cfg)
			d := tr.Deployment("m@a", "a", LatencyHint{})
			for range 3 {
				attempt(t, d, providers.KindServerError)
			}
			clk.Advance(cfg.OpenFor)
			pa, _ := d.Admit(false)                             // episode 1
			pb, _ := d.Admit(false)                             // episode 1
			d.Done(pa, Result{Kind: providers.KindServerError}) // re-opens
			clk.Advance(cfg.OpenFor)
			pc, r1 := d.Admit(false) // episode 2
			pd, r2 := d.Admit(false)
			if !pc.Probe() || !pd.Probe() || r1 != "" || r2 != "" {
				t.Fatalf("episode 2 probes %v %v, reasons %q %q", pc.Probe(), pd.Probe(), r1, r2)
			}
			d.Done(pb, Result{Kind: tt.kind}) // the stale probe ends
			if d.State() != StateHalfOpen {
				t.Fatalf("state %s after the stale probe ended, want half_open", d.State())
			}
			if _, r := d.Admit(false); r != "circuit half-open, probes in flight" {
				t.Errorf("a third concurrent probe: reason %q, want it refused", r)
			}
			d.Done(pc, Result{}) // a current probe still closes it
			if d.State() != StateClosed {
				t.Errorf("state %s after a current probe succeeded, want closed", d.State())
			}
		})
	}
}

// TestWindowCoversItsLength checks that the sliding windows cover their
// whole length: retries made just before a bucket boundary still count
// 9.01 s later, within the 10 s budget window.
func TestWindowCoversItsLength(t *testing.T) {
	cfg := testConfig()
	cfg.BudgetMin = 5
	cfg.BudgetRatio = 0.01
	tr, clk, _ := newTracker(cfg) // the clock starts on a whole second
	d := tr.Deployment("m@a", "a", LatencyHint{})
	clk.Advance(990 * time.Millisecond)
	n := 0
	for d.TryRetry() {
		n++
	}
	if n != cfg.BudgetMin {
		t.Fatalf("%d retries allowed, want %d", n, cfg.BudgetMin)
	}
	clk.Advance(9010 * time.Millisecond)
	if d.TryRetry() {
		t.Errorf("a retry was allowed 9.01 s after the budget was spent, within the 10 s window")
	}
}

func TestWindowSum(t *testing.T) {
	start := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		after time.Duration // from the event, at start+500ms
		want  float64
	}{
		{name: "same bucket", after: 0, want: 4},
		{name: "last full bucket", after: 9 * time.Second, want: 4},
		{name: "sliding out, three quarters left", after: 9*time.Second + 750*time.Millisecond, want: 3},
		{name: "sliding out, half left", after: 10 * time.Second, want: 2},
		{name: "sliding out, a quarter left", after: 10*time.Second + 250*time.Millisecond, want: 1},
		{name: "gone", after: 10*time.Second + 500*time.Millisecond, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWindow(10 * time.Second)
			at := start.Add(500 * time.Millisecond)
			w.add(at, 4, 2)
			a, b := w.sum(at.Add(tt.after))
			if a != tt.want || b != tt.want/2 {
				t.Errorf("sum = %v, %v; want %v, %v", a, b, tt.want, tt.want/2)
			}
		})
	}
}

// TestCommitAndFinish checks the two halves of an attempt that has a
// commit point: Commit ends the admission (a probe closes the breaker at
// once), and Finish records the outcome, an upstream failure counting as
// any failure does.
func TestCommitAndFinish(t *testing.T) {
	cfg := testConfig() // ConsecutiveFailures 3, HalfOpenProbes 2
	t.Run("a probe closes the breaker at commit", func(t *testing.T) {
		tr, clk, _ := newTracker(cfg)
		d := tr.Deployment("m@a", "a", LatencyHint{})
		for range 3 {
			attempt(t, d, providers.KindServerError)
		}
		clk.Advance(cfg.OpenFor)
		p1, _ := d.Admit(false)
		p2, _ := d.Admit(false)
		d.Commit(p1, 10*time.Millisecond)
		if d.State() != StateClosed || d.Skip() != "" {
			t.Fatalf("state %s, Skip %q after the probe committed", d.State(), d.Skip())
		}
		d.Finish(Result{Kind: providers.KindServerError}) // its stream fails later
		d.Done(p2, Result{Kind: providers.KindTimeout})   // the other probe, now ordinary
		if d.State() != StateClosed {
			t.Errorf("state %s after 2 failures, want closed", d.State())
		}
		if got := d.EstimatedTTFT(); got != 10*time.Millisecond {
			t.Errorf("TTFT estimate %s, want the committed 10ms", got)
		}
	})
	t.Run("failures after commit open a closed breaker", func(t *testing.T) {
		tests := []struct {
			name  string
			kind  providers.Kind
			opens bool
		}{
			{"transport", providers.KindTransport, true},
			{"timeout", providers.KindTimeout, true},
			{"server error", providers.KindServerError, true},
			{"canceled", providers.KindCanceled, false},
			{"success", "", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tr, _, _ := newTracker(cfg)
				d := tr.Deployment("m@a", "a", LatencyHint{})
				for range 3 {
					a, _ := d.Admit(false)
					d.Commit(a, time.Millisecond)
					d.Finish(Result{Kind: tt.kind})
				}
				if got := d.State() == StateOpen; got != tt.opens {
					t.Errorf("open = %v after 3 committed attempts ending %q, want %v", got, tt.kind, tt.opens)
				}
			})
		}
	})
	t.Run("a success after commit resets the run", func(t *testing.T) {
		tr, _, _ := newTracker(cfg)
		d := tr.Deployment("m@a", "a", LatencyHint{})
		for _, k := range []providers.Kind{providers.KindTransport, providers.KindTransport, "", providers.KindTransport} {
			a, _ := d.Admit(false)
			d.Commit(a, 0)
			d.Finish(Result{Kind: k, TokensPerSec: 50})
		}
		if d.State() != StateClosed {
			t.Errorf("state %s, want closed", d.State())
		}
		if got := d.EstimatedTokensPerSec(); got != 50 {
			t.Errorf("tokens/s estimate %v, want 50", got)
		}
	})
}
