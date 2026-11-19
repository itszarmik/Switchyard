// Command load measures the latency Switchyard adds to chat completions.
//
// It has two modes. The default, the overhead benchmark (`make
// bench-overhead`), is a closed loop against a zero-latency fake; the slo
// subcommand (`make bench-slo`) is the P7 SLO bench, an open loop at a
// fixed rate against a fake with realistic latency.
//
// # Overhead benchmark
//
// It starts a zero-latency fake upstream, measures latency directly against
// it, then builds and starts the gateway in front of it and measures again
// through the gateway. It prints p50, p90 and p99 for both and the added
// latency, and exits non-zero if the added p99 exceeds -max-p99-overhead or
// any request fails.
//
//	go run ./test/load [-concurrency 200] [-duration 10s] [-warmup 2s]
//	                   [-mode non-stream|stream] [-max-p99-overhead 5ms]
//	                   [-cache off|miss] [-bin PATH] [-gateway-log-level info]
//
// With -cache miss, the gateway's policy turns the exact cache on (in
// memory) and every request is unique, so each one is a lookup that
// misses and a write: the cost of the lookup on the miss path.
//
// Each of the -concurrency workers sends one request at a time, in a closed
// loop, for -warmup plus -duration; only the requests that start after the
// warmup are measured. Run it from inside the switchyard module so it can
// build the gateway, or pass a prebuilt binary with -bin. `make
// bench-overhead` runs it.
//
// # SLO bench
//
//	go run ./test/load slo [-rate 1000] [-duration 60s] [-warmup 5s]
//	                       [-upstream-latency 20ms] [-chunk-delay 1ms]
//	                       [-stream-ratio 0.2] [-max-p50 2ms] [-max-p99 10ms]
//	                       [-bin PATH] [-gateway-log-level info] [-out FILE]
//
// An open-loop generator sends -rate requests a second, each at its
// scheduled time in a goroutine of its own, whatever the earlier ones are
// doing, first directly to a fake upstream (switchyard/test/load/loadkit)
// and then, with a fresh fake, through the gateway binary in front of it.
// One request in 1/-stream-ratio streams (with stream_options.include_usage),
// the rest do not; caching is off. The gateway runs a production-shaped
// hot path: the key has a memory-mode rate limit and a stream cap, far
// above the load so they never refuse, and tracing is on with sample_ratio
// 1, exporting every span to an in-process OTLP collector
// (loadkit.Collector). The fake answers after
// -upstream-latency, and records how long it spent on each request, keyed
// by the X-Load-Seq header, which the gateway forwards.
//
// The added latency of a request is the client's latency (from sending the
// request to reading its last byte) minus the fake's own time for that same
// request. Subtracting per request, rather than comparing the two
// distributions, cancels the fake's timer jitter, which on a loaded machine
// is as large as the overhead being measured. Through the gateway the
// difference is everything Switchyard adds, the extra loopback hop
// included; directly it is the cost of one hop and the client, which the
// report shows for reference along with the percentile-by-percentile
// difference. The run fails if the gateway's added p50 exceeds -max-p50 or
// its p99 exceeds -max-p99 (the P7 gates: 2 ms and 10 ms), or any request
// fails. The report names the hardware (from sysctl on macOS) and the load
// average before and after, since on a shared machine both matter.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// model is both the catalog ID and the upstream model name.
const model = "load-model"

// options are the command-line flags.
type options struct {
	concurrency    int
	duration       time.Duration
	warmup         time.Duration
	stream         bool
	maxP99Overhead time.Duration
	bin            string
	logLevel       string
	// cacheMiss turns the exact cache on and makes every request unique.
	cacheMiss bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the benchmark and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 && args[0] == "slo" {
		return runSLO(ctx, args[1:], stdout, stderr)
	}
	opts, ok := parseFlags(args, stderr)
	if !ok {
		return exitUsage
	}
	if err := bench(ctx, opts, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFail
	}
	return exitOK
}

