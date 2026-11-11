// Command load measures the latency Switchyard adds to chat completions.
//
// It starts a zero-latency fake upstream, measures latency directly against
// it, then builds and starts the gateway in front of it and measures again
// through the gateway. It prints p50, p90 and p99 for both and the added
// latency, and exits non-zero if the added p99 exceeds -max-p99-overhead or
// any request fails.
//
//	go run ./test/load [-concurrency 200] [-duration 10s] [-warmup 2s]
//	                   [-mode non-stream|stream] [-max-p99-overhead 5ms]
//	                   [-bin PATH] [-gateway-log-level info]
//
// Each of the -concurrency workers sends one request at a time, in a closed
// loop, for -warmup plus -duration; only the requests that start after the
// warmup are measured. Run it from inside the switchyard module so it can
// build the gateway, or pass a prebuilt binary with -bin. `make
// bench-overhead` runs it.
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
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the benchmark and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	opts, ok := parseFlags(args, stderr)
	if !ok {
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	if err := fs.Parse(args); err != nil {
		return o, false
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
	case *mode != "non-stream" && *mode != "stream":
		fmt.Fprintf(stderr, "error: -mode must be non-stream or stream, not %q\n", *mode)
	case o.concurrency < 1:
		fmt.Fprintln(stderr, "error: -concurrency must be at least 1")
	case o.duration <= 0 || o.warmup < 0:
		fmt.Fprintln(stderr, "error: -duration must be positive and -warmup not negative")
	default:
		o.stream = *mode == "stream"
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
	if err := harness.WriteConfig(cfgPath, harness.Config{
		ProviderBaseURL: upstream.BaseURL(),
		ProviderAPIKey:  "sk-load-test",
		Models:          []harness.Model{{ID: model, UpstreamModel: model}},
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
	for w := range o.concurrency {
		wg.Go(func() {
			r := &results[w]
			for {
				began := time.Now()
				if !began.Before(end) || runCtx.Err() != nil {
					return
				}
				err := send(runCtx, client, url, key, body, o.stream)
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
	fmt.Fprintf(w, "Switchyard overhead: mode %s, concurrency %d, %s measured after a %s warmup, zero-latency fake upstream\n\n",
		mode, o.concurrency, o.duration, o.warmup)
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
