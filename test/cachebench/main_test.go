package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunUsage(t *testing.T) {
	tests := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"-cache-url", "redis://x/0"}, 2},
		{[]string{"-cache-url", "redis://x/0", "-state-url", "redis://y/0", "-requests", "0"}, 2},
		{[]string{"-cache-url", "redis://x/0", "-state-url", "redis://y/0", "extra"}, 2},
		{[]string{"-bogus"}, 2},
		{[]string{"-cache-url", "redis://x/0", "-state-url", "redis://y/0", "-concurrency", "1,0"}, 2},
		{[]string{"-cache-url", "redis://x/0", "-state-url", "redis://y/0", "-concurrency", "1,x"}, 2},
		{[]string{"-cache-url", "redis://x/0", "-state-url", "redis://y/0", "-concurrency", "8,50"}, 2}, // no level 1 to gate
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			if got := run(tt.args, io.Discard, io.Discard); got != tt.want {
				t.Errorf("run = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	ten := make([]time.Duration, 10)
	for i := range ten {
		ten[i] = time.Duration(i+1) * time.Millisecond
	}
	tests := []struct {
		p    float64
		want time.Duration
	}{{0.5, 5 * time.Millisecond}, {0.9, 9 * time.Millisecond}, {0.99, 10 * time.Millisecond}}
	for _, tt := range tests {
		if got := percentile(ten, tt.p); got != tt.want {
			t.Errorf("percentile(%v) = %s, want %s", tt.p, got, tt.want)
		}
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of none = %s", got)
	}
}

func TestConfig(t *testing.T) {
	c := config(options{cacheURL: "redis://c:6379/0", stateURL: "redis://s:6380/0"}, "http://up/v1", "bench-x", "sha256:00")
	for _, want := range []string{"mode: redis", `cache_url: "redis://c:6379/0"`, "exact: {enabled: true", "id: bench-x"} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q:\n%s", want, c)
		}
	}
}

// TestReport: the p99 is checked at the gated level only, and failures at
// any level fail the run.
func TestReport(t *testing.T) {
	lat := func(p99 time.Duration) []time.Duration {
		l := make([]time.Duration, 100)
		for i := range l {
			l[i] = time.Millisecond
		}
		l[98], l[99] = p99, p99
		return l
	}
	o := options{levels: []int{1, 8, 50}, gate: 8, maxP99: 5 * time.Millisecond, maxMissRate: 0.01}
	miss := errors.New("X-Switchyard-Cache \"miss\"")
	tests := []struct {
		name    string
		runs    []levelRun
		wantErr string
	}{
		{"pass, though concurrency 50 is slow", []levelRun{
			{concurrency: 1, latencies: lat(time.Millisecond), elapsed: time.Second},
			{concurrency: 8, latencies: lat(4 * time.Millisecond), elapsed: time.Second},
			{concurrency: 50, latencies: lat(9 * time.Millisecond), elapsed: time.Second},
		}, ""},
		{"the gate fails", []levelRun{
			{concurrency: 8, latencies: lat(6 * time.Millisecond), elapsed: time.Second},
		}, "concurrency 8: p99 6.000ms exceeds 5.000ms"},
		{"a miss within -max-miss-rate passes", []levelRun{
			{concurrency: 1, latencies: lat(time.Millisecond), failures: []error{miss}, elapsed: time.Second},
			{concurrency: 8, latencies: lat(time.Millisecond), elapsed: time.Second},
		}, ""},
		{"misses over -max-miss-rate fail at any level", []levelRun{
			{concurrency: 1, latencies: lat(time.Millisecond), failures: []error{miss, miss}, elapsed: time.Second},
			{concurrency: 8, latencies: lat(time.Millisecond), elapsed: time.Second},
		}, "concurrency 1: 2 of 102 requests missed"},
		{"a failed request fails at any level", []levelRun{
			{concurrency: 1, latencies: lat(time.Millisecond), failures: []error{&httpError{errors.New("connection refused")}}, elapsed: time.Second},
			{concurrency: 8, latencies: lat(time.Millisecond), elapsed: time.Second},
		}, "concurrency 1: 1 requests failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			err := report(&out, o, tt.runs)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("report = %v, want %q\n%s", err, tt.wantErr, out.String())
			}
			if !strings.Contains(out.String(), "| 8 (gate) |") {
				t.Errorf("the gated level is not marked:\n%s", out.String())
			}
		})
	}
}
