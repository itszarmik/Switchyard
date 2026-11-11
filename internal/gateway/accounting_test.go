package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/usage"
)

// captureSink is a usage.Sink that keeps every event.
type captureSink struct {
	mu  sync.Mutex
	evs []*usage.Event
}

func (s *captureSink) Record(ev *usage.Event) {
	s.mu.Lock()
	s.evs = append(s.evs, ev)
	s.mu.Unlock()
}

func (s *captureSink) Close(context.Context) error { return nil }

func (s *captureSink) events() []*usage.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*usage.Event(nil), s.evs...)
}

// only returns the only event recorded, failing the test otherwise.
func (s *captureSink) only(t *testing.T) *usage.Event {
	t.Helper()
	evs := s.events()
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	return evs[0]
}

// acctEnv is an execEnv whose gateway records usage events and enforces
// budgets for tenant t1, key k1.
type acctEnv struct {
	*execEnv
	sink  *captureSink
	store budget.Store
	now   time.Time
}

// newAcctEnv builds an acctEnv with t1's budgets, the budget store (nil for
// memory) and the config edited by edit. Prices: m costs 1/1 on a, 2/2 on
// b and 3/3 on c per 1M tokens, which is 1, 2 and 3 micro-USD per token.
func newAcctEnv(t *testing.T, budgets []config.Budget, store budget.Store, edit func(*config.Config)) *acctEnv {
	t.Helper()
	if store == nil {
		store = budget.NewMemoryStore()
	}
	sink := &captureSink{}
	now := time.Date(2026, 11, 11, 15, 30, 0, 0, time.UTC)
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	e := newExecEnvOpts(t, r, func(c *config.Config) {
		c.Tenants = []config.Tenant{{ID: "t1", Keys: []config.APIKey{{ID: "k1"}}, Budgets: budgets}}
		if edit != nil {
			edit(c)
		}
	}, func(o *Options) {
		o.Ledger, o.Budgets, o.ConfigVersion = sink, store, "cfg-v1"
		o.Clock = func() time.Time { return now }
	})
	return &acctEnv{execEnv: e, sink: sink, store: store, now: now}
}

// doAs serves body on endpoint as principal p (no principal when p is
// zero), with headers h.
func (e *acctEnv) doAs(ctx context.Context, p auth.Principal, endpoint, body string, h http.Header) exchangeResult {
	ann := &reqctx.Annotations{}
	ctx = reqctx.WithAnnotations(reqctx.WithLogger(ctx, slog.New(slog.NewJSONHandler(e.logs, nil))), ann)
	ctx = reqctx.WithRequestID(ctx, "req-1")
	if p.TenantID != "" {
		ctx = auth.WithPrincipal(ctx, p)
	}
	x := &hdrExchange{recExchange: recExchange{body: []byte(body)}, header: h}
	switch endpoint {
	case EndpointCompletions:
		e.g.Completions(ctx, x)
	case EndpointEmbeddings:
		e.g.Embeddings(ctx, x)
	default:
		e.g.ChatCompletions(ctx, x)
	}
	x.res.ann = annotations(ann)
	return x.res
}

var t1 = auth.Principal{TenantID: "t1", KeyID: "k1"}

func (e *acctEnv) do(endpoint, body string, h http.Header) exchangeResult {
	return e.doAs(context.Background(), t1, endpoint, body, h)
}

