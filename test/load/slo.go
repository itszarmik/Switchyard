package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/load/loadkit"
)

// maxTrustedLag is the generator lag p99 above which the report warns that
// the machine was stalling: the open loop sleeps to each request's due
// time, so a late wake-up means the process was starved of CPU.
const maxTrustedLag = 2 * time.Millisecond

// sloOptions are the flags of the slo subcommand.
type sloOptions struct {
	rate            float64
	duration        time.Duration
	warmup          time.Duration
	upstreamLatency time.Duration
	chunkDelay      time.Duration
	streamRatio     float64
	maxP50          time.Duration
	maxP99          time.Duration
	bin             string
	logLevel        string
	out             string
}

func parseSLOFlags(args []string, stderr io.Writer) (sloOptions, bool) {
	var o sloOptions
	fs := flag.NewFlagSet("load slo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Float64Var(&o.rate, "rate", 1000, "requests per second, open loop")
	fs.DurationVar(&o.duration, "duration", 60*time.Second, "measured duration of each run")
	fs.DurationVar(&o.warmup, "warmup", 5*time.Second, "unmeasured warmup at the same rate before each run")
	fs.DurationVar(&o.upstreamLatency, "upstream-latency", 20*time.Millisecond, "the fake upstream's time to the response headers or first stream event")
	fs.DurationVar(&o.chunkDelay, "chunk-delay", time.Millisecond, "the fake upstream's pause between stream events")
	fs.Float64Var(&o.streamRatio, "stream-ratio", 0.2, "fraction of requests that stream")
	fs.DurationVar(&o.maxP50, "max-p50", 2*time.Millisecond, "fail if the gateway's added p50 exceeds this")
	fs.DurationVar(&o.maxP99, "max-p99", 10*time.Millisecond, "fail if the gateway's added p99 exceeds this")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	fs.StringVar(&o.logLevel, "gateway-log-level", "info", "the gateway's log level")
	fs.StringVar(&o.out, "out", "", "also write the Markdown report to this file")
	if err := fs.Parse(args); err != nil {
		return o, false
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
	case o.rate <= 0 || o.rate > 100000:
		fmt.Fprintln(stderr, "error: -rate must be in (0, 100000]")
	case o.duration <= 0 || o.warmup < 0:
		fmt.Fprintln(stderr, "error: -duration must be positive and -warmup not negative")
	case o.streamRatio < 0 || o.streamRatio > 1:
		fmt.Fprintln(stderr, "error: -stream-ratio must be in [0, 1]")
	case o.upstreamLatency < 0 || o.chunkDelay < 0:
		fmt.Fprintln(stderr, "error: -upstream-latency and -chunk-delay must not be negative")
	default:
		return o, true
	}
	return o, false
}

// runSLO runs the slo subcommand and returns the exit code.
func runSLO(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, ok := parseSLOFlags(args, stderr)
	if !ok {
		return exitUsage
	}
	if err := sloBench(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFail
	}
	return exitOK
}

// sloResult is one request of an SLO run.
type sloResult struct {
	stream   bool
	measured bool          // started after the warmup
	lag      time.Duration // how late the generator sent it
	total    time.Duration // from sending to the end of the response
	err      error
}

// sloRun is the outcome of one SLO run against a target.
type sloRun struct {
	name     string
	results  []sloResult
	upstream *loadkit.Upstream
	elapsed  time.Duration
}

// sloBench measures directly against the fake and through the gateway, at
// the same open-loop rate, and reports the overhead.
func sloBench(ctx context.Context, o sloOptions, stdout, stderr io.Writer) error {
	host := loadkit.DescribeHost(ctx)
	loadBefore := loadkit.LoadAverage(ctx)

	fmt.Fprintf(stderr, "measuring directly against the fake upstream (%s)...\n", o.window())
	direct, err := sloMeasure(ctx, o, "direct", func(baseURL string) (string, string, func(), error) {
		return baseURL + "/chat/completions", "", func() {}, nil
	})
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "switchyard-slo-")
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
	fmt.Fprintf(stderr, "measuring through the gateway (%s)...\n", o.window())
	via, err := sloMeasure(ctx, o, "gateway", func(baseURL string) (string, string, func(), error) {
		return startSLOGateway(ctx, o, dir, bin, baseURL, stderr)
	})
	if err != nil {
		return err
	}
	return sloReport(stdout, o, host, loadBefore, loadkit.LoadAverage(ctx), direct, via)
}

