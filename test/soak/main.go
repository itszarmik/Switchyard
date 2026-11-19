// Command soak runs the gateway binary under constant load for a long time
// and checks that it does not leak and that its errors are the upstream's.
//
//	go run ./test/soak [-duration 1h] [-rate 1000] [-sample-interval 30s]
//	                   [-warmup auto] [-error-rate 0.02] [-stream-ratio 0.2] [-cache-ratio 0.1]
//	                   [-upstream-latency 20ms] [-seed 1] [-redis-url URL] [-bin PATH] [-out FILE]
//
// `make soak` runs it (DURATION ?= 1h). The default -rate, 1,000 req/s, is
// the target load: the rate of the overhead SLO (test/load slo). It starts
// a fake upstream (switchyard/test/load/loadkit) that answers -error-rate
// of its requests, drawn at random, with a 500, a fake OTLP collector
// (loadkit.Collector) that counts the spans it receives, and the gateway
// in front of them, configured as in production apart from the
// failures it must not have: retries and fallback off (one deployment,
// retries_per_deployment 0, a breaker that cannot open at that rate), the
// exact cache on and a tenant budget too large to run out; a per-key rate
// limit (requests_per_minute and burst far above the offered load) and a
// max_concurrent_streams above any number of streams the client can have
// open, so that every request takes the limiter's path and every stream
// a lease, and neither ever refuses one; and tracing on, with sample_ratio
// 1, exporting every span to the collector. An open-loop generator then
// sends -rate requests a second: -cache-ratio of them repeat one of a few
// fixed non-streaming requests, so they are mostly cache hits; of the
// rest, -stream-ratio stream and the others do not, each with unique
// content.
//
// State is in memory by default. With -redis-url, or the environment
// variable SWITCHYARD_SOAK_REDIS_URL, storage.mode is redis on that
// instance, which serves as both the state and the cache Redis, so the
// Redis limiter, its stream leases, its circuit and the cache client are
// soaked too. The redis-state service of deploy/compose.yaml, in the
// test-integration compose project (`make test-integration` uses the same
// project and port, so do not run both at once), serves:
//
//	docker compose -p switchyard-test -f deploy/compose.yaml up -d --wait redis-state
//	make soak DURATION=10m SOAK_FLAGS="-redis-url redis://127.0.0.1:6380/0"
//	docker compose -p switchyard-test -f deploy/compose.yaml down -v
//
// Every -sample-interval it reads go_goroutines,
// process_resident_memory_bytes, go_memstats_heap_inuse_bytes and
// go_memstats_next_gc_bytes from the admin /metrics. (process_resident_memory_bytes is missing from a macOS
// binary built without cgo; the soak then reads the gateway's RSS with ps.)
//
// It passes when (besides flatness and errors, below) the counters read
// from /metrics once the load has ended show no span dropped
// (switchyard_tracing_spans_dropped_total 0) and some exported, no
// request or stream refused by the rate limits
// (switchyard_ratelimit_decisions_total{result="limited"} 0) and some
// allowed, and no log line dropped (switchyard_log_lines_dropped_total 0).
//
// It passes when:
//
//   - Flatness. The samples of the warm-up are ignored, and the rest are
//     split in two halves by time, the middle and the last. The warm-up is
//     the first third of the run, or longer if needed for the gateway's
//     bounded in-memory structures to fill: the memory usage ledger keeps
//     the last 100,000 events and the exact cache 5,000 entries here, and
//     until they are full the heap grows by design. So by default
//     (-warmup 0) it is max(duration/3, 1.1 × 100,000 / rate): at
//     1,000 req/s, 110 s, so a 1-hour soak compares the middle third with
//     the last, and a 10-minute one the last 400 s after a 200 s warm-up. Each statistic of the last part may exceed that of
//     the middle by at most this:
//     goroutines, median: 10% plus 148. The load generator keeps a fixed
//     pool of 64 connections, opened during the warm-up, so the gateway's
//     per-connection goroutines do not follow the client's bursts (an
//     unbounded pool made the count swing between 75 and 300 in a trial
//     run); 64 holds the 1,000 req/s at 20 ms with room to spare. What
//     still moves is the gateway's upstream connections: with at most 64
//     requests in flight, each holding at most one upstream connection of
//     two goroutines, that is 128, plus 20 for everything else (timers,
//     the ledger and cache writers, the stream lease heartbeats). That
//     bounded churn is not a leak; a leak grows without bound, and one
//     goroutine per thousand requests adds 1,200 in each 20-minute third
//     at 1,000 req/s. Smaller leaks than about one per 10,000 requests
//     can pass.
//     Heap, median of the GC goal (go_memstats_next_gc_bytes, the live
//     heap after the last collection times 1+GOGC/100): 15%. The in-use
//     heap is a sawtooth between collections, swinging by up to the GOGC
//     factor whatever the trend (116 to 189 MiB within a minute in a
//     trial run), so it is reported but not judged; the GC goal follows
//     only what survived a collection.
//     RSS, peak: 15%. The runtime's scavenger returns free pages to the OS
//     at its own pace, so a sample can catch RSS far below the working set
//     (100 to 209 MiB in a trial run); the peak of each part is the
//     working set's high-water mark, which a leak raises and the scavenger
//     does not lower.
//     A leak of a few hundred bytes per request exceeds 15% within the
//     hour.
//   - Errors. With retries off, every injected 500 reaches the client as
//     an error, and nothing else may: the client's error count must equal
//     the fake's count of injected errors exactly. The report also shows
//     the observed error rate of the requests that reached the upstream
//     (those that were not cache hits) with its 99% Wilson score interval,
//     for information.
//
// The Markdown report goes to -out, and a summary to stdout; the exit code
// is 0 on a pass, 1 on a failure and 2 on bad flags.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	"switchyard/test/load/loadkit"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// model is the catalog model the soak requests.
const model = "soak-model"

