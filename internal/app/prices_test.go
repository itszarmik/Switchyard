package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/gateway"
	"switchyard/internal/usage"
	"switchyard/test/eval/fakejudge"
	"switchyard/test/fakeupstream"
)

// priceBoundary is the price change of pricesYAML: m@up goes from input 1,
// output 2 to input 3, output 5 USD per 1M tokens.
var priceBoundary = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// pricesYAML renders a config with provider up at baseURL, model m with a
// dated price change at priceBoundary (and extraModels after it), and
// tenant test with a daily budget and the key hash.
func pricesYAML(baseURL, hash, extraModels string) string {
	return `version: 1
server: {listen: 127.0.0.1:0, admin_listen: 127.0.0.1:0, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: up, type: openai, base_url: "` + baseURL + `", api_key: "${OPENAI_API_KEY}"}
models:
  - id: m
    capabilities: [streaming, stream_usage]
    deployments:
      - provider: up
        upstream_model: m
        pricing:
          input: "1"
          output: "2"
          changes:
            - {from: "2026-06-15T12:00:00Z", input: "3", output: "5"}
` + extraModels + `tenants:
  - id: test
    budgets: [{scope: tenant, window: day, limit_usd: "1000"}]
    keys:
      - {id: key-a, hash: ` + hash + `}
`
}

// fakeClock is a clock the test sets, with a ticker and a timer it
// drives: a send on fire is the timer going off, and armed receives the
// duration of each timer set.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	ticks chan time.Time
	fire  chan time.Time
	armed chan time.Duration
	// onTicker, when set, runs when the price-change loop creates its
	// ticker, just before the loop first looks at the catalog.
	onTicker func()
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t, ticks: make(chan time.Time), fire: make(chan time.Time), armed: make(chan time.Duration, 64)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) clock(t *testing.T) clock {
	return clock{
		now: c.now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			if d != priceCheckInterval {
				t.Errorf("ticker interval %v, want %v", d, priceCheckInterval)
			}
			if c.onTicker != nil {
				c.onTicker()
			}
			return c.ticks, func() {}
		},
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			select {
			case c.armed <- d:
			default:
			}
			return c.fire, func() {}
		},
	}
}

// recordingLedger is a memory ledger that also hands each event to the
// test, which receives it once the request has settled its budgets.
type recordingLedger struct {
	*usage.MemoryLedger
	events  chan *usage.Event
	shadows chan *usage.ShadowEval
}

func (l *recordingLedger) Record(ev *usage.Event) {
	l.MemoryLedger.Record(ev)
	l.events <- ev
}

func (l *recordingLedger) RecordShadowEval(ctx context.Context, e *usage.ShadowEval) error {
	err := l.MemoryLedger.RecordShadowEval(ctx, e)
	l.shadows <- e
	return err
}

// pricesEnv is a running app on a fake clock.
type pricesEnv struct {
	*reloadEnv
	clk     *fakeClock
	ledger  *recordingLedger
	budgets budget.Store
	key     string
	hash    string
	checked chan error
	// fail makes the next stage calls fail while it is positive, and
	// afterStage, when set, runs after each stage call.
	failMu     sync.Mutex
	fail       int
	afterStage func()
	// firstArm is the duration of the loop's first timer, which
	// startPricesEnv waits for.
	firstArm time.Duration
}

func newPricesEnv(t *testing.T, start time.Time) *pricesEnv {
	t.Helper()
	up := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(up.Close)
	// 1M input and 1M output tokens: 3 USD before the change, 8 after.
	up.Script(fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}})
	return startPricesEnv(t, start, up, func(hash string) string { return pricesYAML(up.BaseURL(), hash, "") })
}

