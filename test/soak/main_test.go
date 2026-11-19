package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"switchyard/test/load/loadkit"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		ok         bool
		wantWarmup time.Duration
	}{
		{name: "defaults: a third of an hour", args: nil, ok: true, wantWarmup: 20 * time.Minute},
		{name: "ten minutes at the default rate", args: []string{"-duration", "10m"}, ok: true, wantWarmup: 200 * time.Second},
		{name: "twenty minutes at 200/s waits for the ledger", args: []string{"-duration", "20m", "-rate", "200"}, ok: true, wantWarmup: 550 * time.Second},
		{name: "explicit warmup", args: []string{"-duration", "10m", "-warmup", "1m"}, ok: true, wantWarmup: time.Minute},
		{name: "too short after the warm-up", args: []string{"-duration", "10m", "-rate", "200"}, ok: false},
		{name: "bad rate", args: []string{"-rate", "0"}, ok: false},
		{name: "bad error rate", args: []string{"-error-rate", "0"}, ok: false},
		{name: "bad cache ratio", args: []string{"-cache-ratio", "1"}, ok: false},
		{name: "negative warmup", args: []string{"-warmup", "-1s"}, ok: false},
		{name: "empty out", args: []string{"-out", ""}, ok: false},
		{name: "extra argument", args: []string{"x"}, ok: false},
	}
	t.Setenv(redisURLEnv, "")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, ok := parseFlags(tt.args, io.Discard)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && o.warmup != tt.wantWarmup {
				t.Errorf("warmup = %s, want %s", o.warmup, tt.wantWarmup)
			}
			if ok && (o.rate != 1000 && !slices.Contains(tt.args, "-rate") || o.redisURL != "") {
				t.Errorf("rate %v, redis %q; want the default 1000 and memory", o.rate, o.redisURL)
			}
		})
	}
	// The environment sets -redis-url's default; the flag overrides it.
	t.Setenv(redisURLEnv, "redis://env:6379/0")
	for args, want := range map[string]string{"": "redis://env:6379/0", "redis://flag:6379/1": "redis://flag:6379/1"} {
		var a []string
		if args != "" {
			a = []string{"-redis-url", args}
		}
		if o, ok := parseFlags(a, io.Discard); !ok || o.redisURL != want || o.storage() != "Redis" {
			t.Errorf("parseFlags(%v): redis %q (ok %v), want %q", a, o.redisURL, ok, want)
		}
	}
}

func TestWilson(t *testing.T) {
	tests := []struct {
		k, n   int64
		lo, hi float64 // expected, to 4 decimals
	}{
		// Reference values from the Wilson score formula at z = 2.5758.
		{k: 20, n: 1000, lo: 0.0114, hi: 0.0350},
		{k: 2400, n: 120000, lo: 0.0190, hi: 0.0211},
		{k: 0, n: 100, lo: 0, hi: 0.0622},
	}
	for _, tt := range tests {
		lo, hi := wilson(tt.k, tt.n, confidenceZ)
		if math.Abs(lo-tt.lo) > 5e-4 || math.Abs(hi-tt.hi) > 5e-4 {
			t.Errorf("wilson(%d, %d) = [%.4f, %.4f], want [%.4f, %.4f]", tt.k, tt.n, lo, hi, tt.lo, tt.hi)
		}
	}
	if lo, hi := wilson(0, 0, confidenceZ); lo != 0 || hi != 1 {
		t.Errorf("wilson with no trials = [%v, %v], want [0, 1]", lo, hi)
	}
}

