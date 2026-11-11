package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Regression tests for the Phase 4 review findings: requests that could
// spend past a blocking budget, and usage events that misreport what a
// request did.

// microLimit is a budget limit of micro micro-USD.
func microLimit(t *testing.T, micro int64) config.Price {
	t.Helper()
	return mustPrice(t, fmt.Sprintf("%d.%06d", micro/1_000_000, micro%1_000_000))
}

// tenantDayBudget is t1's blocking day budget of micro micro-USD.
func tenantDayBudget(t *testing.T, micro int64) []config.Budget {
	return []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: microLimit(t, micro)}}
}

// spent returns what t1's first budget has spent today.
func (e *acctEnv) spent(t *testing.T) int64 {
	t.Helper()
	n, err := e.store.Spent(context.Background(), e.g.scopes[principal{"t1", "k1"}][0], e.now)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBudgetEstimateCountsChoices checks that n multiplies the output
// estimate: a request for 100 choices of 10 tokens, which the upstream
// bills as 1,000 output tokens, does not fit a budget with room for one.
func TestBudgetEstimateCountsChoices(t *testing.T) {
	body := `{"model":"m","n":100,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	in := int64(len(body) / 4)
	enableN := func(c *config.Config) {
		c.Models[0].Capabilities = append(c.Models[0].Capabilities, string(schema.CapN))
	}

	e := newAcctEnv(t, tenantDayBudget(t, in+10), nil, enableN)
	e.a.script(step{body: `{"object":"chat.completion","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":1000}}`})
	res := e.do(EndpointChatCompletions, body, nil)
	if res.status != 429 || e.a.count() != 0 {
		t.Errorf("status %d with %d upstream calls; want 429 before any call", res.status, e.a.count())
	}

	// With room for 100 × 10 output tokens, it is served and settled.
	e = newAcctEnv(t, tenantDayBudget(t, in+1000), nil, enableN)
	e.a.script(step{body: `{"object":"chat.completion","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":1000}}`})
	if res := e.do(EndpointChatCompletions, body, nil); res.status != 200 {
		t.Fatalf("status %d %+v", res.status, res.err)
	}
	if got := e.spent(t); got != 1006 {
		t.Errorf("spent %d, want 1006", got)
	}
}

// TestOutputsEstimate checks the number of completions each request shape
// generates, which the output estimate is multiplied by.
func TestOutputsEstimate(t *testing.T) {
	chat := []struct {
		body string
		want int64
	}{
		{`{"model":"m","messages":[{"role":"user","content":"x"}]}`, 1},
		{`{"model":"m","n":0,"messages":[{"role":"user","content":"x"}]}`, 1},
		{`{"model":"m","n":4,"messages":[{"role":"user","content":"x"}]}`, 4},
	}
	for _, tt := range chat {
		req, err := schema.DecodeChatRequest([]byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := chatOutputs(req); got != tt.want {
			t.Errorf("chatOutputs(%s) = %d, want %d", tt.body, got, tt.want)
		}
	}
	completions := []struct {
		body string
		want int64
	}{
		{`{"model":"m","prompt":"a"}`, 1},
		{`{"model":"m","prompt":"a","n":3}`, 3},
		{`{"model":"m","prompt":"a","n":2,"best_of":5}`, 5},
		{`{"model":"m","prompt":["a","b","c"],"n":2}`, 6},
		{`{"model":"m","prompt":["a","b"],"n":2,"best_of":3}`, 6},
	}
	for _, tt := range completions {
		req, err := schema.DecodeCompletionRequest([]byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := completionOutputs(req); got != tt.want {
			t.Errorf("completionOutputs(%s) = %d, want %d", tt.body, got, tt.want)
		}
	}
}

// TestCancelAfterUpstreamCharged checks that a client cannot spend past a
// blocking budget by going away once the upstream has its request: such a
// request is charged its estimate, so the next one is refused.
func TestCancelAfterUpstreamCharged(t *testing.T) {
	body := `{"model":"m","max_tokens":4000,"messages":[{"role":"user","content":"long prompt"}]}`
	est := int64(len(body)/4) + 4000
	e := newAcctEnv(t, tenantDayBudget(t, est), nil, nil)
	e.a.script(step{hang: true})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for e.a.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if res := e.doAs(ctx, t1, EndpointChatCompletions, body, nil); res.status != StatusClientClosedRequest {
		t.Fatalf("status %d", res.status)
	}
	if got := e.spent(t); got != est {
		t.Errorf("spent %d after a canceled request the upstream had, want its estimate %d", got, est)
	}
	ev := e.sink.only(t)
	if !ev.UsageEstimated || ev.UsageKnown || ev.CostKnown || ev.Cost.MicroUSDCeil() != est ||
		ev.Deployment != "m@a" || ev.Provider != "a" {
		t.Errorf("event %+v", ev)
	}

	// The budget is spent: the next request is refused before any call.
	res := e.do(EndpointChatCompletions, body, nil)
	if res.status != 429 || res.err.Code != codeBudgetExceeded || e.a.count() != 1 {
		t.Errorf("repeat: status %d, %d upstream calls; want 429 and no new call", res.status, e.a.count())
	}
}

// TestUnpricedRefusedByExhaustedBudget checks that a deployment without
// prices, whose estimate is 0, is still refused by a spent blocking
// budget, and charged 1 micro-USD when served.
func TestUnpricedRefusedByExhaustedBudget(t *testing.T) {
	unpriced := func(c *config.Config) {
		for i := range c.Models[0].Deployments {
			c.Models[0].Deployments[i].Pricing = config.Pricing{}
		}
	}
	body := `{"model":"m","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`

	e := newAcctEnv(t, tenantDayBudget(t, 100), nil, unpriced)
	if _, err := e.store.Admit(context.Background(), []budget.Scope{e.g.scopes[principal{"t1", "k1"}][0]}, 100, e.now); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		res := e.do(EndpointChatCompletions, body, nil)
		if res.status != 429 {
			t.Errorf("request %d on a spent budget: status %d", i+1, res.status)
		}
	}
	if n := e.a.count() + e.b.count() + e.c.count(); n != 0 || e.spent(t) != 100 {
		t.Errorf("%d upstream calls, spent %d; want none and 100", n, e.spent(t))
	}

	e = newAcctEnv(t, tenantDayBudget(t, 100), nil, unpriced)
	if res := e.do(EndpointChatCompletions, body, nil); res.status != 200 {
		t.Fatalf("status %d %+v", res.status, res.err)
	}
	if got := e.spent(t); got != 1 {
		t.Errorf("spent %d after an unpriced request, want the minimum 1", got)
	}
}