// startPricesEnv runs an app on the config yaml returns for the key hash,
// at start on a fake clock. up, which may be nil, is the fake upstream.
func startPricesEnv(t *testing.T, start time.Time, up *fakeupstream.Server, yaml func(hash string) string, opts ...func(*pricesEnv)) *pricesEnv {
	t.Helper()
	restoreDefaultLogger(t)
	key, hash := genKey(t)
	e := &pricesEnv{
		reloadEnv: &reloadEnv{t: t, path: t.TempDir() + "/switchyard.yaml", up: up, logs: newLogBuffer(),
			c: &http.Client{Timeout: 10 * time.Second}},
		clk: newFakeClock(start),
		ledger: &recordingLedger{MemoryLedger: usage.NewMemoryLedger(), events: make(chan *usage.Event, 16),
			shadows: make(chan *usage.ShadowEval, 16)},
		budgets: budget.NewMemoryStore(),
		key:     key,
		hash:    hash,
		checked: make(chan error),
	}
	e.write(yaml(hash))
	load := func() (*config.Snapshot, error) { return config.Load(e.path, config.LoadOptions{LookupEnv: fakeEnv}) }
	snap, err := load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := testNewWith(snap, e.logs, storage{ledger: e.ledger, budgets: e.budgets}, e.clk.clock(t))
	if err != nil {
		t.Fatal(err)
	}
	a.reload.load = load
	stage := a.reload.stage
	a.reload.stage = func(cfg *config.Config, v string, at time.Time) (*gateway.Staged, error) {
		e.failMu.Lock()
		defer e.failMu.Unlock()
		if e.fail > 0 {
			e.fail--
			return nil, errors.New("test: staging failed")
		}
		st, err := stage(cfg, v, at)
		if e.afterStage != nil {
			e.afterStage()
		}
		return st, err
	}
	a.priceChecked = func(err error) { e.checked <- err }
	for _, o := range opts {
		o(e)
	}
	e.app, e.run = a, startBuilt(t, a, e.logs)
	// Wait until the price loop has set its timer for a pending change,
	// so that a test that moves the clock past it afterwards finds the
	// loop waiting, as at a real boundary, rather than starting late.
	if next := a.gw.Catalog().NextPriceChange(); next.After(start) && e.clk.onTicker == nil {
		e.firstArm = next.Sub(start)
		e.armedFor(e.firstArm)
	}
	return e
}

// tick runs one price check from the minute ticker and returns its
// result.
func (e *pricesEnv) tick() error {
	e.t.Helper()
	return e.check(e.clk.ticks, "tick")
}

// fire sets the clock to at and runs the price check of the boundary
// timer going off, which must be set, and returns its result.
func (e *pricesEnv) fire(at time.Time) error {
	e.t.Helper()
	e.clk.set(at)
	return e.check(e.clk.fire, "timer")
}

// check sends on c, the ticker's or the timer's channel, and returns the
// result of the check it starts.
func (e *pricesEnv) check(c chan time.Time, what string) error {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	select {
	case c <- e.clk.now():
	case <-timeout:
		e.t.Fatalf("the price check loop took no %s", what)
	}
	select {
	case err := <-e.checked:
		return err
	case <-timeout:
		e.t.Fatal("the price check did not finish")
		return nil
	}
}

// armedFor waits until the loop sets its boundary timer for d.
func (e *pricesEnv) armedFor(d time.Duration) {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-e.clk.armed:
			if got == d {
				return
			}
		case <-timeout:
			e.t.Fatalf("the price check loop set no timer for %v", d)
		}
	}
}

// request sends a chat request for model m and returns its usage event.
func (e *pricesEnv) request() *usage.Event { return e.requestFor("m", "hi") }

// requestFor sends a chat request for model with the user message text and
// returns its usage event.
func (e *pricesEnv) requestFor(model, text string) *usage.Event {
	e.t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"` + text + `"}]}`
	if s, b := e.do(http.MethodPost, e.run.public+"/v1/chat/completions", e.key, body); s != http.StatusOK {
		e.t.Fatalf("chat: %d %s", s, b)
	}
	select {
	case ev := <-e.ledger.events:
		return ev
	case <-time.After(10 * time.Second):
		e.t.Fatal("no usage event")
		return nil
	}
}

// spent returns the tenant's daily budget spend in micro-USD.
func (e *pricesEnv) spent() int64 {
	e.t.Helper()
	scopes := budget.ScopesFor(e.app.reload.cfg.Tenants[0], "key-a")
	n, err := e.budgets.Spent(context.Background(), scopes[0], e.clk.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// metric returns the value of the series (name and labels) on the admin
// /metrics, failing the test if it is missing.
func (e *pricesEnv) metric(series string) float64 {
	e.t.Helper()
	_, body := e.do(http.MethodGet, e.run.admin+"/metrics", "", "")
	for line := range strings.SplitSeq(body, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				e.t.Fatalf("metric %s: %v", line, err)
			}
			return f
		}
	}
	e.t.Fatalf("metrics lack %s", series)
	return 0
}

// recentRequests returns the health tracker's recent request count of m@up
// from GET /admin/v1/providers.
func (e *pricesEnv) recentRequests() int64 {
	e.t.Helper()
	_, body := e.do(http.MethodGet, e.run.admin+"/admin/v1/providers", "", "")
	var r struct {
		Providers []struct {
			Deployments []struct {
				ID     string `json:"id"`
				Recent struct {
					Requests int64 `json:"requests"`
				} `json:"recent"`
			} `json:"deployments"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || len(r.Providers) != 1 || len(r.Providers[0].Deployments) == 0 {
		e.t.Fatalf("providers %s: %v", body, err)
	}
	return r.Providers[0].Deployments[0].Recent.Requests
}