// cacheBodies is how many distinct cacheable requests there are.
const cacheBodies = 8

// clientConns is the most connections the load generator opens to the
// gateway: at the default 1,000 req/s and 20 ms, about 25 requests are in
// flight.
const clientConns = 64

// Limits in the soak's config, far above what the client can reach, so
// the limiter and the stream leases are exercised but never refuse.
const (
	soakRPM     = 1_000_000 // the most validation allows: about 16,700 req/s
	soakBurst   = 100_000
	soakStreams = 10 * clientConns
)

// redisURLEnv names the environment variable that sets -redis-url.
const redisURLEnv = "SWITCHYARD_SOAK_REDIS_URL"

// ledgerEvents is how many events the gateway's memory usage ledger keeps
// (usage.DefaultMemoryMaxEvents); until it is full the heap grows.
const ledgerEvents = 100_000

// Flatness limits; see the package doc.
const (
	goroutineGrowth = 0.10
	// goroutineSlack is 20 plus two per client connection: with at most
	// clientConns requests in flight, each can hold one upstream
	// connection, whose transport runs two goroutines, so the count can
	// move by that much between parts without anything leaking.
	goroutineSlack = 20 + 2*clientConns
	memoryGrowth   = 0.15
	// confidenceZ is the two-sided 99% normal quantile.
	confidenceZ = 2.5758293035489
)