// TestBrokenStreamUsageEstimated checks a stream that broke off after its
// upstream reported partial usage, as Anthropic's message_start does: the
// input classes reported are kept, the output is the larger of the one
// reported and the output relayed, and the usage is estimated.
func TestBrokenStreamUsageEstimated(t *testing.T) {
	text := strings.Repeat("abcd", 1000) // 4,000 bytes: 1,000 tokens
	chunks := []string{
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":1000,"completion_tokens":1,"total_tokens":1001}}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}`,
	}
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for _, tt := range []struct {
		name string
		end  error
	}{
		{"upstream drops", errTransport},
		{"client goes away", errCanceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcctEnv(t, nil, nil, nil)
			e.a.script(step{chunks: chunks, end: tt.end})
			e.do(EndpointChatCompletions, body, nil)
			ev := e.sink.only(t)
			if ev.Tokens != (usage.Tokens{Input: 1000, Output: 1000}) || !ev.UsageEstimated || ev.UsageKnown ||
				ev.Cost.String() != "0.002" || !ev.CostKnown {
				t.Errorf("event tokens %+v estimated %v known %v cost %s", ev.Tokens, ev.UsageEstimated, ev.UsageKnown, ev.Cost)
			}
		})
	}

	// A complete stream keeps its usage as reported.
	e := newAcctEnv(t, nil, nil, nil)
	e.a.script(step{chunks: append(chunks[:1:1], `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`)})
	e.do(EndpointChatCompletions, body, nil)
	if ev := e.sink.only(t); ev.Tokens != (usage.Tokens{Input: 1000, Output: 1}) || !ev.UsageKnown || ev.UsageEstimated {
		t.Errorf("complete stream: tokens %+v known %v estimated %v", ev.Tokens, ev.UsageKnown, ev.UsageEstimated)
	}
}

// TestPanicEventStatus checks that a request whose handler panicked is
// recorded with the 500 its client gets, and still settled.
func TestPanicEventStatus(t *testing.T) {
	e := newAcctEnv(t, tenantDayBudget(t, 1_000_000), nil, nil)
	e.g.adapters["a"] = panicky{e.a}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the handler's caller")
			}
		}()
		e.do(EndpointChatCompletions, chatBody, nil)
	}()
	if ev := e.sink.only(t); ev.Status != 500 {
		t.Errorf("event status %d, want 500", ev.Status)
	}
	if got := e.spent(t); got != 0 {
		t.Errorf("spent %d after a panic before the upstream had the request, want 0", got)
	}
}

// TestBudgetFallbackSkipMetric checks that a pricier fallback passed over
// for budget is counted as a skip, not as a 429 budget rejection.
func TestBudgetFallbackSkipMetric(t *testing.T) {
	est := int64(len(chatBody)/4) + defaultEstOut
	e := newAcctEnv(t, tenantDayBudget(t, est+10), nil, nil)
	e.a.script(step{err: err503})
	res := e.do(EndpointChatCompletions, chatBody, nil)
	if res.status == 429 {
		t.Fatalf("status %d", res.status)
	}
	if got := testutil.ToFloat64(e.g.metrics.budgetRejections); got != 0 {
		t.Errorf("budget_rejections_total = %v for a %d answer", got, res.status)
	}
	if got := testutil.ToFloat64(e.g.metrics.budgetFallbackSkips); got != 2 {
		t.Errorf("budget_fallback_skips_total = %v, want 2 (b and c)", got)
	}
}