// switchLines returns the "catalog prices changed" log lines.
func (e *pricesEnv) switchLines() []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(e.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "catalog prices changed" {
			out = append(out, m)
		}
	}
	return out
}

const (
	switchesOK    = `switchyard_catalog_price_switches_total{result="ok"}`
	switchesError = `switchyard_catalog_price_switches_total{result="error"}`
	nextChange    = "switchyard_catalog_next_price_change_timestamp_seconds"
)

// checkEvent checks a usage event's cost and prices.
func checkEvent(t *testing.T, ev *usage.Event, cost, input, output string) {
	t.Helper()
	if ev.Cost.String() != cost || !ev.CostKnown || ev.Pricing.Input.String() != input || ev.Pricing.Output.String() != output {
		t.Errorf("event cost %s (known %v) at input %s, output %s; want %s at %s, %s",
			ev.Cost, ev.CostKnown, ev.Pricing.Input, ev.Pricing.Output, cost, input, output)
	}
	if ev.Pricing.Changes != nil {
		t.Errorf("event pricing keeps the changes: %v", ev.Pricing.Changes)
	}
}

// TestPriceSwitchAtBoundary: a running gateway switches prices at the
// boundary, when its timer for the catalog's next change goes off, without
// a reload or a restart. The request before is costed at the old prices,
// the one just after at the new ones, in the ledger and in budget spend;
// health state carries over.
func TestPriceSwitchAtBoundary(t *testing.T) {
	// Seconds apart, so that every request stays in the health tracker's
	// recent window.
	e := newPricesEnv(t, priceBoundary.Add(-5*time.Second))
	version := e.app.gw.ConfigVersion()
	// The timer is set for the boundary.
	if e.firstArm != 5*time.Second {
		t.Errorf("first timer %v, want 5s", e.firstArm)
	}

	ev := e.request()
	checkEvent(t, ev, "3", "1", "2")
	if got := e.spent(); got != 3_000_000 {
		t.Errorf("spent %d micro-USD, want 3000000", got)
	}
	if got := e.metric(nextChange); got != float64(priceBoundary.Unix()) {
		t.Errorf("%s = %v, want %d", nextChange, got, priceBoundary.Unix())
	}
	// Before the boundary, a check changes nothing.
	if err := e.tick(); !errors.Is(err, errNoPriceChange) {
		t.Errorf("check before the boundary: %v, want errNoPriceChange", err)
	}
	if got := e.recentRequests(); got != 1 {
		t.Fatalf("recent requests %d, want 1", got)
	}

	// The timer goes off at the boundary; a request a millisecond later
	// has the new prices.
	if err := e.fire(priceBoundary); err != nil {
		t.Fatalf("check at the boundary: %v", err)
	}
	e.clk.set(priceBoundary.Add(time.Millisecond))
	ev = e.request()
	checkEvent(t, ev, "8", "3", "5")
	if got := e.spent(); got != 11_000_000 {
		t.Errorf("spent %d micro-USD, want 3+8 USD = 11000000", got)
	}
	if ev.ConfigVersion != version || e.app.gw.ConfigVersion() != version {
		t.Errorf("config version %s (event %s), want %s unchanged", e.app.gw.ConfigVersion(), ev.ConfigVersion, version)
	}
	// The health tracker kept its state: 2 requests in its window.
	if got := e.recentRequests(); got != 2 {
		t.Errorf("recent requests %d, want 2", got)
	}
	// A second check finds nothing more to do.
	if err := e.tick(); !errors.Is(err, errNoPriceChange) {
		t.Errorf("second check: %v, want errNoPriceChange", err)
	}

	for series, want := range map[string]float64{
		switchesOK: 1, switchesError: 0, nextChange: 0,
		`switchyard_config_reloads_total{result="ok"}`: 0,
	} {
		if got := e.metric(series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	lines := e.switchLines()
	if len(lines) != 1 {
		t.Fatalf("price switch lines %v, want 1", lines)
	}
	l := lines[0]
	if l["level"] != "INFO" || l["trigger"] != "schedule" || l["config_version"] != version || l["next_price_change"] != "none" ||
		!slices.Equal(anyStrings(l["deployments"]), []string{"m@up"}) ||
		!slices.Equal(anyStrings(l["changes"]), []string{"m@up: input 1 -> 3, output 2 -> 5"}) {
		t.Errorf("price switch line %v", l)
	}
	if strings.Contains(e.logs.String(), openAIKey) {
		t.Error("the logs contain the API key")
	}
}

// TestPriceSwitchRetries: a failed rebuild keeps the old prices, is
// logged and counted, and is tried again at the next check.
func TestPriceSwitchRetries(t *testing.T) {
	e := newPricesEnv(t, priceBoundary)
	// Built at the boundary, the catalog already has the new prices.
	checkEvent(t, e.request(), "8", "3", "5")

	e = newPricesEnv(t, priceBoundary.Add(-time.Second))
	e.clk.set(priceBoundary)
	e.failMu.Lock()
	e.fail = 2
	e.failMu.Unlock()
	for i := range 2 {
		if err := e.tick(); err == nil || !strings.Contains(err.Error(), "test: staging failed") {
			t.Fatalf("check %d: %v, want the staging error", i, err)
		}
		checkEvent(t, e.request(), "3", "1", "2")
	}
	if got := e.metric(switchesError); got != 2 {
		t.Errorf("%s = %v, want 2", switchesError, got)
	}
	if got := e.metric(nextChange); got != float64(priceBoundary.Unix()) {
		t.Errorf("%s = %v, want the pending change", nextChange, got)
	}
	if n := strings.Count(e.logs.String(), `"msg":"catalog price change failed; the prices in force keep serving and the change is tried again at the next check"`); n != 2 {
		t.Errorf("%d failure lines, want 2:\n%s", n, e.logs.String())
	}
	if err := e.tick(); err != nil {
		t.Fatalf("third check: %v", err)
	}
	checkEvent(t, e.request(), "8", "3", "5")
	if got := e.metric(switchesOK); got != 1 {
		t.Errorf("%s = %v, want 1", switchesOK, got)
	}
}

// TestPriceSwitchByReload: a reload after the boundary, before the next
// check, rebuilds the snapshot even though the configuration is the same,
// and a later reload keeps the new prices.
func TestPriceSwitchByReload(t *testing.T) {
	e := newPricesEnv(t, priceBoundary.Add(-time.Second))
	version := e.app.gw.ConfigVersion()
	e.clk.set(priceBoundary.Add(time.Second))

	status, r := e.reload("")
	if status != http.StatusOK || r.Unchanged || r.Fingerprint != version {
		t.Fatalf("reload: %d %+v; want ok, not unchanged, fingerprint %s", status, r, version)
	}
	checkEvent(t, e.request(), "8", "3", "5")
	if err := e.tick(); !errors.Is(err, errNoPriceChange) {
		t.Errorf("check after the reload: %v, want errNoPriceChange", err)
	}
	if lines := e.switchLines(); len(lines) != 1 || lines[0]["trigger"] != "reload" ||
		!slices.Equal(anyStrings(lines[0]["deployments"]), []string{"m@up"}) {
		t.Errorf("price switch lines %v", lines)
	}

	// An identical config is now a no-op, and a changed one keeps the
	// prices in force.
	if status, r := e.reload(""); status != http.StatusOK || !r.Unchanged {
		t.Errorf("second reload: %d %+v; want unchanged", status, r)
	}
	e.write(pricesYAML(e.up.BaseURL(), e.hash,
		"  - id: m2\n    deployments: [{provider: up, upstream_model: m2, pricing: {input: \"1\", output: \"1\"}}]\n"))
	if status, r := e.reload(""); status != http.StatusOK || r.Unchanged || r.Fingerprint == version {
		t.Fatalf("third reload: %d %+v; want a new fingerprint", status, r)
	}
	checkEvent(t, e.request(), "8", "3", "5")
	if got := e.metric(switchesOK); got != 1 {
		t.Errorf("%s = %v, want 1", switchesOK, got)
	}
}

// anyStrings converts a decoded JSON array of strings.
func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// shadowYAML renders a config for shadow sampling at rate 1 through the
// fake judge at baseURL: auto requests are served by small and compared
// with the baseline large, judged by judge, whose prices go from input 1,
// output 2 to input 10, output 20 at priceBoundary.
func shadowYAML(baseURL, hash string) string {
	return `version: 1
server: {listen: 127.0.0.1:0, admin_listen: 127.0.0.1:0, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: fj, type: openai, base_url: "` + baseURL + `", api_key: "${OPENAI_API_KEY}"}
models:
  - id: small
    capabilities: [sampling]
    quality: {general: 0.9}
    deployments: [{provider: fj, upstream_model: small-up, pricing: {input: "0.10", output: "0.40"}}]
  - id: large
    capabilities: [sampling]
    quality: {general: 0.97}
    deployments: [{provider: fj, upstream_model: large-up, pricing: {input: "1", output: "2"}}]
  - id: judge
    capabilities: [sampling]
    quality: {general: 0.98}
    deployments:
      - provider: fj
        upstream_model: judge-up
        pricing: {input: "1", output: "2", changes: [{from: "2026-06-15T12:00:00Z", input: "10", output: "20"}]}
policies:
  - id: default
    baseline_model: large
    shadow: {sample_rate: 1.0}
evaluation:
  judge: {model: judge}
  shadow: {daily_budget_usd: "100", concurrency: 1}
tenants:
  - id: test
    keys:
      - {id: key-a, hash: ` + hash + `}
`
}

// shadowEval waits for the next shadow evaluation.
func (e *pricesEnv) shadowEval() *usage.ShadowEval {
	e.t.Helper()
	select {
	case ev := <-e.ledger.shadows:
		return ev
	case <-time.After(10 * time.Second):
		e.t.Fatal("no shadow evaluation")
		return nil
	}
}

// TestPriceSwitchReachesTheJudge: the shadow judge, built at startup,
// charges its calls at the prices in force, so a price switch changes
// the shadow spend charged to the shadow budget.
func TestPriceSwitchReachesTheJudge(t *testing.T) {
	// 1,000 input and 1,000 output tokens per call: the comparison with
	// large costs 0.003 USD, and each of the two judge calls 0.003 USD
	// before the change and 0.03 after it.
	fake := fakejudge.New(func(r fakejudge.Request) fakejudge.Reply {
		reply := fakejudge.Reply{Content: "Answer by " + r.Model + ".", PromptTokens: 1000, CompletionTokens: 1000}
		if r.Judge {
			reply.Content = fakejudge.Verdict("tie")
		}
		return reply
	})
	t.Cleanup(fake.Close)
	e := startPricesEnv(t, priceBoundary.Add(-5*time.Second), nil, func(hash string) string { return shadowYAML(fake.BaseURL(), hash) })
	// The shadow runner charges its budget on the App's clock, the one the
	// test sets, so its daily window is the fake clock's day.
	scope := e.app.shadow.Scope("test")
	spent := func() int64 {
		t.Helper()
		n, err := e.budgets.Spent(context.Background(), scope, e.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	e.requestFor("auto", "Say hello to guest 1.")
	ev := e.shadowEval()
	if ev.ErrorKind != "" || ev.Cost.String() != "0.009" {
		t.Errorf("shadow evaluation before the change: error %q, cost %s; want 0.003 + 2 x 0.003 = 0.009", ev.ErrorKind, ev.Cost)
	}
	if got := spent(); got != 9_000 {
		t.Errorf("shadow spend %d micro-USD, want 9000", got)
	}

	e.clk.set(priceBoundary.Add(5 * time.Second))
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	e.requestFor("auto", "Say hello to guest 2.")
	ev = e.shadowEval()
	if ev.ErrorKind != "" || ev.Cost.String() != "0.063" {
		t.Errorf("shadow evaluation after the change: error %q, cost %s; want 0.003 + 2 x 0.03 = 0.063", ev.ErrorKind, ev.Cost)
	}
	if got := spent(); got != 72_000 {
		t.Errorf("shadow spend %d micro-USD, want 9000 + 63000 = 72000", got)
	}
}

// semanticYAML renders a config whose default policy turns the semantic
// cache on, embedding with emb at baseURL, whose input price goes from 1
// to 3 at priceBoundary.
func semanticYAML(baseURL, hash string) string {
	return `version: 1
server: {listen: 127.0.0.1:0, admin_listen: 127.0.0.1:0, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: up, type: openai, base_url: "` + baseURL + `", api_key: "${OPENAI_API_KEY}"}
models:
  - id: m
    capabilities: [streaming, stream_usage]
    deployments: [{provider: up, upstream_model: m, pricing: {input: "1", output: "2"}}]
  - id: emb
    type: embedding
    dimensions: 8
    deployments:
      - provider: up
        upstream_model: emb-up
        pricing: {input: "1", changes: [{from: "2026-06-15T12:00:00Z", input: "3"}]}
policies:
  - id: default
    cache: {semantic: {enabled: true}}
semantic_cache: {embedding_model: emb}
tenants:
  - id: test
    keys:
      - {id: key-a, hash: ` + hash + `}
`
}

// TestPriceSwitchReachesTheEmbedder: the semantic cache's embedder, built
// at startup, prices its embeddings at the prices in force, so a price
// switch changes the embedding cost charged as the request's overhead.
func TestPriceSwitchReachesTheEmbedder(t *testing.T) {
	// Orthogonal vectors, so that every lookup misses.
	var n atomic.Int64
	up := fakeupstream.New(fakeupstream.Options{Embed: func(string) []float32 {
		v := make([]float32, 8)
		v[n.Add(1)%8] = 1
		return v
	}})
	t.Cleanup(up.Close)
	// Each embedding reports 1M tokens: 1 USD before the change, 3 after.
	up.Script(fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 1_000_000, CompletionTokens: 10}})
	e := startPricesEnv(t, priceBoundary.Add(-5*time.Second), up, func(hash string) string { return semanticYAML(up.BaseURL(), hash) })

	if ev := e.requestFor("m", "first question"); ev.CacheStatus != usage.CacheStatusMiss || ev.OverheadCost.String() != "1" {
		t.Errorf("before the change: cache %q, overhead %s; want a miss with 1 USD of embedding", ev.CacheStatus, ev.OverheadCost)
	}
	e.clk.set(priceBoundary.Add(5 * time.Second))
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	if ev := e.requestFor("m", "second question"); ev.CacheStatus != usage.CacheStatusMiss || ev.OverheadCost.String() != "3" {
		t.Errorf("after the change: cache %q, overhead %s; want a miss with 3 USD of embedding", ev.CacheStatus, ev.OverheadCost)
	}
}

// TestLivePrices: livePrices follows the catalog in force, and keeps the
// last prices it knew when a reload removes the deployment.
func TestLivePrices(t *testing.T) {
	e := newPricesEnv(t, priceBoundary.Add(-time.Second))
	prices := livePrices(e.app.gw, e.app.gw.Catalog().Deployment("m@up"))
	check := func(when, input, output string) {
		t.Helper()
		if p := prices(); p.Input.String() != input || p.Output.String() != output {
			t.Errorf("%s: prices %s/%s, want %s/%s", when, p.Input, p.Output, input, output)
		}
	}
	check("startup", "1", "2")
	e.clk.set(priceBoundary)
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	check("after the switch", "3", "5")

	e.write(strings.Replace(pricesYAML(e.up.BaseURL(), e.hash, ""), "  - id: m\n", "  - id: renamed\n", 1))
	if status, r := e.reload(""); status != http.StatusOK || r.Unchanged {
		t.Fatalf("reload: %d %+v", status, r)
	}
	if e.app.gw.Catalog().Deployment("m@up") != nil {
		t.Fatal("m@up is still in the catalog")
	}
	check("after a reload removed it", "3", "5")
}

// TestPriceTimerFollowsReload: the boundary timer is set again for the
// next change of the catalog a reload puts in force.
func TestPriceTimerFollowsReload(t *testing.T) {
	e := newPricesEnv(t, priceBoundary.Add(-10*time.Second))

	earlier := priceBoundary.Add(-4 * time.Second)
	e.write(strings.Replace(pricesYAML(e.up.BaseURL(), e.hash, ""), "2026-06-15T12:00:00Z", earlier.Format(time.RFC3339), 1))
	if status, r := e.reload(""); status != http.StatusOK || r.Unchanged {
		t.Fatalf("reload: %d %+v", status, r)
	}
	e.armedFor(6 * time.Second)
	if err := e.fire(earlier); err != nil {
		t.Fatalf("check at the new boundary: %v", err)
	}
	checkEvent(t, e.request(), "8", "3", "5")
}

// TestPriceSwitchByReloadReportsOnlyTheChange: a reload that edits prices
// while a dated change is due counts the switch once and logs only the
// dated change's prices; the edits are the reload's.
func TestPriceSwitchByReloadReportsOnlyTheChange(t *testing.T) {
	m2 := func(price string) string {
		return "  - id: m2\n    deployments: [{provider: up, upstream_model: m2, pricing: {input: \"" + price + "\", output: \"" + price + "\"}}]\n"
	}
	up := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(up.Close)
	e := startPricesEnv(t, priceBoundary.Add(-time.Second), up, func(hash string) string { return pricesYAML(up.BaseURL(), hash, m2("1")) })
	e.clk.set(priceBoundary.Add(time.Second))
	e.write(pricesYAML(up.BaseURL(), e.hash, m2("7")))
	if status, r := e.reload(""); status != http.StatusOK || r.Unchanged {
		t.Fatalf("reload: %d %+v", status, r)
	}
	lines := e.switchLines()
	if len(lines) != 1 || lines[0]["trigger"] != "reload" ||
		!slices.Equal(anyStrings(lines[0]["deployments"]), []string{"m@up"}) ||
		!slices.Equal(anyStrings(lines[0]["changes"]), []string{"m@up: input 1 -> 3, output 2 -> 5"}) {
		t.Errorf("price switch lines %v", lines)
	}
	if got := e.metric(switchesOK); got != 1 {
		t.Errorf("%s = %v, want 1", switchesOK, got)
	}
	if p := e.app.gw.Catalog().Deployment("m2@up").Prices; p.Input.String() != "7" {
		t.Errorf("m2@up input %s, want the reload's 7", p.Input)
	}
}

// judgeChoiceYAML is shadowYAML with a second judge deployment, judge@fk at
// 3/6, and a sampling policy that permits only judge@fj among the judge's
// deployments. Before the boundary judge@fj (1/2) is the cheapest; from it
// (10/20) judge@fk is.
func judgeChoiceYAML(baseURL, hash string) string {
	s := shadowYAML(baseURL, hash)
	s = strings.Replace(s, "models:\n", "  - {id: fk, type: openai, base_url: \""+baseURL+"\", api_key: \"${OPENAI_API_KEY}\"}\nmodels:\n", 1)
	s = strings.Replace(s, "        pricing: {input: \"1\", output: \"2\", changes: [{from: \"2026-06-15T12:00:00Z\", input: \"10\", output: \"20\"}]}\n",
		"        pricing: {input: \"1\", output: \"2\", changes: [{from: \"2026-06-15T12:00:00Z\", input: \"10\", output: \"20\"}]}\n"+
			"      - {provider: fk, upstream_model: judge-up, pricing: {input: \"3\", output: \"6\"}}\n", 1)
	return strings.Replace(s, "    baseline_model: large\n", "    baseline_model: large\n    allow: [small@fj, large@fj, judge@fj]\n", 1)
}

// TestPriceSwitchKeepsTheJudgeValid (P10 review H1): a dated change that
// makes a judge deployment the sampling policy refuses the cheapest
// neither blocks the price switch nor the startup after it: the judge is
// the cheapest permitted deployment.
func TestPriceSwitchKeepsTheJudgeValid(t *testing.T) {
	fake := fakejudge.New(func(r fakejudge.Request) fakejudge.Reply {
		if r.Judge {
			return fakejudge.Reply{Content: fakejudge.Verdict("tie")}
		}
		return fakejudge.Reply{Content: "Answer by " + r.Model + "."}
	})
	t.Cleanup(fake.Close)
	yaml := func(hash string) string { return judgeChoiceYAML(fake.BaseURL(), hash) }

	e := startPricesEnv(t, priceBoundary.Add(-time.Second), nil, yaml)
	if err := e.fire(priceBoundary); err != nil {
		t.Fatalf("price switch: %v", err)
	}
	if p := e.app.gw.Catalog().Deployment("judge@fj").Prices; p.Input.String() != "10" {
		t.Errorf("judge@fj input %s after the switch, want 10", p.Input)
	}
	// A reload of the same files after the boundary passes its checks.
	if status, r := e.reload(""); status != http.StatusOK {
		t.Errorf("reload after the boundary: %d %+v", status, r)
	}

	// A restart after the boundary.
	after := startPricesEnv(t, priceBoundary.Add(time.Hour), nil, yaml)
	if after.app.shadow == nil {
		t.Fatal("no shadow runner after a restart past the boundary")
	}
	after.requestFor("auto", "Say hello.")
	if ev := after.shadowEval(); ev.JudgeModel != "judge@fj" || ev.ErrorKind != "" {
		t.Errorf("judged by %s (error %q), want judge@fj", ev.JudgeModel, ev.ErrorKind)
	}
}

// checkedAlone waits for the result of a price check the loop runs on its own,
// without a tick or the timer.
func (e *pricesEnv) checkedAlone() error {
	e.t.Helper()
	select {
	case err := <-e.checked:
		return err
	case <-time.After(10 * time.Second):
		e.t.Fatal("the price check loop ran no check")
		return nil
	}
}

// TestPriceSwitchDueAtStart (P10 review N1): a change that becomes due
// between the startup catalog's build and the price loop's first look is
// switched at once, not at the next minute's tick.
func TestPriceSwitchDueAtStart(t *testing.T) {
	e := startPricesEnv(t, priceBoundary.Add(-time.Second), nil,
		func(hash string) string { return pricesYAML("http://127.0.0.1:1/v1", hash, "") },
		func(e *pricesEnv) { e.clk.onTicker = func() { e.clk.set(priceBoundary) } })
	if err := e.checkedAlone(); err != nil {
		t.Fatalf("check at start: %v", err)
	}
	if p := e.app.gw.Catalog().Deployment("m@up").Prices; p.Input.String() != "3" {
		t.Errorf("input %s after the start, want 3", p.Input)
	}
}

// TestPriceSwitchDueAfterReload (P10 review N1): a reload that commits a
// catalog built just before a change, which is due by the time the loop
// sets its timer again, is followed by a switch at once.
func TestPriceSwitchDueAfterReload(t *testing.T) {
	e := newPricesEnv(t, priceBoundary.Add(-10*time.Second))
	e.clk.set(priceBoundary.Add(-time.Second))
	e.failMu.Lock()
	e.afterStage = func() { e.clk.set(priceBoundary) }
	e.failMu.Unlock()
	e.write(pricesYAML(e.up.BaseURL(), e.hash, "  - id: m2\n    deployments: [{provider: up, upstream_model: m2, pricing: {input: \"1\", output: \"1\"}}]\n"))
	if status, r := e.reload(""); status != http.StatusOK || r.Unchanged {
		t.Fatalf("reload: %d %+v", status, r)
	}
	if err := e.checkedAlone(); err != nil {
		t.Fatalf("check after the reload: %v", err)
	}
	checkEvent(t, e.request(), "8", "3", "5")
}

// TestPriceSwitchFailureWaitsForTheTick: a due change whose switch fails
// is retried at the ticker, not at once again.
func TestPriceSwitchFailureWaitsForTheTick(t *testing.T) {
	e := startPricesEnv(t, priceBoundary.Add(-time.Second), nil,
		func(hash string) string { return pricesYAML("http://127.0.0.1:1/v1", hash, "") },
		func(e *pricesEnv) {
			e.fail = 1
			e.clk.onTicker = func() { e.clk.set(priceBoundary) }
		})
	if err := e.checkedAlone(); err == nil {
		t.Fatal("the first check succeeded, want the injected failure")
	}
	select {
	case err := <-e.checked:
		t.Fatalf("a second check ran without a tick: %v", err)
	default:
	}
	if err := e.tick(); err != nil {
		t.Fatalf("retry at the tick: %v", err)
	}
}
