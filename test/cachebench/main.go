// Command cachebench measures the latency of exact-cache hits through the
// Switchyard gateway with storage.mode redis (DESIGN §11.2, PLAN P6: an
// exact hit takes at most 5 ms at p99 with a local Redis).
//
// It starts a fake upstream, builds and starts the gateway with its exact
// cache in the cache Redis and its namespace versions in the state Redis,
// and warms one entry. Then, for each concurrency level of -concurrency,
// it fires -requests identical requests from that many closed-loop
// workers, after -warmup unmeasured ones that fill the gateway's and the
// client's connection pools. It prints p50, p90 and p99 of the
// client-observed latency of the exact hits per level, and exits non-zero
// if any request fails outright, if more than -max-miss-rate of a level's
// requests miss, or if the p99 at -gate-concurrency exceeds -max-p99.
//
// A miss on the warmed entry is the cache failing open on a slow Redis
// call, by design (DESIGN §14.7); a rare one on a loaded machine says
// nothing about how fast hits are, so misses are tolerated up to a small
// rate and reported in their own column.
//
// The gate is at concurrency 1 because the criterion is the hit's own
// latency: at higher concurrency a closed loop on a shared machine
// measures queueing (by Little's law, the latency is the workers over the
// throughput) and CPU contention, which vary with whatever else the
// machine runs. The other levels are reported for information.
//
//	go run ./test/cachebench -cache-url redis://127.0.0.1:6379/0 \
//	    -state-url redis://127.0.0.1:6380/0 [-requests 20000] \
//	    [-warmup 2000] [-concurrency 1,8,50] [-gate-concurrency 1] \
//	    [-max-p99 5ms] [-max-miss-rate 0.001] [-bin PATH]
//
// `make bench-cache` starts both Redis instances and runs it.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// options are the command-line flags.
type options struct {
	cacheURL, stateURL string
	requests           int
	warmup             int
	// levels are the concurrency levels measured, in order; gate is the
	// one whose p99 is checked.
	levels []int
	gate   int
	maxP99 time.Duration
	// maxMissRate is the largest fraction of a level's requests that may
	// miss.
	maxMissRate float64
	bin         string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the benchmark and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("cachebench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.cacheURL, "cache-url", "", "the cache Redis URL (database 0)")
	fs.StringVar(&o.stateURL, "state-url", "", "the state Redis URL")
	fs.IntVar(&o.requests, "requests", 20_000, "measured hit requests")
	fs.IntVar(&o.warmup, "warmup", 2_000, "unmeasured requests before the measurement")
	levels := fs.String("concurrency", "1,8,50", "comma-separated concurrency levels to measure, each a number of closed-loop workers")
	fs.IntVar(&o.gate, "gate-concurrency", 1, "the level of -concurrency whose p99 must not exceed -max-p99")
	fs.DurationVar(&o.maxP99, "max-p99", 5*time.Millisecond, "fail if the p99 latency of a hit at -gate-concurrency exceeds this")
	fs.Float64Var(&o.maxMissRate, "max-miss-rate", 0.001, "fail if more than this fraction of a level's requests miss (fail open) instead of hitting")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var err error
	o.levels, err = parseLevels(*levels)
	switch {
	case o.cacheURL == "" || o.stateURL == "" || o.requests < 1 || o.warmup < 0 || fs.NArg() > 0:
		fmt.Fprintln(stderr, "error: -cache-url and -state-url are required; -requests must be at least 1")
		return 2
	case o.maxMissRate < 0 || o.maxMissRate >= 1:
		fmt.Fprintln(stderr, "error: -max-miss-rate must be in [0, 1)")
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "error: -concurrency: %v\n", err)
		return 2
	case !slices.Contains(o.levels, o.gate):
		fmt.Fprintln(stderr, "error: -gate-concurrency must be one of the -concurrency levels")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bench(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// parseLevels parses a comma-separated list of concurrency levels, each at
// least 1.
func parseLevels(s string) ([]int, error) {
	var out []int
	for f := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a concurrency level of at least 1", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// config renders the gateway's config: one model on the fake, the exact
// cache on for the default policy, a tenant of its own so that runs
// sharing a Redis never share entries. Logging stays at info, since the
// harness finds the listeners in the info-level "listening" line; it
// discards the lines after it. The lookup timeout is raised from the 25ms
// default to 250ms so that the bench measures hit latency: on a loaded
// machine an occasional lookup over 25ms fails open as a miss by design,
// which says nothing about how fast hits are.
func config(o options, baseURL, tenant, hash string) string {
	return fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 10s}
logging: {level: info, format: json}
cache: {lookup_timeout: 250ms}
storage:
  mode: redis
  redis: {cache_url: %q, state_url: %q}
providers:
  - {id: up, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: bench-model
    deployments:
      - {provider: up, upstream_model: bench-up, pricing: {input: "1", output: "2"}}
policies:
  - id: default
    cache: {exact: {enabled: true, ttl: 1h}}
tenants:
  - id: %s
    keys: [{id: k1, hash: %q}]
`, o.cacheURL, o.stateURL, baseURL, tenant, hash)
}

// bench runs the measurement and reports it.
func bench(ctx context.Context, o options, stdout, stderr io.Writer) error {
	up := fakeupstream.New(fakeupstream.Options{DisableRecording: true})
	defer up.Close()
	dir, err := os.MkdirTemp("", "switchyard-cachebench-")
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
	var b [4]byte
	_, _ = rand.Read(b[:])
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfgPath, []byte(config(o, up.BaseURL(), "bench-"+hex.EncodeToString(b[:]), hash)), 0o600); err != nil {
		return fmt.Errorf("writing the config: %w", err)
	}
	gw, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{DiscardLogs: true, Env: []string{"PROVIDER_KEY=sk-cachebench"}})
	if err != nil {
		return err
	}
	defer func() {
		if err := gw.Stop(30 * time.Second); err != nil {
			fmt.Fprintf(stderr, "warning: %v\n%s", err, gw.Stderr())
		}
	}()

	most := slices.Max(o.levels)
	transport := &http.Transport{MaxIdleConns: 2 * most, MaxIdleConnsPerHost: 2 * most, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	url := gw.PublicURL + "/v1/chat/completions"
	body := []byte(`{"model":"bench-model","temperature":0,"messages":[{"role":"user","content":"Cache me."}]}`)

	// Warm the entry: the write is asynchronous, so wait for the first hit.
	fmt.Fprintln(stderr, "warming one entry...")
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := send(ctx, client, url, key, body)
		if err != nil {
			return err
		}
		if status == "exact" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the entry is still a %q after 10s; is the cache Redis up?", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var runs []levelRun
	for _, c := range o.levels {
		fmt.Fprintf(stderr, "concurrency %d: warming the connection pools with %d requests...\n", c, o.warmup)
		if _, failures, _ := fire(ctx, client, url, key, body, o.warmup, c); len(failures) > 0 {
			var err *httpError
			if errors.As(failures[0], &err) {
				return failures[0]
			}
		}
		fmt.Fprintf(stderr, "concurrency %d: firing %d hits...\n", c, o.requests)
		all, failures, elapsed := fire(ctx, client, url, key, body, o.requests, c)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("interrupted: %w", err)
		}
		runs = append(runs, levelRun{concurrency: c, latencies: all, failures: failures, elapsed: elapsed})
	}
	return report(stdout, o, runs)
}

// levelRun is the measurement at one concurrency level.
type levelRun struct {
	concurrency int
	// latencies are the exact hits', sorted.
	latencies []time.Duration
	failures  []error
	elapsed   time.Duration
}

// httpError is a request that failed outright, as opposed to a miss.
type httpError struct{ err error }

func (e *httpError) Error() string { return e.err.Error() }
func (e *httpError) Unwrap() error { return e.err }

// fire sends n requests from workers closed-loop workers and returns the
// sorted latencies of the exact hits, the failures and misses, and the
// time it took.
func fire(ctx context.Context, client *http.Client, url, key string, body []byte, n, workers int) ([]time.Duration, []error, time.Duration) {
	var (
		next     atomic.Int64
		mu       sync.Mutex
		all      = make([]time.Duration, 0, n)
		failures []error
		wg       sync.WaitGroup
	)
	start := time.Now()
	for range workers {
		wg.Go(func() {
			lat := make([]time.Duration, 0, n/workers+1)
			var errs []error
			for next.Add(1) <= int64(n) && ctx.Err() == nil {
				began := time.Now()
				status, err := send(ctx, client, url, key, body)
				d := time.Since(began)
				switch {
				case err != nil:
					errs = append(errs, &httpError{err})
				case status != "exact":
					errs = append(errs, fmt.Errorf("X-Switchyard-Cache %q, want exact", status))
				default:
					lat = append(lat, d)
				}
			}
			mu.Lock()
			all = append(all, lat...)
			failures = append(failures, errs...)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(all)
	return all, failures, time.Since(start)
}

// report prints the measurements and checks them: no request may fail
// outright at any level, at most -max-miss-rate of a level's requests may
// miss, and the p99 at the gated level must not exceed -max-p99.
func report(stdout io.Writer, o options, runs []levelRun) error {
	fmt.Fprintf(stdout, "Exact-cache hits through the gateway, storage.mode redis; p99 gated at concurrency %d\n\n", o.gate)
	fmt.Fprintln(stdout, "| concurrency | hits | misses | errors | req/s | p50 | p90 | p99 | max |")
	fmt.Fprintln(stdout, "|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	var errs []error
	var gateP99 time.Duration
	for _, r := range runs {
		p := func(q float64) time.Duration { return percentile(r.latencies, q) }
		maxD := time.Duration(0)
		if len(r.latencies) > 0 {
			maxD = r.latencies[len(r.latencies)-1]
		}
		label := strconv.Itoa(r.concurrency)
		if r.concurrency == o.gate {
			label += " (gate)"
		}
		var httpErrs, misses []error
		for _, f := range r.failures {
			if _, ok := errors.AsType[*httpError](f); ok {
				httpErrs = append(httpErrs, f)
			} else {
				misses = append(misses, f)
			}
		}
		fmt.Fprintf(stdout, "| %s | %d | %d | %d | %.0f | %s | %s | %s | %s |\n", label, len(r.latencies), len(misses), len(httpErrs),
			float64(len(r.latencies))/r.elapsed.Seconds(), ms(p(0.50)), ms(p(0.90)), ms(p(0.99)), ms(maxD))
		if len(httpErrs) > 0 {
			errs = append(errs, fmt.Errorf("concurrency %d: %d requests failed; the first: %w", r.concurrency, len(httpErrs), httpErrs[0]))
		}
		if total := len(r.latencies) + len(r.failures); float64(len(misses)) > o.maxMissRate*float64(total) {
			errs = append(errs, fmt.Errorf("concurrency %d: %d of %d requests missed, over the %g limit; the first: %w", r.concurrency, len(misses), total, o.maxMissRate, misses[0]))
		}
		if r.concurrency != o.gate {
			continue
		}
		gateP99 = p(0.99)
		if len(r.latencies) == 0 {
			errs = append(errs, fmt.Errorf("concurrency %d: no hit was measured", r.concurrency))
		} else if gateP99 > o.maxP99 {
			errs = append(errs, fmt.Errorf("concurrency %d: p99 %s exceeds %s", r.concurrency, ms(gateP99), ms(o.maxP99)))
		}
	}
	fmt.Fprintln(stdout)
	if len(errs) > 0 {
		fmt.Fprintln(stdout, "FAIL")
		return errors.Join(errs...)
	}
	fmt.Fprintf(stdout, "PASS: p99 %s <= %s at concurrency %d\n", ms(gateP99), ms(o.maxP99), o.gate)
	return nil
}

// send makes one request and returns its X-Switchyard-Cache.
func send(ctx context.Context, client *http.Client, url, key string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending the request: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %.200s", resp.StatusCode, b)
	}
	return resp.Header.Get("X-Switchyard-Cache"), nil
}

// percentile returns the nearest-rank p-th percentile (0 < p <= 1) of the
// sorted durations, or 0 for none.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// ms formats d in milliseconds with microsecond precision.
func ms(d time.Duration) string {
	return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
}