// options are the command-line flags.
type options struct {
	duration        time.Duration
	rate            float64
	sampleInterval  time.Duration
	warmup          time.Duration
	errorRate       float64
	streamRatio     float64
	cacheRatio      float64
	upstreamLatency time.Duration
	seed            uint64
	redisURL        string
	bin             string
	out             string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	o, ok := parseFlags(args, stderr)
	if !ok {
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := soak(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFail
	}
	return exitOK
}

func parseFlags(args []string, stderr io.Writer) (options, bool) {
	var o options
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.DurationVar(&o.duration, "duration", time.Hour, "how long to run the load")
	fs.Float64Var(&o.rate, "rate", 1000, "requests per second, open loop (default: the target load, the SLO's rate)")
	fs.DurationVar(&o.sampleInterval, "sample-interval", 30*time.Second, "how often to sample the gateway's /metrics")
	fs.DurationVar(&o.warmup, "warmup", 0, "samples before this are ignored; 0 means max(duration/3, the memory ledger's fill time)")
	fs.Float64Var(&o.errorRate, "error-rate", 0.02, "fraction of upstream requests answered with an injected 500")
	fs.Float64Var(&o.streamRatio, "stream-ratio", 0.2, "fraction of the uncached requests that stream")
	fs.Float64Var(&o.cacheRatio, "cache-ratio", 0.1, "fraction of requests that repeat a cacheable request")
	fs.DurationVar(&o.upstreamLatency, "upstream-latency", 20*time.Millisecond, "the fake upstream's time to the first byte")
	fs.Uint64Var(&o.seed, "seed", 1, "seed of the injected error draw")
	fs.StringVar(&o.redisURL, "redis-url", os.Getenv(redisURLEnv), "with storage.mode redis on this Redis (default $"+redisURLEnv+"; empty: memory)")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	fs.StringVar(&o.out, "out", "soak-report.md", "where to write the Markdown report")
	if err := fs.Parse(args); err != nil {
		return o, false
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
	case o.rate <= 0 || o.rate > 20000:
		fmt.Fprintln(stderr, "error: -rate must be in (0, 20000]")
	case o.sampleInterval <= 0:
		fmt.Fprintln(stderr, "error: -sample-interval must be positive")
	case o.warmup < 0:
		fmt.Fprintln(stderr, "error: -warmup must not be negative")
	case o.errorRate <= 0 || o.errorRate >= 1:
		fmt.Fprintln(stderr, "error: -error-rate must be in (0, 1)")
	case o.streamRatio < 0 || o.streamRatio > 1 || o.cacheRatio < 0 || o.cacheRatio >= 1:
		fmt.Fprintln(stderr, "error: -stream-ratio must be in [0, 1] and -cache-ratio in [0, 1)")
	case o.upstreamLatency < 0:
		fmt.Fprintln(stderr, "error: -upstream-latency must not be negative")
	case o.out == "":
		fmt.Fprintln(stderr, "error: -out must name a file")
	default:
		if o.warmup == 0 {
			fill := time.Duration(1.1 * ledgerEvents / o.rate * float64(time.Second))
			o.warmup = max(o.duration/3, fill).Round(time.Second)
		}
		if o.duration-o.warmup < 4*o.sampleInterval {
			fmt.Fprintf(stderr, "error: the %s after the %s warm-up must be at least 4 sample intervals, for two samples in each half; lengthen -duration or raise -rate\n",
				o.duration-o.warmup, o.warmup)
			return o, false
		}
		return o, true
	}
	return o, false
}

// sample is one reading of the gateway's /metrics.
type sample struct {
	at         time.Duration // since the load started
	goroutines float64
	rss        float64 // bytes; 0 if unavailable
	heapInuse  float64 // bytes
	gcGoal     float64 // go_memstats_next_gc_bytes: the live heap at the last GC times 1+GOGC/100
	// logDropped is switchyard_log_lines_dropped_total, or -1 when the
	// binary does not export it.
	logDropped float64
	// The tracing and rate-limit counters.
	spansExported, spansDropped                   float64
	rlAllowed, rlLimited, rlErrors, streamsLeased float64
	requests                                      int64 // requests completed so far
}

// counts are the client's tallies.
type counts struct {
	requests  atomic.Int64 // completed, successfully or not
	hits      atomic.Int64 // cache hits (X-Switchyard-Cache: exact)
	errors    atomic.Int64
	streams   atomic.Int64
	mu        sync.Mutex
	statuses  map[string]int64 // error kind -> count
	firstErrs []string
}

func (c *counts) fail(kind, detail string) {
	c.errors.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses[kind]++
	if len(c.firstErrs) < 5 && !slices.Contains(c.firstErrs, detail) {
		c.firstErrs = append(c.firstErrs, detail)
	}
}

// soak runs the whole soak and writes the report.
func soak(ctx context.Context, o options, stdout, stderr io.Writer) error {
	host := loadkit.DescribeHost(ctx)
	loadBefore := loadkit.LoadAverage(ctx)
	up := loadkit.NewUpstream(loadkit.UpstreamOptions{Latency: o.upstreamLatency, ChunkDelay: time.Millisecond, ErrorRate: o.errorRate, Seed: o.seed})
	defer up.Close()
	collector := loadkit.NewCollector()
	defer collector.Close()

	dir, err := os.MkdirTemp("", "switchyard-soak-")
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
	if err := os.WriteFile(cfgPath, []byte(soakConfig(up.BaseURL(), hash, o.redisURL != "")), 0o600); err != nil {
		return fmt.Errorf("writing the config: %w", err)
	}
	gw, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{
		DiscardLogs: true, Env: []string{
			"SOAK_PROVIDER_KEY=sk-soak-provider-key-0123456789",
			"SOAK_COLLECTOR=" + collector.Endpoint(),
			"SOAK_REDIS_URL=" + o.redisURL,
		},
	})
	if err != nil {
		return err
	}
	stopped := false
	stopGateway := func() error {
		if stopped {
			return nil
		}
		stopped = true
		return gw.Stop(30 * time.Second)
	}
	defer func() { _ = stopGateway() }()

	// A fixed pool of connections, opened during the warm-up and kept, so
	// the gateway's connection goroutines do not come and go with the
	// client's bursts; requests beyond it wait in the client.
	transport := &http.Transport{
		MaxConnsPerHost: clientConns, MaxIdleConns: clientConns, MaxIdleConnsPerHost: clientConns,
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	c := &counts{statuses: make(map[string]int64)}

	fmt.Fprintf(stderr, "soaking %s at %.0f req/s, %.1f%% injected upstream errors, %s state; report to %s\n",
		o.duration, o.rate, o.errorRate*100, o.storage(), o.out)
	start := time.Now()
	end := start.Add(o.duration)
	samples := make(chan []sample, 1)
	loadDone := make(chan struct{})
	go func() {
		samples <- sampleLoop(ctx, client, gw.AdminURL, start, end, o.sampleInterval, c, loadDone, stderr)
	}()

	completions := gw.PublicURL + "/v1/chat/completions"
	fired := loadkit.OpenLoop(ctx, o.rate, start, end, func(i int, _ time.Time) {
		body, stream := soakBody(o, i)
		if stream {
			c.streams.Add(1)
		}
		sendOne(ctx, client, completions, key, body, stream, c)
	})
	close(loadDone)
	got := <-samples
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted: %w", err)
	}
	// The counters once the load has ended, before the gateway stops.
	final, finalErr := readMetrics(ctx, client, gw.AdminURL)
	stopErr := stopGateway()

	r := result{
		o: o, host: host, loadBefore: loadBefore, loadAfter: loadkit.LoadAverage(ctx), fired: fired,
		requests: c.requests.Load(), hits: c.hits.Load(), errors: c.errors.Load(), streams: c.streams.Load(),
		statuses: c.statuses, firstErrs: c.firstErrs,
		upstreamRequests: up.Requests(), injected: up.Injected(), samples: got, stopErr: stopErr,
		final: final, finalErr: finalErr, collectorSpans: collector.Spans(), collectorRejected: collector.Rejected(),
	}
	report, failures := r.render()
	if err := os.WriteFile(o.out, []byte(report), 0o600); err != nil {
		failures = append(failures, fmt.Errorf("writing the report: %w", err))
	}
	if len(failures) > 0 {
		fmt.Fprintf(stdout, "soak FAIL (report: %s)\n", o.out)
		return errors.Join(failures...)
	}
	fmt.Fprintf(stdout, "soak PASS (report: %s)\n", o.out)
	return nil
}

