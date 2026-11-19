package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switchyard/test/load/loadkit"
)

func TestParseSLOFlags(t *testing.T) {
	tests := []struct {
		args []string
		ok   bool
	}{
		{nil, true},
		{[]string{"-rate", "50", "-duration", "1s", "-stream-ratio", "0.5"}, true},
		{[]string{"-rate", "0"}, false},
		{[]string{"-rate", "200000"}, false},
		{[]string{"-duration", "0s"}, false},
		{[]string{"-warmup", "-1s"}, false},
		{[]string{"-stream-ratio", "1.5"}, false},
		{[]string{"-upstream-latency", "-1ms"}, false},
		{[]string{"extra"}, false},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if _, ok := parseSLOFlags(tc.args, io.Discard); ok != tc.ok {
				t.Errorf("parseSLOFlags ok = %v, want %v", ok, tc.ok)
			}
		})
	}
	o, _ := parseSLOFlags(nil, io.Discard)
	if o.rate != 1000 || o.duration != time.Minute || o.maxP50 != 2*time.Millisecond || o.maxP99 != 10*time.Millisecond ||
		o.upstreamLatency != 20*time.Millisecond || o.streamRatio != 0.2 {
		t.Errorf("defaults %+v, want the P7 SLO bench settings", o)
	}
}

// TestSLOMeasureDirect runs a short open loop against the fake alone and
// checks the per-request subtraction and the report.
func TestSLOMeasureDirect(t *testing.T) {
	o := sloOptions{rate: 200, duration: 300 * time.Millisecond, warmup: 50 * time.Millisecond,
		upstreamLatency: 5 * time.Millisecond, streamRatio: 0.25, maxP50: time.Hour, maxP99: time.Hour}
	direct := func(baseURL string) (string, string, func(), error) {
		return baseURL + "/chat/completions", "", func() {}, nil
	}
	run, err := sloMeasure(context.Background(), o, "direct", direct)
	if err != nil {
		t.Fatal(err)
	}
	s := run.stats()
	if s.errors != 0 || s.missing != 0 || len(s.added) == 0 {
		t.Fatalf("stats: %d errors (first %v), %d missing, %d measured", s.errors, s.firstErr, s.missing, len(s.added))
	}
	if len(s.addedStream) == 0 || len(s.addedPlain) == 0 {
		t.Fatalf("mode mix: %d stream, %d non-stream; want both", len(s.addedStream), len(s.addedPlain))
	}
	for _, d := range s.added {
		if d < 0 {
			t.Fatalf("negative added latency %s: the upstream's time must be inside the client's", d)
		}
	}
	// The client waits at least the upstream's latency.
	if p50 := loadkit.Percentile(s.total, 0.5); p50 < o.upstreamLatency {
		t.Errorf("client p50 %s is below the upstream latency %s", p50, o.upstreamLatency)
	}

	out := filepath.Join(t.TempDir(), "slo.md")
	o.out = out
	var buf bytes.Buffer
	if err := sloReport(&buf, o, loadkit.Host{CPU: "test chip", Cores: 2}, "1 1 1", "2 2 2", run, run); err != nil {
		t.Fatalf("sloReport: %v\n%s", err, buf.String())
	}
	file, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"test chip, 2 cores", "1 1 1 before", "| gateway |", "| stream |", "**PASS**"} {
		if !strings.Contains(buf.String(), want) || !strings.Contains(string(file), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}

	t.Run("gates", func(t *testing.T) {
		tight := o
		tight.out = ""
		tight.maxP50, tight.maxP99 = time.Nanosecond, time.Nanosecond
		var buf bytes.Buffer
		err := sloReport(&buf, tight, loadkit.Host{}, "", "", run, run)
		if err == nil || !strings.Contains(err.Error(), "added p50") || !strings.Contains(err.Error(), "added p99") ||
			!strings.Contains(buf.String(), "**FAIL**") {
			t.Errorf("sloReport with 1ns gates = %v\n%s", err, buf.String())
		}
	})
}