func money(t *testing.T, s string) usage.Money {
	t.Helper()
	m, err := usage.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustPrice(t *testing.T, s string) config.Price {
	t.Helper()
	p, err := config.ParsePrice(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// TestUsageEventPerRequest checks that every request an authenticated
// caller makes is recorded exactly once, whatever its endpoint and
// outcome, with what it used and cost.
func TestUsageEventPerRequest(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		body     string
		header   http.Header
		script   []step
		cancel   bool
		check    func(t *testing.T, res exchangeResult, ev *usage.Event)
	}{
		{name: "chat", body: chatBody, check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
			// 3 prompt + 1 completion tokens at 1 USD per 1M tokens.
			if res.info.CostUSD != "0.000004" || ev.Cost.String() != "0.000004" || !ev.CostKnown || !ev.UsageKnown || ev.UsageEstimated {
				t.Errorf("header %q, event cost %s known %v usage known %v", res.info.CostUSD, ev.Cost, ev.CostKnown, ev.UsageKnown)
			}
			if ev.Tokens != (usage.Tokens{Input: 3, Output: 1}) || ev.Status != 200 || ev.Deployment != "m@a" || ev.Provider != "a" ||
				ev.Endpoint != EndpointChatCompletions || ev.RequestedModel != "m" || ev.Attempts != 1 || ev.Stream ||
				ev.RouteReason != reasonCheapest || ev.CacheStatus != usage.CacheStatusMiss || ev.ConfigVersion != "cfg-v1" ||
				ev.ID != "req-1" || ev.Tenant != "t1" || ev.KeyID != "k1" || ev.BaselineModel != "m" ||
				!ev.BaselineCost.Equal(ev.Cost) || ev.Pricing.Input.String() != "1" || ev.LatencyMS <= 0 {
				t.Errorf("event %+v", ev)
			}
			if res.ann["cost_usd"] != "0.000004" {
				t.Errorf("access log cost_usd = %v", res.ann["cost_usd"])
			}
		}},
		{name: "chat stream", body: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
				// The usage chunk says 3 + 2 tokens; no cost header on a stream.
				if res.info.CostUSD != "" || ev.Cost.String() != "0.000005" || !ev.Stream || ev.Status != 200 || ev.TTFTMS <= 0 {
					t.Errorf("header %q, event %+v", res.info.CostUSD, ev)
				}
			}},
		{name: "completions", endpoint: EndpointCompletions, body: `{"model":"m","prompt":"hi"}`,
			script: []step{{body: `{"object":"text_completion","choices":[{"text":"hey"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`}},
			check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
				if res.info.CostUSD != "0.000015" || ev.Endpoint != EndpointCompletions || ev.Cost.String() != "0.000015" {
					t.Errorf("header %q, event %+v", res.info.CostUSD, ev)
				}
			}},
		{name: "embeddings", endpoint: EndpointEmbeddings, body: `{"model":"e","input":"hi"}`,
			check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
				// 2 prompt tokens at 1 USD per 1M tokens; no output price needed.
				if res.info.CostUSD != "0.000002" || ev.Tokens != (usage.Tokens{Input: 2}) || ev.Deployment != "e@a" || !ev.CostKnown {
					t.Errorf("header %q, event %+v", res.info.CostUSD, ev)
				}
			}},
		{name: "upstream rejects", body: chatBody, script: []step{{err: err400}},
			check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
				if ev.Status != 400 || ev.Deployment != "" || ev.Provider != "" || !ev.Cost.IsZero() || !ev.CostKnown || ev.Attempts != 1 {
					t.Errorf("event %+v", ev)
				}
			}},
		{name: "fallback", body: chatBody, script: []step{{err: err503}},
			check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
				// Served by b at 2/2: 8 micro-USD; the baseline is a at 1/1.
				if ev.Deployment != "m@b" || ev.Cost.String() != "0.000008" || ev.BaselineCost.String() != "0.000004" ||
					ev.FallbackReason != "server_error" || ev.Attempts != 2 || ev.RouteReason != "fallback:server_error" {
					t.Errorf("event %+v", ev)
				}
			}},
		{name: "invalid json", body: `{`, check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
			if ev.Status != 400 || ev.RequestedModel != "" || ev.Attempts != 0 {
				t.Errorf("event %+v", ev)
			}
		}},
		{name: "unknown model", body: `{"model":"zzz","messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
				if ev.Status != 404 || ev.RequestedModel != "zzz" || ev.BaselineModel != "" {
					t.Errorf("event %+v", ev)
				}
			}},
		{name: "tags", body: chatBody, header: http.Header{HeaderTags: {"team=search, env=prod"}},
			check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
				if len(ev.Tags) != 2 || ev.Tags["team"] != "search" || ev.Tags["env"] != "prod" {
					t.Errorf("tags %v", ev.Tags)
				}
			}},
		{name: "invalid tags", body: chatBody, header: http.Header{HeaderTags: {"Team=search"}},
			check: func(t *testing.T, res exchangeResult, ev *usage.Event) {
				if res.status != 400 || res.err.Code != codeInvalidTags || strings.Contains(res.err.Message, "Team") ||
					ev.Status != 400 || ev.Tags != nil {
					t.Errorf("status %d, error %+v, event %+v", res.status, res.err, ev)
				}
			}},
		{name: "client cancels", body: chatBody, cancel: true, script: []step{{hang: true}},
			check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
				// The upstream had the request: it is charged its estimate,
				// at a's 1 micro-USD per token, as an estimated, unknown cost.
				est := usage.Tokens{Input: int64(len(chatBody) / 4), Output: defaultEstOut}
				if ev.Status != StatusClientClosedRequest || ev.Deployment != "m@a" || ev.Provider != "a" ||
					!ev.UsageEstimated || ev.UsageKnown || ev.CostKnown || ev.Tokens != est ||
					ev.Cost.MicroUSDCeil() != est.Input+est.Output || ev.Pricing.Input.String() != "1" {
					t.Errorf("event %+v", ev)
				}
			}},
		{name: "client cancels before the upstream has the request", body: chatBody, cancel: true, script: []step{{hang: true, unsent: true}},
			check: func(t *testing.T, _ exchangeResult, ev *usage.Event) {
				if ev.Status != StatusClientClosedRequest || !ev.Cost.IsZero() || !ev.CostKnown || ev.Deployment != "" || ev.UsageEstimated {
					t.Errorf("event %+v", ev)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcctEnv(t, nil, nil, nil)
			if tt.script != nil {
				e.a.script(tt.script...)
			}
			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				go func() {
					for e.a.count() == 0 {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
			}
			endpoint := tt.endpoint
			if endpoint == "" {
				endpoint = EndpointChatCompletions
			}
			res := e.doAs(ctx, t1, endpoint, tt.body, tt.header)
			tt.check(t, res, e.sink.only(t))
		})
	}

	t.Run("no principal", func(t *testing.T) {
		e := newAcctEnv(t, nil, nil, nil)
		e.doAs(context.Background(), auth.Principal{}, EndpointChatCompletions, chatBody, nil)
		if n := len(e.sink.events()); n != 0 {
			t.Errorf("recorded %d events for an unauthenticated request", n)
		}
	})
}

// TestExactCost checks the cost of every token class against hand-computed
// decimals, in the response header, the event and the metrics.
func TestExactCost(t *testing.T) {
	pricing := func(in, out, cached, write string) config.Pricing {
		p := config.Pricing{Input: mustPrice(t, in), Output: mustPrice(t, out)}
		if cached != "" {
			p.CachedInput = mustPrice(t, cached)
		}
		if write != "" {
			p.CacheWrite = mustPrice(t, write)
		}
		return p
	}
	tests := []struct {
		name    string
		pricing config.Pricing
		usage   string
		want    string
		tokens  usage.Tokens
	}{
		// 1,234 × 0.15 + 567 × 0.6 = 185.1 + 340.2 = 525.3 per 1M.
		{"input and output", pricing("0.15", "0.6", "", ""),
			`{"prompt_tokens":1234,"completion_tokens":567}`, "0.0005253", usage.Tokens{Input: 1234, Output: 567}},
		// Uncached 6,197 − 2,051 − 2,051 = 2,095. 2,095 × 3 + 2,051 × 0.3
		// + 2,051 × 3.75 + 503 × 15 = 6,285 + 615.3 + 7,691.25 + 7,545 =
		// 22,136.55 per 1M.
		{"cached input and cache writes", pricing("3", "15", "0.3", "3.75"),
			`{"prompt_tokens":6197,"completion_tokens":503,"prompt_tokens_details":{"cached_tokens":2051,"cache_write_tokens":2051}}`,
			"0.02213655", usage.Tokens{Input: 2095, CachedInput: 2051, CacheWrite: 2051, Output: 503}},
		// Without their own prices, cached input and cache writes cost the
		// input price: (2,095 + 2,051 + 2,051) × 3 + 503 × 15 = 26,136 per 1M.
		{"fallback prices", pricing("3", "15", "", ""),
			`{"prompt_tokens":6197,"completion_tokens":503,"prompt_tokens_details":{"cached_tokens":2051,"cache_write_tokens":2051}}`,
			"0.026136", usage.Tokens{Input: 2095, CachedInput: 2051, CacheWrite: 2051, Output: 503}},
		// Nano-dollar prices stay exact: 7 × 0.000000001 + 3 × 0.123456789 =
		// 0.370370374 per 1M, at the femto-USD resolution.
		{"nine decimals", pricing("0.000000001", "0.123456789", "", ""),
			`{"prompt_tokens":7,"completion_tokens":3}`, "0.000000370370374", usage.Tokens{Input: 7, Output: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcctEnv(t, nil, nil, func(c *config.Config) {
				c.Models[0].Deployments = c.Models[0].Deployments[1:2] // a only
				c.Models[0].Deployments[0].Pricing = tt.pricing
			})
			e.a.script(step{body: `{"object":"chat.completion","choices":[{"message":{"content":"x"}}],"usage":` + tt.usage + `}`})
			res := e.do(EndpointChatCompletions, chatBody, nil)
			ev := e.sink.only(t)
			if res.info.CostUSD != tt.want || ev.Cost.String() != tt.want || ev.Tokens != tt.tokens {
				t.Errorf("header %q, event cost %s tokens %+v; want %s, %+v", res.info.CostUSD, ev.Cost, ev.Tokens, tt.want, tt.tokens)
			}
			got := testutil.ToFloat64(e.g.metrics.cost.WithLabelValues("a", "m"))
			if want := money(t, tt.want).Float64(); got != want {
				t.Errorf("cost_usd_total = %v, want %v", got, want)
			}
			if got := testutil.ToFloat64(e.g.metrics.tokens.WithLabelValues("a", "m", "output")); got != float64(tt.tokens.Output) {
				t.Errorf("tokens_total{output} = %v", got)
			}
		})
	}

	t.Run("unknown prices", func(t *testing.T) {
		e := newAcctEnv(t, nil, nil, func(c *config.Config) {
			c.Models[0].Deployments = c.Models[0].Deployments[1:2]
			c.Models[0].Deployments[0].Pricing = config.Pricing{Input: mustPrice(t, "1")}
		})
		res := e.do(EndpointChatCompletions, chatBody, nil)
		ev := e.sink.only(t)
		if res.info.CostUSD != "" || ev.CostKnown || !ev.Cost.IsZero() || !ev.UsageKnown {
			t.Errorf("header %q, event %+v", res.info.CostUSD, ev)
		}
	})
}

// TestEstimatedUsage checks the estimate recorded when the upstream
// reports no usage: the input estimate, and the output relayed at one
// token per 4 bytes.
func TestEstimatedUsage(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	e := newAcctEnv(t, nil, nil, nil)
	e.a.script(step{chunks: defaultChunks[:3]}) // "hel" + "lo", no usage chunk
	e.do(EndpointChatCompletions, body, nil)
	ev := e.sink.only(t)
	in := int64(len(body) / 4)
	want := usage.Tokens{Input: in, Output: 2} // 5 bytes → 2 tokens
	if !ev.UsageEstimated || ev.UsageKnown || ev.Tokens != want || ev.Cost.MicroUSDCeil() != in+2 {
		t.Errorf("event %+v, want estimated tokens %+v", ev, want)
	}

	// A stream broken off mid-way after its first event.
	e = newAcctEnv(t, nil, nil, nil)
	e.a.script(step{chunks: defaultChunks[:2], end: errTransport})
	e.do(EndpointChatCompletions, body, nil)
	ev = e.sink.only(t)
	if !ev.UsageEstimated || ev.Tokens != (usage.Tokens{Input: in, Output: 1}) || ev.Deployment != "m@a" {
		t.Errorf("broken stream event %+v", ev)
	}

	// A non-streaming answer without usage.
	e = newAcctEnv(t, nil, nil, nil)
	e.a.script(step{body: `{"object":"chat.completion","choices":[{"message":{"content":"12345678"}}]}`})
	res := e.do(EndpointChatCompletions, chatBody, nil)
	ev = e.sink.only(t)
	if !ev.UsageEstimated || ev.Tokens != (usage.Tokens{Input: int64(len(chatBody) / 4), Output: 2}) || res.info.CostUSD != "" {
		t.Errorf("non-stream event %+v, header %q", ev, res.info.CostUSD)
	}
}

// TestMetricsMatchLedger checks that cost_usd_total sums what the ledger
// records, and that the routing savings follow baseline − cost.
func TestMetricsMatchLedger(t *testing.T) {
	e := newAcctEnv(t, nil, nil, nil)
	e.a.script(step{}, step{err: err503}, step{}, step{err: err400})
	for range 4 {
		e.do(EndpointChatCompletions, chatBody, nil)
	}
	e.do(EndpointEmbeddings, `{"model":"e","input":"hi"}`, nil)
	var sum, savings usage.Money
	for _, ev := range e.sink.events() {
		sum = sum.Add(ev.Cost)
		if ev.CostKnown {
			savings = savings.Add(ev.BaselineCost.Sub(ev.Cost))
		}
	}
	var metric float64
	for _, l := range [][2]string{{"a", "m"}, {"b", "m"}, {"c", "m"}, {"a", "e"}, {"b", "e"}} {
		metric += testutil.ToFloat64(e.g.metrics.cost.WithLabelValues(l[0], l[1]))
	}
	if d := metric - sum.Float64(); d > 1e-12 || d < -1e-12 {
		t.Errorf("cost_usd_total %v, ledger %s", metric, sum)
	}
	// One fallback to b cost 8 micro-USD against a baseline of 4.
	if got := testutil.ToFloat64(e.g.metrics.savingsRouting); got != savings.Float64() || savings.String() != "-0.000004" {
		t.Errorf("savings_usd_total = %v, ledger %s", got, savings)
	}
}

// failingStore is a budget store that is down.
type failingStore struct{ budget.Store }

func (failingStore) Admit(context.Context, []budget.Scope, int64, time.Time) (budget.Admission, error) {
	return budget.Admission{}, fmt.Errorf("admit: %w", budget.ErrUnavailable)
}

func TestBudgets(t *testing.T) {
	// The estimate of chatBody on a: its input at bytes/4 plus 500 output
	// tokens, at 1 micro-USD per token.
	est := int64(len(chatBody)/4) + defaultEstOut
	limit := func(micro int64) config.Price {
		return mustPrice(t, fmt.Sprintf("%d.%06d", micro/1_000_000, micro%1_000_000))
	}
	tenantDay := func(micro int64, on string) []config.Budget {
		return []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: limit(micro), OnExhausted: on}}
	}
	scope := func(e *acctEnv) budget.Scope {
		return e.g.scopes[principal{"t1", "k1"}][0]
	}

	t.Run("admit and settle", func(t *testing.T) {
		e := newAcctEnv(t, tenantDay(est, ""), nil, nil)
		if res := e.do(EndpointChatCompletions, chatBody, nil); res.status != 200 {
			t.Fatalf("status %d %+v", res.status, res.err)
		}
		// Settled at the actual 4 micro-USD.
		if spent, _ := e.store.Spent(context.Background(), scope(e), e.now); spent != 4 {
			t.Errorf("spent %d, want 4", spent)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		e := newAcctEnv(t, tenantDay(est-1, ""), nil, nil)
		res := e.do(EndpointChatCompletions, chatBody, nil)
		if res.status != 429 || res.err.Code != codeBudgetExceeded || res.err.Type != typeInsufficientQuota ||
			res.err.RetryAfter != 8*time.Hour+30*time.Minute {
			t.Fatalf("status %d, error %+v", res.status, res.err)
		}
		if e.a.count() != 0 {
			t.Errorf("a rejected request reached the upstream: %s", e.calls())
		}
		if ev := e.sink.only(t); ev.Status != 429 || !ev.Cost.IsZero() {
			t.Errorf("event %+v", ev)
		}
		if got := testutil.ToFloat64(e.g.metrics.budgetRejections); got != 1 {
			t.Errorf("budget_rejections_total = %v", got)
		}
		// max_tokens lowers the estimate enough to fit.
		res = e.do(EndpointChatCompletions, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if res.status != 200 {
			t.Errorf("with max_tokens: status %d %+v", res.status, res.err)
		}
	})

	t.Run("notify", func(t *testing.T) {
		e := newAcctEnv(t, tenantDay(1, config.BudgetNotify), nil, nil)
		if res := e.do(EndpointChatCompletions, chatBody, nil); res.status != 200 {
			t.Fatalf("status %d %+v", res.status, res.err)
		}
		if got := testutil.ToFloat64(e.g.metrics.budgetNotices.WithLabelValues("t1/day", noticeExceeded)); got != 1 {
			t.Errorf("notifications{exceeded} = %v", got)
		}
	})

	t.Run("soft threshold", func(t *testing.T) {
		b := tenantDay(10*est, "")
		b[0].SoftAt = 0.05
		e := newAcctEnv(t, b, nil, nil)
		e.do(EndpointChatCompletions, chatBody, nil)
		e.do(EndpointChatCompletions, chatBody, nil)
		if got := testutil.ToFloat64(e.g.metrics.budgetNotices.WithLabelValues("t1/day", noticeSoftThreshold)); got != 1 {
			t.Errorf("notifications{soft_threshold} = %v, want 1", got)
		}
	})

	t.Run("pricier fallback", func(t *testing.T) {
		// Room for the estimate on a, not for the difference to b.
		e := newAcctEnv(t, tenantDay(est+10, ""), nil, nil)
		e.a.script(step{err: err503})
		res := e.do(EndpointChatCompletions, chatBody, nil)
		if res.status != 502 || e.b.count() != 0 || e.c.count() != 0 {
			t.Errorf("status %d, calls %s", res.status, e.calls())
		}
		// a had the request and answered 5xx, which may come after it did
		// the work: the estimate stays charged.
		if spent, _ := e.store.Spent(context.Background(), scope(e), e.now); spent != est {
			t.Errorf("spent %d after a 5xx from an upstream that had the request, want the estimate %d", spent, est)
		}
		// A dial failure never reached a: nothing is charged.
		e = newAcctEnv(t, tenantDay(est+10, ""), nil, nil)
		e.a.script(step{err: errTransport, unsent: true})
		if res := e.do(EndpointChatCompletions, chatBody, nil); res.status != 502 {
			t.Errorf("status %d, calls %s", res.status, e.calls())
		}
		if spent, _ := e.store.Spent(context.Background(), scope(e), e.now); spent != 0 {
			t.Errorf("spent %d after a request that never reached an upstream, want 0", spent)
		}

		// With room, the difference is charged before the fallback, and
		// the actual cost settled.
		e = newAcctEnv(t, tenantDay(3*est, ""), nil, nil)
		e.a.script(step{err: err503})
		if res := e.do(EndpointChatCompletions, chatBody, nil); res.status != 200 || res.info.Provider != "b" {
			t.Fatalf("status %d, info %+v", res.status, res.info)
		}
		if spent, _ := e.store.Spent(context.Background(), scope(e), e.now); spent != 8 {
			t.Errorf("spent %d, want 8", spent)
		}
	})

	t.Run("key budget", func(t *testing.T) {
		e := newAcctEnv(t, []config.Budget{
			{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowMonth, LimitUSD: limit(100 * est)},
			{Scope: config.BudgetScopeKey, Window: config.BudgetWindowDay, LimitUSD: limit(est - 1)},
		}, nil, nil)
		res := e.do(EndpointChatCompletions, chatBody, nil)
		if res.status != 429 || !strings.Contains(res.err.Message, "t1/key:k1/day") {
			t.Fatalf("status %d, error %+v", res.status, res.err)
		}
		// Midnight to midnight: the key's day budget resets first.
		if res.err.RetryAfter != 8*time.Hour+30*time.Minute {
			t.Errorf("Retry-After %v", res.err.RetryAfter)
		}
	})

	for _, mode := range []string{config.BudgetFailOpen, config.BudgetFailClosed} {
		t.Run("store down, fail_mode "+mode, func(t *testing.T) {
			e := newAcctEnv(t, tenantDay(est, ""), failingStore{budget.NewMemoryStore()}, func(c *config.Config) {
				c.Budgets.FailMode = mode
			})
			res := e.do(EndpointChatCompletions, chatBody, nil)
			want := 200
			if mode == config.BudgetFailClosed {
				want = 503
			}
			if res.status != want || (want == 503 && res.err.Code != codeBudgetUnavailable) {
				t.Errorf("status %d, error %+v; want %d", res.status, res.err, want)
			}
			if got := testutil.ToFloat64(e.g.metrics.budgetStoreErrors); got != 1 {
				t.Errorf("budget_store_errors_total = %v", got)
			}
		})
	}

	t.Run("utilization", func(t *testing.T) {
		e := newAcctEnv(t, tenantDay(8, ""), nil, nil)
		if _, err := e.store.Admit(context.Background(), []budget.Scope{scope(e)}, 2, e.now); err != nil {
			t.Fatal(err)
		}
		want := "\n# HELP switchyard_budget_utilization_ratio Spend in the current window as a fraction of the limit, by budget scope (for example acme/month or acme/key:k1/day).\n" +
			"# TYPE switchyard_budget_utilization_ratio gauge\nswitchyard_budget_utilization_ratio{scope=\"t1/day\"} 0.25\n"
		if err := testutil.GatherAndCompare(e.reg, strings.NewReader(want), "switchyard_budget_utilization_ratio"); err != nil {
			t.Error(err)
		}
	})
}

func TestParseTags(t *testing.T) {
	long := strings.Repeat("v", 64)
	tests := []struct {
		name   string
		values []string
		want   map[string]string
		ok     bool
	}{
		{"absent", nil, nil, true},
		{"empty", []string{" "}, nil, true},
		{"one", []string{"team=search"}, map[string]string{"team": "search"}, true},
		{"spaces", []string{" team = search , env=prod "}, map[string]string{"team": "search", "env": "prod"}, true},
		{"two lines", []string{"a=1", "b=2"}, map[string]string{"a": "1", "b": "2"}, true},
		{"value with space", []string{"note=a b"}, map[string]string{"note": "a b"}, true},
		{"longest value", []string{"k=" + long}, map[string]string{"k": long}, true},
		{"ten", []string{"a=1,b=1,c=1,d=1,e=1,f=1,g=1,h=1,i=1,j=1"}, map[string]string{
			"a": "1", "b": "1", "c": "1", "d": "1", "e": "1", "f": "1", "g": "1", "h": "1", "i": "1", "j": "1"}, true},
		{"eleven", []string{"a=1,b=1,c=1,d=1,e=1,f=1,g=1,h=1,i=1,j=1,k=1"}, nil, false},
		{"upper-case key", []string{"Team=x"}, nil, false},
		{"key starts with dash", []string{"-a=x"}, nil, false},
		{"key too long", []string{strings.Repeat("k", 33) + "=x"}, nil, false},
		{"no value", []string{"team="}, nil, false},
		{"no equals", []string{"team"}, nil, false},
		{"two equals", []string{"a=b=c"}, nil, false},
		{"empty pair", []string{"a=1,,b=2"}, nil, false},
		{"duplicate", []string{"a=1,a=2"}, nil, false},
		{"value too long", []string{"k=" + long + "v"}, nil, false},
		{"non-ASCII", []string{"k=café"}, nil, false},
		{"control", []string{"k=a\tb"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.values {
				h.Add(HeaderTags, v)
			}
			got, ok := parseTags(h)
			if ok != tt.ok || fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("parseTags(%q) = %v, %v; want %v, %v", tt.values, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestSettleAfterClientGone checks that a request whose client went away
// after the upstream had it is still settled, at its estimate, with a
// context that is not canceled.
func TestSettleAfterClientGone(t *testing.T) {
	e := newAcctEnv(t, []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "1")}}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	e.a.script(step{hang: true})
	go func() {
		for e.a.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	res := e.doAs(ctx, t1, EndpointChatCompletions, chatBody, nil)
	if res.status != StatusClientClosedRequest {
		t.Fatalf("status %d", res.status)
	}
	est := int64(len(chatBody)/4) + defaultEstOut
	if spent, err := e.store.Spent(context.Background(), e.g.scopes[principal{"t1", "k1"}][0], e.now); err != nil || spent != est {
		t.Errorf("spent %d, %v; want the estimate %d settled", spent, err, est)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("ctx not canceled")
	}
}