// soakConfig is the gateway config: one deployment with prices, no
// retries, a breaker that the injected rate cannot open, the exact cache
// and a budget that cannot run out; a key rate limit and stream cap that
// the load cannot reach; tracing of every request to the collector at
// $SOAK_COLLECTOR; and with redis, storage.mode redis on $SOAK_REDIS_URL.
func soakConfig(baseURL, keyHash string, redis bool) string {
	storage := ""
	if redis {
		storage = "storage: {mode: redis, redis: {state_url: \"${SOAK_REDIS_URL}\", cache_url: \"${SOAK_REDIS_URL}\"}}\n"
	}
	return fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 10s}
logging: {level: info, format: json}
%stelemetry:
  tracing: {enabled: true, endpoint: "${SOAK_COLLECTOR}", sample_ratio: 1}
providers:
  - {id: up, type: openai, base_url: %q, api_key: "${SOAK_PROVIDER_KEY}"}
models:
  - id: %s
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: up, upstream_model: soak-upstream, pricing: {input: "1", output: "2"}}
routing:
  fallback: {max_deployments: 1, retries_per_deployment: 0}
  breaker: {consecutive_failures: 1000, failure_ratio: 1, min_requests: 1000000}
cache: {memory_max_entries: 5000}
policies:
  - id: default
    cache: {exact: {enabled: true, ttl: 1h}}