func parseFlags(args []string, stderr io.Writer) (options, bool) {
	var o options
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&o.concurrency, "concurrency", 100, "concurrent closed-loop workers")
	fs.DurationVar(&o.duration, "duration", 10*time.Second, "measured duration of each run")
	fs.DurationVar(&o.warmup, "warmup", 2*time.Second, "unmeasured warmup before each run")
	mode := fs.String("mode", "non-stream", "request mode: non-stream or stream")
	fs.DurationVar(&o.maxP99Overhead, "max-p99-overhead", 5*time.Millisecond, "fail if the added p99 latency exceeds this")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	fs.StringVar(&o.logLevel, "gateway-log-level", "info", "the gateway's log level")
	cacheMode := fs.String("cache", "off", "off, or miss: the exact cache on, and every request a miss")
	if err := fs.Parse(args); err != nil {
		return o, false
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
	case *mode != "non-stream" && *mode != "stream":
		fmt.Fprintf(stderr, "error: -mode must be non-stream or stream, not %q\n", *mode)
	case *cacheMode != "off" && *cacheMode != "miss":
		fmt.Fprintf(stderr, "error: -cache must be off or miss, not %q\n", *cacheMode)
	case o.concurrency < 1:
		fmt.Fprintln(stderr, "error: -concurrency must be at least 1")
	case o.duration <= 0 || o.warmup < 0:
		fmt.Fprintln(stderr, "error: -duration must be positive and -warmup not negative")
	default:
		o.stream = *mode == "stream"
		o.cacheMiss = *cacheMode == "miss"
		return o, true
	}
	return o, false
}

// bench runs both measurements and reports them. It returns an error if the
// benchmark could not run or did not pass.
func bench(ctx context.Context, o options, stdout, stderr io.Writer) error {
	upstream := fakeupstream.New(fakeupstream.Options{DisableRecording: true})
	defer upstream.Close()
	body := requestBody(o.stream)

	fmt.Fprintf(stderr, "measuring directly against the fake upstream (%s)...\n", o.window())
	direct, err := measure(ctx, o, upstream.BaseURL()+"/chat/completions", "", body)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "switchyard-load-")
	if err != nil {
		return fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)
	bin := o.bin
	if bin == "" {
		fmt.Fprintln(stderr, "building the gateway...")
		if bin, err = harness.Build(ctx, dir, false); err != nil {
			return err
		}
	}
	key, hash, err := harness.GenerateKey()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	var policies []map[string]any
	if o.cacheMiss {
		policies = []map[string]any{{"id": "default", "cache": map[string]any{"exact": map[string]any{"enabled": true}}}}
	}
	if err := harness.WriteConfig(cfgPath, harness.Config{
		Policies:        policies,
		ProviderBaseURL: upstream.BaseURL(),
		ProviderAPIKey:  "sk-load-test",
		Models:          []harness.Model{{ID: model, UpstreamModel: model, Capabilities: []string{"streaming", "stream_usage"}}},
		KeyHash:         hash,
		LogLevel:        o.logLevel,
	}); err != nil {
		return err
	}
	gw, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{DiscardLogs: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "measuring through the gateway at %s (%s)...\n", gw.PublicURL, o.window())
	via, measureErr := measure(ctx, o, gw.PublicURL+"/v1/chat/completions", key, body)
	if err := gw.Stop(30 * time.Second); err != nil {
		fmt.Fprintf(stderr, "warning: %v\n%s", err, gw.Stderr())
	}
	if measureErr != nil {
		return measureErr
	}

	return report(stdout, o, direct, via)
}

func (o options) window() string {
	return fmt.Sprintf("%d workers, %s after a %s warmup", o.concurrency, o.duration, o.warmup)
}

// requestBody is a small chat completion request.
func requestBody(stream bool) []byte {
	b := `{"model":"` + model + `","messages":[{"role":"user","content":"Say something short."}]`
	if stream {
		b += `,"stream":true`
	}
	return []byte(b + "}")
}

// uniqueBody returns body, a requestBody, with n appended to its message,
// so that no two requests share an exact-cache key.
func uniqueBody(body []byte, n int64) []byte {
	return bytes.Replace(body, []byte(`short."`), fmt.Appendf(nil, `short. %d"`, n), 1)
}

// stats are the results of one measurement.
type stats struct {
	latencies []time.Duration // sorted
	errors    int
	firstErr  error
	elapsed   time.Duration // the measured window
}

func (s stats) percentile(p float64) time.Duration {
	return percentile(s.latencies, p)
}