func (o sloOptions) window() string {
	return fmt.Sprintf("%.0f req/s for %s after a %s warmup", o.rate, o.duration, o.warmup)
}

// The production-shaped settings of the SLO gateway: a key rate limit
// and a stream cap far above the load, so every request takes the
// limiter's path and every stream a lease, without one ever being
// refused.
const (
	sloRPM     = 1_000_000 // the most validation allows: about 16,700 req/s
	sloBurst   = 100_000
	sloStreams = 100_000
)

// startSLOGateway starts the gateway in front of the upstream at baseURL,
// with caching off and the sequence header forwarded, and returns its
// completions URL, the client key and a stop function. It runs a
// production-shaped hot path: a memory-mode rate limit on the key (and a
// max_concurrent_streams), never reached, and tracing of every request
// (sample_ratio 1) to an in-process OTLP collector, which stop closes.
func startSLOGateway(ctx context.Context, o sloOptions, dir, bin, baseURL string, stderr io.Writer) (string, string, func(), error) {
	key, hash, err := harness.GenerateKey()
	if err != nil {
		return "", "", nil, err
	}
	collector := loadkit.NewCollector()
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := harness.WriteConfig(cfgPath, harness.Config{
		ProviderBaseURL: baseURL,
		ProviderAPIKey:  "sk-load-test",
		ForwardHeaders:  []string{loadkit.SeqHeader},
		Models:          []harness.Model{{ID: model, UpstreamModel: model, Capabilities: []string{"streaming", "stream_usage"}}},
		KeyHash:         hash,
		LogLevel:        o.logLevel,
		KeyRateLimit:    map[string]any{"requests_per_minute": sloRPM, "burst": sloBurst, "max_concurrent_streams": sloStreams},
		Telemetry:       map[string]any{"tracing": map[string]any{"enabled": true, "endpoint": collector.Endpoint(), "sample_ratio": 1}},
	}); err != nil {
		collector.Close()
		return "", "", nil, err
	}
	gw, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{DiscardLogs: true})
	if err != nil {
		collector.Close()
		return "", "", nil, err
	}
	stop := func() {
		if err := gw.Stop(30 * time.Second); err != nil {
			fmt.Fprintf(stderr, "warning: %v\n%s", err, gw.Stderr())
		}
		fmt.Fprintf(stderr, "the collector received %d spans in %d exports\n", collector.Spans(), collector.Exports())
		collector.Close()
	}
	return gw.PublicURL + "/v1/chat/completions", key, stop, nil
}

// sloMeasure starts a fresh upstream, lets target put the gateway (or
// nothing) in front of it, and runs the open loop against the URL it
// returns.
func sloMeasure(ctx context.Context, o sloOptions, name string,
	target func(baseURL string) (url, key string, stop func(), err error)) (sloRun, error) {
	up := loadkit.NewUpstream(loadkit.UpstreamOptions{
		Latency: o.upstreamLatency, ChunkDelay: o.chunkDelay, RecordTimings: true,
	})
	defer up.Close()
	url, key, stop, err := target(up.BaseURL())
	if err != nil {
		return sloRun{}, err
	}
	defer stop()

	// Enough idle connections for the requests in flight at the rate.
	transport := &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	plain, streaming := requestBody(false), streamBody()

	start := time.Now().Add(100 * time.Millisecond)
	measureFrom := start.Add(o.warmup)
	end := measureFrom.Add(o.duration)
	total := int(o.rate*(o.warmup+o.duration).Seconds()) + 1
	results := make([]sloResult, total)
	// Every 1/streamRatio-th request streams, spread evenly.
	isStream := func(i int) bool {
		return o.streamRatio > 0 && int(float64(i+1)*o.streamRatio) > int(float64(i)*o.streamRatio)
	}
	n := loadkit.OpenLoop(ctx, o.rate, start, end, func(i int, due time.Time) {
		if i >= len(results) {
			return
		}
		r := &results[i]
		r.stream = isStream(i)
		r.measured = !due.Before(measureFrom)
		body := plain
		if r.stream {
			body = streaming
		}
		began := time.Now()
		r.lag = began.Sub(due)
		r.err = sendSeq(ctx, client, url, key, body, r.stream, i)
		r.total = time.Since(began)
	})
	if err := ctx.Err(); err != nil {
		return sloRun{}, fmt.Errorf("interrupted: %w", err)
	}
	run := sloRun{name: name, results: results[:min(n, len(results))], upstream: up, elapsed: o.duration}
	return run, nil
}