func TestMedianAndPhases(t *testing.T) {
	g := func(v ...float64) []sample {
		out := make([]sample, len(v))
		for i, x := range v {
			out[i].goroutines = x
		}
		return out
	}
	f := func(s sample) float64 { return s.goroutines }
	for _, tt := range []struct {
		in   []sample
		want float64
	}{{nil, 0}, {g(3), 3}, {g(5, 1, 9), 5}, {g(4, 1, 3, 2), 2.5}} {
		if got := median(tt.in, f); got != tt.want {
			t.Errorf("median(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}

	o := options{duration: 90 * time.Second, warmup: 30 * time.Second}
	for _, tt := range []struct {
		at   time.Duration
		want string
	}{{0, "warm-up"}, {29 * time.Second, "warm-up"}, {30 * time.Second, "middle"}, {59 * time.Second, "middle"}, {60 * time.Second, "last"}, {90 * time.Second, "last"}} {
		if got := o.phase(tt.at); got != tt.want {
			t.Errorf("phase(%s) = %s, want %s", tt.at, got, tt.want)
		}
	}
}

func TestSoakBodyMix(t *testing.T) {
	o := options{cacheRatio: 0.1, streamRatio: 0.2}
	var cached, streams, plain int
	seen := map[string]bool{}
	for i := range 1000 {
		body, stream := soakBody(o, i)
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("body %d is not JSON: %s", i, body)
		}
		switch {
		case bytes.Contains(body, []byte("cached question")):
			cached++
			seen[string(body)] = true
			if stream {
				t.Fatal("a cacheable request streams")
			}
		case stream:
			streams++
		default:
			plain++
		}
	}
	if cached != 100 || len(seen) != cacheBodies || streams != 180 || plain != 720 {
		t.Errorf("mix: %d cached (%d distinct), %d streams, %d plain; want 100 (%d), 180, 720", cached, len(seen), streams, plain, cacheBodies)
	}
}

// TestRender checks the verdicts on synthetic runs.
func TestRender(t *testing.T) {
	o := options{duration: 90 * time.Second, warmup: 30 * time.Second, sampleInterval: 10 * time.Second, rate: 100, errorRate: 0.02}
	samples := func(goroutines, memGrowth float64) []sample {
		var out []sample
		for i := range 10 {
			at := time.Duration(i) * 10 * time.Second
			grow := 1.0
			if at >= 60*time.Second {
				grow = 1 + memGrowth
			}
			g := 50.0
			if at >= 60*time.Second {
				g = goroutines
			}
			out = append(out, sample{at: at, goroutines: g, rss: 40e6 * grow, heapInuse: 20e6 * grow, gcGoal: 30e6 * grow})
		}
		return out
	}
	good := sample{spansExported: 5000, rlAllowed: 11000, streamsLeased: 2000}
	ok := func(r result) result {
		r.requests, r.hits, r.injected = 11000, 1000, 200
		if r.errors == 0 {
			r.errors = 200
		}
		if r.samples == nil {
			r.samples = samples(50, 0)
		}
		if r.final == (sample{}) {
			r.final = good
		}
		return r
	}
	with := func(f func(*sample)) sample {
		s := good
		f(&s)
		return s
	}
	tests := []struct {
		name     string
		r        result
		wantFail []string
	}{
		{name: "flat with the injected errors", r: ok(result{samples: samples(60, 0.05)})},
		{name: "goroutine leak", r: ok(result{samples: samples(250, 0)}), wantFail: []string{"goroutines"}},
		{name: "memory growth", r: ok(result{samples: samples(50, 0.2)}), wantFail: []string{"RSS", "heap (GC goal)"}},
		{name: "an error besides the injected ones", r: ok(result{errors: 201}), wantFail: []string{"errors"}},
		{name: "an injected error missing", r: ok(result{errors: 199}), wantFail: []string{"errors"}},
		{name: "too few samples", r: ok(result{samples: samples(50, 0)[:5]}), wantFail: []string{"samples"}},
		{name: "unclean shutdown", r: ok(result{stopErr: os.ErrDeadlineExceeded}), wantFail: []string{"shutdown"}},
		{name: "spans dropped", r: ok(result{final: with(func(s *sample) { s.spansDropped = 3 })}), wantFail: []string{"spans"}},
		{name: "no spans", r: ok(result{final: with(func(s *sample) { s.spansExported = 0 })}), wantFail: []string{"spans"}},
		{name: "a request rate limited", r: ok(result{final: with(func(s *sample) { s.rlLimited = 1 })}), wantFail: []string{"rate limits"}},
		{name: "log lines dropped", r: ok(result{final: with(func(s *sample) { s.logDropped = 1 })}), wantFail: []string{"log lines"}},
		{name: "no final reading", r: ok(result{finalErr: os.ErrDeadlineExceeded}), wantFail: []string{"counters"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.r.o = o
			report, failures := tt.r.render()
			var names []string
			for _, f := range failures {
				name, _, _ := strings.Cut(f.Error(), ":")
				names = append(names, name)
			}
			if strings.Join(names, ",") != strings.Join(tt.wantFail, ",") {
				t.Fatalf("failures %v, want %v\n%s", failures, tt.wantFail, report)
			}
			want := "**PASS**"
			if len(tt.wantFail) > 0 {
				want = "**FAIL**"
			}
			if !strings.Contains(report, want) || !strings.Contains(report, "| 30s | middle |") {
				t.Errorf("report lacks %s or the samples table:\n%s", want, report)
			}
		})
	}
}

// TestSoakShort runs a few seconds of soak through the gateway binary.
func TestSoakShort(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway")
	}
	out := filepath.Join(t.TempDir(), "soak.md")
	o := options{duration: 4 * time.Second, rate: 100, sampleInterval: 500 * time.Millisecond, warmup: time.Second,
		errorRate: 0.2, streamRatio: 0.3, cacheRatio: 0.2, upstreamLatency: time.Millisecond, seed: 3, out: out}
	var stdout, stderr bytes.Buffer
	err := soak(context.Background(), o, &stdout, &stderr)
	report, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("no report: %v (soak: %v)\n%s", rerr, err, stderr.String())
	}
	// A few seconds cannot judge flatness, so only the errors, the P7
	// counters and the plumbing are checked.
	for _, want := range []string{"| errors | pass |", "| spans | pass |", "| rate limits | pass |", "| log lines | pass |",
		"injected 500s", "cache hits", "## Samples", loadkit.DescribeHost(context.Background()).CPU} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(string(report), "| shutdown |") {
		t.Errorf("the gateway did not shut down cleanly:\n%s", report)
	}
	t.Logf("soak: %v\n%s", err, report)
}