tenants:
  - id: soak
    keys:
      - id: k1
        hash: %q
        rate_limit: {requests_per_minute: %d, burst: %d, max_concurrent_streams: %d}
    budgets: [{scope: tenant, window: day, limit_usd: "1000000"}]
`, storage, baseURL, model, keyHash, soakRPM, soakBurst, soakStreams)
}

// storage names the soak's state store.
func (o options) storage() string {
	if o.redisURL != "" {
		return "Redis"
	}
	return "in-memory"
}

// soakBody returns request i's body and whether it streams. The mix is
// spread evenly, deterministically: one request in 1/cacheRatio is
// cacheable, and of the rest one in 1/streamRatio streams.
func soakBody(o options, i int) ([]byte, bool) {
	every := func(ratio float64, n int) bool {
		return ratio > 0 && int(float64(n+1)*ratio) > int(float64(n)*ratio)
	}
	// cached is how many cacheable requests came before i; the others are
	// numbered on their own, so the two mixes do not line up.
	cached := int(float64(i) * o.cacheRatio)
	if every(o.cacheRatio, i) {
		return fmt.Appendf(nil, `{"model":%q,"temperature":0,"messages":[{"role":"user","content":"cached question %d"}]}`, model, cached%cacheBodies), false
	}
	if every(o.streamRatio, i-cached) {
		return fmt.Appendf(nil, `{"model":%q,"stream":true,"messages":[{"role":"user","content":"stream %d"}]}`, model, i), true
	}
	return fmt.Appendf(nil, `{"model":%q,"messages":[{"role":"user","content":"question %d"}]}`, model, i), false
}

// sendOne makes one request and tallies its outcome.
func sendOne(ctx context.Context, client *http.Client, url, key string, body []byte, stream bool, c *counts) {
	defer c.requests.Add(1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		c.fail("client", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		c.fail("transport", err.Error())
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	switch {
	case err != nil:
		c.fail("read", err.Error())
	case resp.StatusCode != http.StatusOK:
		c.fail(strconv.Itoa(resp.StatusCode), fmt.Sprintf("status %d: %.200s", resp.StatusCode, bytes.TrimSpace(b)))
	case stream && !bytes.HasSuffix(b, []byte("data: [DONE]\n\n")):
		c.fail("truncated stream", "a stream did not end with data: [DONE]")
	case resp.Header.Get("X-Switchyard-Cache") == "exact":
		c.hits.Add(1)
	}
}

// sampleLoop samples /metrics when the load starts and every interval
// until end.
func sampleLoop(ctx context.Context, client *http.Client, adminURL string, start, end time.Time, interval time.Duration,
	c *counts, loadDone <-chan struct{}, stderr io.Writer) []sample {
	var out []sample
	take := func() {
		s, err := readMetrics(ctx, client, adminURL)
		if err != nil {
			fmt.Fprintf(stderr, "warning: sampling /metrics: %v\n", err)
			return
		}
		s.at = time.Since(start)
		s.requests = c.requests.Load()
		out = append(out, s)
		fmt.Fprintf(stderr, "t=%-8s goroutines=%-5.0f rss=%-9s heap_inuse=%-9s gc_goal=%-9s requests=%d errors=%d hits=%d\n",
			s.at.Round(time.Second), s.goroutines, mib(s.rss), mib(s.heapInuse), mib(s.gcGoal), s.requests, c.errors.Load(), c.hits.Load())
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	take()
	for {
		select {
		case <-ctx.Done():
			return out
		case <-loadDone:
			return out
		case now := <-ticker.C:
			if now.After(end) {
				<-loadDone
				return out
			}
			take()
		}
	}
}

// sampledMetrics are the unlabeled series readMetrics reads.
var sampledMetrics = []string{
	"go_goroutines", "process_resident_memory_bytes", "go_memstats_heap_inuse_bytes", "go_memstats_next_gc_bytes",
	"switchyard_log_lines_dropped_total", "switchyard_tracing_spans_exported_total", "switchyard_tracing_spans_dropped_total",
}

// rateLimitDecisions starts every series of the rate-limit decisions
// counter, which readMetrics reads by kind and result.
const rateLimitDecisions = "switchyard_ratelimit_decisions_total{"

// readMetrics reads the gauges and counters from the admin /metrics. When the
// binary does not export process_resident_memory_bytes (macOS without
// cgo), it reads the RSS of the process listening on the admin port with
// ps.
func readMetrics(ctx context.Context, client *http.Client, adminURL string) (sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/metrics", nil)
	if err != nil {
		return sample{}, fmt.Errorf("building the request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return sample{}, fmt.Errorf("GET /metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("GET /metrics: status %d", resp.StatusCode)
	}
	values := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if v, err := strconv.ParseFloat(value, 64); err == nil && (slices.Contains(sampledMetrics, name) || strings.HasPrefix(name, rateLimitDecisions)) {
			values[name] = v
		}
	}
	if err := sc.Err(); err != nil {
		return sample{}, fmt.Errorf("reading /metrics: %w", err)
	}
	s := sample{goroutines: values["go_goroutines"], rss: values["process_resident_memory_bytes"], heapInuse: values["go_memstats_heap_inuse_bytes"],
		gcGoal: values["go_memstats_next_gc_bytes"], logDropped: -1}
	if v, ok := values["switchyard_log_lines_dropped_total"]; ok {
		s.logDropped = v
	}
	s.spansExported, s.spansDropped = values["switchyard_tracing_spans_exported_total"], values["switchyard_tracing_spans_dropped_total"]
	for name, v := range values {
		if !strings.HasPrefix(name, rateLimitDecisions) {
			continue
		}
		switch {
		case strings.Contains(name, `result="limited"`):
			s.rlLimited += v
		case strings.Contains(name, `result="error"`):
			s.rlErrors += v
		case strings.Contains(name, `kind="request"`):
			s.rlAllowed += v
		default:
			s.streamsLeased += v
		}
	}
	if s.goroutines == 0 || s.heapInuse == 0 {
		return sample{}, errors.New("/metrics lacks go_goroutines or go_memstats_heap_inuse_bytes")
	}
	if s.rss == 0 {
		s.rss = rssByPort(ctx, adminURL)
	}
	return s, nil
}

// rssByPort returns the resident set size, in bytes, of the process
// listening on adminURL's port, or 0 if it cannot be found.
func rssByPort(ctx context.Context, adminURL string) float64 {
	u, err := url.Parse(adminURL)
	if err != nil || u.Port() == "" {
		return 0
	}
	pid, err := exec.CommandContext(ctx, "lsof", "-t", "-sTCP:LISTEN", "-iTCP:"+u.Port()).Output() // #nosec G204 -- a port number
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(pid)), "\n")
	if _, err := strconv.Atoi(first); err != nil {
		return 0
	}
	kb, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", first).Output() // #nosec G204 -- a numeric PID
	if err != nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(kb)), 64)
	if err != nil {
		return 0
	}
	return v * 1024
}

// result is everything the report needs.
type result struct {
	o                options
	host             loadkit.Host
	loadBefore       string
	loadAfter        string
	fired            int
	requests, hits   int64
	errors, streams  int64
	statuses         map[string]int64
	firstErrs        []string
	upstreamRequests int64
	injected         int64
	samples          []sample
	stopErr          error
	// final is the /metrics reading once the load has ended, unless
	// finalErr; collectorSpans the spans the collector received in all.
	final             sample
	finalErr          error
	collectorSpans    int64
	collectorRejected int64
}

// phase names the part of the run a sample at t falls in: the warm-up,
// or the middle or last half of what follows it.
func (o options) phase(t time.Duration) string {
	switch {
	case t < o.warmup:
		return "warm-up"
	case t < o.warmup+(o.duration-o.warmup)/2:
		return "middle"
	default:
		return "last"
	}
}

// split returns the samples of the middle and the last parts.
func split(samples []sample, o options) (mid, last []sample) {
	for _, s := range samples {
		switch o.phase(s.at) {
		case "middle":
			mid = append(mid, s)
		case "last":
			last = append(last, s)
		}
	}
	return mid, last
}

// median returns the median of f over samples.
func median(samples []sample, f func(sample) float64) float64 {
	v := make([]float64, len(samples))
	for i, s := range samples {
		v[i] = f(s)
	}
	slices.Sort(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// peak returns the largest f over samples.
func peak(samples []sample, f func(sample) float64) float64 {
	m := 0.0
	for _, s := range samples {
		m = max(m, f(s))
	}
	return m
}

// wilson returns the Wilson score interval for k successes in n trials at
// normal quantile z.
func wilson(k, n int64, z float64) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	p := float64(k) / float64(n)
	nf := float64(n)
	denom := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / denom
	half := z / denom * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return max(0, center-half), min(1, center+half)
}

// check is one pass criterion.
type check struct {
	name, detail string
	ok           bool
}

// render writes the Markdown report and returns it with the failures.
func (r result) render() (string, []error) {
	var b strings.Builder
	var checks []check

	mid, last := split(r.samples, r.o)
	if len(mid) < 2 || len(last) < 2 {
		checks = append(checks, check{name: "samples", detail: fmt.Sprintf("%d in the middle part and %d in the last; need 2 each", len(mid), len(last))})
	}
	flat := func(name, stat string, agg func([]sample, func(sample) float64) float64, f func(sample) float64,
		growth, slack float64, unit func(float64) string) {
		m, l := agg(mid, f), agg(last, f)
		limit := m*(1+growth) + slack
		if m == 0 {
			checks = append(checks, check{name: name, ok: true, detail: "not available on this platform; skipped"})
			return
		}
		checks = append(checks, check{name: name, ok: l <= limit,
			detail: fmt.Sprintf("%s %s in the middle part, %s in the last (limit %s, %+.1f%%)", stat, unit(m), unit(l), unit(limit), (l/m-1)*100)})
	}
	count := func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
	flat("goroutines", "median", median, func(s sample) float64 { return s.goroutines }, goroutineGrowth, goroutineSlack, count)
	flat("RSS", "peak", peak, func(s sample) float64 { return s.rss }, memoryGrowth, 0, mib)
	flat("heap (GC goal)", "median", median, func(s sample) float64 { return s.gcGoal }, memoryGrowth, 0, mib)

	reached := r.requests - r.hits
	lo, hi := wilson(r.errors, reached, confidenceZ)
	observed := 0.0
	if reached > 0 {
		observed = float64(r.errors) / float64(reached)
	}
	checks = append(checks, check{name: "errors", ok: reached > 0 && r.errors == r.injected,
		detail: fmt.Sprintf("the client saw %d errors, the upstream injected %d (must be equal)", r.errors, r.injected)})
	if r.finalErr != nil {
		checks = append(checks, check{name: "counters", detail: "reading /metrics after the load: " + r.finalErr.Error()})
	} else {
		f := r.final
		checks = append(checks,
			check{name: "spans", ok: f.spansDropped == 0 && f.spansExported > 0 && r.collectorRejected == 0,
				detail: fmt.Sprintf("%.0f exported, %.0f dropped by the end of the load; the collector received %d in all, rejected %d exports",
					f.spansExported, f.spansDropped, r.collectorSpans, r.collectorRejected)},
			check{name: "rate limits", ok: f.rlLimited == 0 && f.rlAllowed > 0,
				detail: fmt.Sprintf("%.0f requests allowed, %.0f stream leases taken, %.0f refused, %.0f store errors",
					f.rlAllowed, f.streamsLeased, f.rlLimited, f.rlErrors)},
			check{name: "log lines", ok: f.logDropped == 0,
				detail: fmt.Sprintf("%.0f dropped by the asynchronous writer", f.logDropped)},
		)
	}
	if r.stopErr != nil {
		checks = append(checks, check{name: "shutdown", detail: r.stopErr.Error()})
	}

	fmt.Fprintf(&b, "# Soak report\n\n")
	fmt.Fprintf(&b, "- Hardware: %s\n", r.host)
	fmt.Fprintf(&b, "- Load average (1, 5, 15 min): %s before, %s after\n", r.loadBefore, r.loadAfter)
	fmt.Fprintf(&b, "- Load: %.0f req/s open loop for %s; %.0f%% cacheable repeats, %.0f%% of the rest streaming; upstream latency %s; %.2f%% injected upstream 500s (seed %d); retries and fallback off\n",
		r.o.rate, r.o.duration, r.o.cacheRatio*100, r.o.streamRatio*100, r.o.upstreamLatency, r.o.errorRate*100, r.o.seed)
	fmt.Fprintf(&b, "- Gateway: %s state; key rate limit %d rpm, burst %d, %d concurrent streams (never reached); tracing on, sample_ratio 1, to an in-process OTLP collector\n",
		r.o.storage(), soakRPM, soakBurst, soakStreams)
	fmt.Fprintf(&b, "- Error rate of the uncached requests (information): %d in %d = %.3f%%; 99%% Wilson interval [%.3f%%, %.3f%%]; configured %.3f%%\n",
		r.errors, reached, observed*100, lo*100, hi*100, r.o.errorRate*100)
	mid0 := r.o.warmup + (r.o.duration-r.o.warmup)/2
	fmt.Fprintf(&b, "- Flatness compares the median of the middle part (%s to %s) with the last (%s to %s), after a %s warm-up; %d samples every %s\n",
		r.o.warmup, mid0, mid0, r.o.duration, r.o.warmup, len(r.samples), r.o.sampleInterval)
	fmt.Fprintf(&b, "- Requests: %d sent, %d completed, %d cache hits, %d streams, %d errors\n", r.fired, r.requests, r.hits, r.streams, r.errors)
	if n := len(r.samples); n > 0 && r.samples[n-1].logDropped >= 0 {
		fmt.Fprintf(&b, "- Log lines dropped by the gateway's asynchronous writer (switchyard_log_lines_dropped_total, last sample): %.0f\n", r.samples[n-1].logDropped)
	}
	fmt.Fprintf(&b, "- Upstream: %d requests, %d injected 500s; the client saw %d errors (equal when every error is an injected one)\n", r.upstreamRequests, r.injected, r.errors)
	if len(r.statuses) > 0 {
		kinds := make([]string, 0, len(r.statuses))
		for k, v := range r.statuses {
			kinds = append(kinds, fmt.Sprintf("%s: %d", k, v))
		}
		slices.Sort(kinds)
		fmt.Fprintf(&b, "- Errors by kind: %s\n", strings.Join(kinds, ", "))
		fmt.Fprintf(&b, "- Distinct first errors: `%s`\n", strings.Join(r.firstErrs, "`; `"))
	}
	fmt.Fprintln(&b)

	var failures []error
	fmt.Fprintln(&b, "| check | result | detail |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, c := range checks {
		verdict := "pass"
		if !c.ok {
			verdict = "**FAIL**"
			failures = append(failures, fmt.Errorf("%s: %s", c.name, c.detail))
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.name, verdict, c.detail)
	}
	if len(failures) == 0 {
		fmt.Fprintf(&b, "\n**PASS**\n\n")
	} else {
		fmt.Fprintf(&b, "\n**FAIL**\n\n")
	}

	fmt.Fprintln(&b, "## Samples")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| t | part | goroutines | RSS | heap in use | GC goal | requests |")
	fmt.Fprintln(&b, "|---:|---|---:|---:|---:|---:|---:|")
	for _, s := range r.samples {
		fmt.Fprintf(&b, "| %s | %s | %.0f | %s | %s | %s | %d |\n", s.at.Round(time.Second), r.o.phase(s.at), s.goroutines, mib(s.rss), mib(s.heapInuse), mib(s.gcGoal), s.requests)
	}
	return b.String(), failures
}

// mib formats a byte count in MiB.
func mib(v float64) string {
	if v == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f MiB", v/(1<<20))
}