// streamBody is a streaming request that asks for the usage chunk, as the
// gateway does upstream.
func streamBody() []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say something short."}],"stream":true,"stream_options":{"include_usage":true}}`)
}

// sendSeq is send with the sequence header set.
func sendSeq(ctx context.Context, client *http.Client, url, key string, body []byte, stream bool, seq int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(loadkit.SeqHeader, strconv.Itoa(seq))
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

// sloStats are the sorted distributions of one run's measured requests.
type sloStats struct {
	requests, errors int
	firstErr         error
	total            []time.Duration // client latency
	added            []time.Duration // client latency minus upstream time
	addedPlain       []time.Duration
	addedStream      []time.Duration
	lag              []time.Duration
	missing          int
}

func (r sloRun) stats() sloStats {
	var s sloStats
	for i, res := range r.results {
		if !res.measured {
			continue
		}
		s.requests++
		s.lag = append(s.lag, res.lag)
		if res.err != nil {
			s.errors++
			if s.firstErr == nil {
				s.firstErr = res.err
			}
			continue
		}
		up, ok := r.upstream.Timing(i)
		if !ok {
			s.missing++
			continue
		}
		added := res.total - up
		s.total = append(s.total, res.total)
		s.added = append(s.added, added)
		if res.stream {
			s.addedStream = append(s.addedStream, added)
		} else {
			s.addedPlain = append(s.addedPlain, added)
		}
	}
	for _, d := range [][]time.Duration{s.total, s.added, s.addedPlain, s.addedStream, s.lag} {
		slices.Sort(d)
	}
	return s
}

// sloReport prints the Markdown report, also to o.out when set, and checks
// the gates.
func sloReport(stdout io.Writer, o sloOptions, host loadkit.Host, loadBefore, loadAfter string, direct, via sloRun) error {
	var b strings.Builder
	d, g := direct.stats(), via.stats()
	p := loadkit.Percentile
	ms := loadkit.Ms

	fmt.Fprintf(&b, "## SLO bench: %.0f req/s open loop, %s measured after a %s warmup\n\n", o.rate, o.duration, o.warmup)
	fmt.Fprintf(&b, "- Hardware: %s\n", host)
	fmt.Fprintf(&b, "- Load average (1, 5, 15 min): %s before, %s after\n", loadBefore, loadAfter)
	fmt.Fprintf(&b, "- Fake upstream: %s to the first byte, %s between stream events; %.0f%% of requests stream (with usage), the rest do not; caching off; gateway log level %s\n",
		o.upstreamLatency, o.chunkDelay, o.streamRatio*100, o.logLevel)
	fmt.Fprintf(&b, "- Gateway: production-shaped hot path: memory-mode key rate limit (%d rpm, burst %d) and max_concurrent_streams %d, never reached; tracing on, sample_ratio 1, every span exported to an in-process OTLP collector\n",
		sloRPM, sloBurst, sloStreams)
	fmt.Fprintf(&b, "- Added latency per request = the client's latency (request sent to last byte read) minus the upstream's own time for that request (handler start to its last write), matched by the %s header. Through the gateway it is everything Switchyard adds, including the extra loopback hop; directly it is the cost of one hop and the client, for reference.\n\n", loadkit.SeqHeader)

	fmt.Fprintln(&b, "| target | requests | errors | req/s | latency p50 | p99 | added p50 | added p90 | added p99 | added max |")
	fmt.Fprintln(&b, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, row := range []struct {
		name string
		s    sloStats
	}{{"direct", d}, {"gateway", g}} {
		s := row.s
		fmt.Fprintf(&b, "| %s | %d | %d | %.0f | %s | %s | %s | %s | %s | %s |\n", row.name, s.requests, s.errors,
			float64(s.requests)/o.duration.Seconds(), ms(p(s.total, 0.5)), ms(p(s.total, 0.99)),
			ms(p(s.added, 0.5)), ms(p(s.added, 0.9)), ms(p(s.added, 0.99)), ms(p(s.added, 1)))
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Through the gateway, by mode:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| mode | requests | added p50 | added p90 | added p99 |")
	fmt.Fprintln(&b, "|---|---:|---:|---:|---:|")
	for _, row := range []struct {
		name string
		d    []time.Duration
	}{{"non-stream", g.addedPlain}, {"stream", g.addedStream}} {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n", row.name, len(row.d), ms(p(row.d, 0.5)), ms(p(row.d, 0.9)), ms(p(row.d, 0.99)))
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Gateway minus direct, percentile by percentile: p50 %s, p90 %s, p99 %s.\n",
		ms(p(g.added, 0.5)-p(d.added, 0.5)), ms(p(g.added, 0.9)-p(d.added, 0.9)), ms(p(g.added, 0.99)-p(d.added, 0.99)))
	fmt.Fprintf(&b, "Generator lag (how late a request left, open loop): direct p99 %s max %s; gateway p99 %s max %s.\n\n",
		ms(p(d.lag, 0.99)), ms(p(d.lag, 1)), ms(p(g.lag, 0.99)), ms(p(g.lag, 1)))

	for _, row := range []struct {
		name string
		s    sloStats
	}{{"direct", d}, {"gateway", g}} {
		if lag := p(row.s.lag, 0.99); lag > maxTrustedLag {
			fmt.Fprintf(&b, "**Warning**: the %s run's generator lag p99 is %s (over %s): the load generator, in the same process as the fake, could not keep its schedule, so the machine was stalling during that run and its tail is not trustworthy. Rerun on a quieter machine.\n\n",
				row.name, ms(lag), ms(maxTrustedLag))
		}
	}

	var failures []error
	for _, row := range []struct {
		name string
		s    sloStats
	}{{"direct", d}, {"gateway", g}} {
		if row.s.errors > 0 {
			failures = append(failures, fmt.Errorf("%d %s requests failed; the first: %w", row.s.errors, row.name, row.s.firstErr))
		}
		if row.s.missing > 0 {
			failures = append(failures, fmt.Errorf("%d %s requests have no upstream timing", row.s.missing, row.name))
		}
		if len(row.s.added) == 0 {
			failures = append(failures, fmt.Errorf("no %s request completed in the measured window", row.name))
		}
	}
	if v := p(g.added, 0.5); v > o.maxP50 {
		failures = append(failures, fmt.Errorf("gateway added p50 %s exceeds %s", ms(v), ms(o.maxP50)))
	}
	if v := p(g.added, 0.99); v > o.maxP99 {
		failures = append(failures, fmt.Errorf("gateway added p99 %s exceeds %s", ms(v), ms(o.maxP99)))
	}
	if len(failures) > 0 {
		fmt.Fprintln(&b, "**FAIL**")
		for _, f := range failures {
			fmt.Fprintf(&b, "- %v\n", f)
		}
	} else {
		fmt.Fprintf(&b, "**PASS**: gateway added p50 %s <= %s and p99 %s <= %s\n",
			ms(p(g.added, 0.5)), ms(o.maxP50), ms(p(g.added, 0.99)), ms(o.maxP99))
	}

	fmt.Fprint(stdout, b.String())
	if o.out != "" {
		if err := os.WriteFile(o.out, []byte(b.String()), 0o600); err != nil {
			failures = append(failures, fmt.Errorf("writing the report: %w", err))
		}
	}
	return errors.Join(failures...)
}