// percentile returns the nearest-rank p-th percentile (0 < p <= 1) of the
// sorted latencies, or 0 for none.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// measure runs the closed-loop workers against url and collects the
// latencies of the requests started after the warmup.
func measure(ctx context.Context, o options, url, key string, body []byte) (stats, error) {
	transport := &http.Transport{
		MaxIdleConns:        2 * o.concurrency,
		MaxIdleConnsPerHost: 2 * o.concurrency,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	start := time.Now()
	measureFrom := start.Add(o.warmup)
	end := measureFrom.Add(o.duration)
	runCtx, cancel := context.WithDeadline(ctx, end.Add(30*time.Second))
	defer cancel()

	type workerResult struct {
		latencies []time.Duration
		errors    int
		firstErr  error
	}
	results := make([]workerResult, o.concurrency)
	var wg sync.WaitGroup
	var seq atomic.Int64
	for w := range o.concurrency {
		wg.Go(func() {
			r := &results[w]
			for {
				began := time.Now()
				if !began.Before(end) || runCtx.Err() != nil {
					return
				}
				b := body
				if o.cacheMiss {
					b = uniqueBody(body, seq.Add(1))
				}
				err := send(runCtx, client, url, key, b, o.stream)
				latency := time.Since(began)
				if began.Before(measureFrom) {
					continue
				}
				if err != nil {
					r.errors++
					if r.firstErr == nil {
						r.firstErr = err
					}
					continue
				}
				r.latencies = append(r.latencies, latency)
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return stats{}, fmt.Errorf("interrupted: %w", err)
	}

	var s stats
	for _, r := range results {
		s.latencies = append(s.latencies, r.latencies...)
		s.errors += r.errors
		if s.firstErr == nil {
			s.firstErr = r.firstErr
		}
	}
	slices.Sort(s.latencies)
	s.elapsed = o.duration
	return s, nil
}

// send makes one request and reads the whole response. A stream must end
// with data: [DONE].
func send(ctx context.Context, client *http.Client, url, key string, body []byte, stream bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending the request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %.200s", resp.StatusCode, b)
	}
	if stream && !bytes.HasSuffix(b, []byte("data: [DONE]\n\n")) {
		return errors.New("the stream did not end with data: [DONE]")
	}
	return nil
}

// report prints the results as a Markdown table and checks the threshold.
func report(w io.Writer, o options, direct, via stats) error {
	mode := "non-stream"
	if o.stream {
		mode = "stream"
	}
	cacheNote := ""
	if o.cacheMiss {
		cacheNote = ", exact cache on and every request a miss"
	}
	fmt.Fprintf(w, "Switchyard overhead: mode %s, concurrency %d, %s measured after a %s warmup, zero-latency fake upstream%s\n\n",
		mode, o.concurrency, o.duration, o.warmup, cacheNote)
	fmt.Fprintln(w, "| target | requests | errors | req/s | p50 | p90 | p99 |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|")
	row := func(name string, s stats) {
		rps := float64(len(s.latencies)) / s.elapsed.Seconds()
		fmt.Fprintf(w, "| %s | %d | %d | %.0f | %s | %s | %s |\n", name, len(s.latencies), s.errors, rps,
			ms(s.percentile(0.50)), ms(s.percentile(0.90)), ms(s.percentile(0.99)))
	}
	row("direct", direct)
	row("gateway", via)
	added := func(p float64) time.Duration { return via.percentile(p) - direct.percentile(p) }
	fmt.Fprintf(w, "| added | | | | %s | %s | %s |\n\n", ms(added(0.50)), ms(added(0.90)), ms(added(0.99)))

	var failures []error
	for name, s := range map[string]stats{"direct": direct, "gateway": via} {
		if s.errors > 0 {
			failures = append(failures, fmt.Errorf("%d %s requests failed; the first: %w", s.errors, name, s.firstErr))
		}
		if len(s.latencies) == 0 {
			failures = append(failures, fmt.Errorf("no %s request completed in the measured window", name))
		}
	}
	if p99 := added(0.99); p99 > o.maxP99Overhead {
		failures = append(failures, fmt.Errorf("p99 added latency %s exceeds %s", ms(p99), ms(o.maxP99Overhead)))
	}
	if len(failures) > 0 {
		fmt.Fprintln(w, "FAIL")
		return errors.Join(failures...)
	}
	fmt.Fprintf(w, "PASS: p99 added latency %s <= %s\n", ms(added(0.99)), ms(o.maxP99Overhead))
	return nil
}

// ms formats d in milliseconds with microsecond precision.
func ms(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
}
