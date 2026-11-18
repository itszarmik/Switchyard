package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"switchyard/test/fakeupstream"
)

func TestPercentile(t *testing.T) {
	ten := make([]time.Duration, 10)
	for i := range ten {
		ten[i] = time.Duration(i+1) * time.Millisecond
	}
	tests := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"empty", nil, 0.5, 0},
		{"one", []time.Duration{7}, 0.99, 7},
		{"p50 of ten", ten, 0.50, 5 * time.Millisecond},
		{"p90 of ten", ten, 0.90, 9 * time.Millisecond},
		{"p99 of ten", ten, 0.99, 10 * time.Millisecond},
		{"p100 of ten", ten, 1, 10 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.sorted, tc.p); got != tc.want {
				t.Errorf("percentile(%v) = %s, want %s", tc.p, got, tc.want)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		args   []string
		ok     bool
		stream bool
	}{
		{nil, true, false},
		{[]string{"-mode", "stream", "-concurrency", "3", "-duration", "1s"}, true, true},
		{[]string{"-mode", "bogus"}, false, false},
		{[]string{"-concurrency", "0"}, false, false},
		{[]string{"-duration", "0s"}, false, false},
		{[]string{"extra"}, false, false},
		{[]string{"-cache", "miss"}, true, false},
		{[]string{"-cache", "always"}, false, false},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			o, ok := parseFlags(tc.args, io.Discard)
			if ok != tc.ok || (ok && o.stream != tc.stream) {
				t.Errorf("parseFlags = %+v, %v; want ok %v, stream %v", o, ok, tc.ok, tc.stream)
			}
		})
	}
	o, _ := parseFlags(nil, io.Discard)
	if o.concurrency != 100 || o.maxP99Overhead != 5*time.Millisecond {
		t.Errorf("defaults %+v, want concurrency 100 and max-p99-overhead 5ms", o)
	}
}

func TestUniqueBody(t *testing.T) {
	a, b := uniqueBody(requestBody(false), 1), uniqueBody(requestBody(true), 2)
	if !bytes.Contains(a, []byte(`short. 1"`)) || !bytes.Contains(b, []byte(`short. 2"`)) || !bytes.Contains(b, []byte(`"stream":true`)) {
		t.Errorf("uniqueBody: %s, %s", a, b)
	}
}

// TestMeasureAgainstTheFake runs a short measurement in both modes and
// checks the report.
func TestMeasureAgainstTheFake(t *testing.T) {
	upstream := fakeupstream.New(fakeupstream.Options{DisableRecording: true})
	defer upstream.Close()
	for _, stream := range []bool{false, true} {
		o := options{concurrency: 4, duration: 200 * time.Millisecond, warmup: 50 * time.Millisecond,
			stream: stream, maxP99Overhead: time.Hour}
		s, err := measure(context.Background(), o, upstream.BaseURL()+"/chat/completions", "", requestBody(stream))
		if err != nil {
			t.Fatal(err)
		}
		if s.errors != 0 || len(s.latencies) == 0 {
			t.Fatalf("stream %v: %d errors (first: %v), %d latencies", stream, s.errors, s.firstErr, len(s.latencies))
		}
		var out bytes.Buffer
		if err := report(&out, o, s, s); err != nil || !strings.Contains(out.String(), "PASS") {
			t.Errorf("report = %v:\n%s", err, out.String())
		}
	}

	t.Run("failures are reported", func(t *testing.T) {
		o := options{concurrency: 2, duration: 100 * time.Millisecond, maxP99Overhead: time.Millisecond}
		s, err := measure(context.Background(), o, upstream.BaseURL()+"/no-such-path", "", requestBody(false))
		if err != nil {
			t.Fatal(err)
		}
		if s.errors == 0 || s.firstErr == nil || !strings.Contains(s.firstErr.Error(), "status 404") {
			t.Fatalf("errors %d, first %v; want 404 failures", s.errors, s.firstErr)
		}
		slow := stats{latencies: []time.Duration{time.Second}, elapsed: time.Second}
		fast := stats{latencies: []time.Duration{time.Millisecond}, elapsed: time.Second}
		var out bytes.Buffer
		if err := report(&out, o, fast, slow); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("report with a 999ms overhead = %v", err)
		}
		if err := report(&out, o, fast, s); err == nil || !strings.Contains(err.Error(), "failed") {
			t.Errorf("report with failed requests = %v", err)
		}
	})
}
