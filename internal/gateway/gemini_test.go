package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/gemini"
	"switchyard/internal/providers/openai"
	"switchyard/internal/usage"
	"switchyard/test/fakegemini"
	"switchyard/test/fakeupstream"
)

// useGemini makes provider "a" of an exec env a real gemini adapter in
// front of a fake Gemini API, and returns the fake.
func useGemini(t *testing.T, e *execEnv) *fakegemini.Server {
	t.Helper()
	f := fakegemini.New(fakegemini.Options{APIKey: "AIzaSyGatewayTestKey000000000000000000"})
	t.Cleanup(f.Close)
	a, err := gemini.New(config.Provider{
		ID: "a", Type: "gemini", BaseURL: f.BaseURL(), APIKey: config.NewSecret("AIzaSyGatewayTestKey000000000000000000"),
	}, providers.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.CloseIdleConnections)
	e.g.current().adapters["a"] = a
	return f
}

// TestGeminiUsageCost checks, at the ledger, that Gemini's usage is costed
// as Google bills it, in a JSON response and in a stream: cached tokens at
// the cached-input price, the rest of the prompt (tool-use prompt tokens
// included) at the input price, and thinking tokens once, at the output
// price, as reasoning_tokens within completion_tokens.
func TestGeminiUsageCost(t *testing.T) {
	// Gemini 4 Carbon's prices: 2.00 input, 10.00 output, 0.10 cached.
	pricing := config.Pricing{Input: mustPrice(t, "2"), Output: mustPrice(t, "10"), CachedInput: mustPrice(t, "0.1")}
	// usageMetadata: 12,000 prompt (8,192 cached), 100 tool-use prompt,
	// 50 candidates, 300 thoughts. Uncached input = 12,100 − 8,192 =
	// 3,908; output = 350. 3,908 × 2 + 8,192 × 0.1 + 350 × 10 = 7,816 +
	// 819.2 + 3,500 = 12,135.2 per 1M.
	want := usage.Tokens{Input: 3908, CachedInput: 8192, Output: 350}
	const cost = "0.0121352"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			e := newAcctEnv(t, nil, nil, func(c *config.Config) {
				c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
				c.Models[0].Deployments = c.Models[0].Deployments[1:2] // a only
				c.Models[0].Deployments[0].Pricing = pricing
			})
			f := useGemini(t, e.execEnv)
			f.Script(fakegemini.Behavior{Usage: &fakegemini.Usage{Prompt: 12000, Cached: 8192, ToolUsePrompt: 100, Candidates: 50, Thoughts: 300}})
			body := chatBody
			if stream {
				body = `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
			}
			res := e.do(EndpointChatCompletions, body, nil)
			if res.err != nil {
				t.Fatalf("error %+v", res.err)
			}
			ev := e.sink.only(t)
			if ev.Tokens != want || ev.Cost.String() != cost || !ev.CostKnown || ev.UsageEstimated {
				t.Errorf("event tokens %+v cost %s (known %v, estimated %v); want %+v, %s", ev.Tokens, ev.Cost, ev.CostKnown, ev.UsageEstimated, want, cost)
			}
			if !stream && res.info.CostUSD != cost {
				t.Errorf("X-Switchyard-Cost-Usd %q, want %s", res.info.CostUSD, cost)
			}
			wire := res.body
			if stream {
				wire = strings.Join(res.events, "\n")
			}
			for _, s := range []string{`"prompt_tokens":12100`, `"completion_tokens":350`, `"cached_tokens":8192`, `"reasoning_tokens":300`} {
				if !strings.Contains(wire, s) {
					t.Errorf("client response lacks %s: %s", s, wire)
				}
			}
		})
	}
}

// TestGeminiUnsupportedFallsBack checks that a request the native API
// cannot express is never sent to Gemini: it goes to the next deployment
// of its plan, or, when Gemini is the only one, fails with a 400 naming
// the reason.
func TestGeminiUnsupportedFallsBack(t *testing.T) {
	bodies := map[string]string{
		"an https image URL": `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is it?"},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`,
		"a trailing assistant message": `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Sure, "}]}`,
	}
	reasons := map[string]string{"an https image URL": "image URLs", "a trailing assistant message": "prefilled"}
	for name, body := range bodies {
		t.Run(name+"/falls back", func(t *testing.T) {
			r := fastRouting()
			r.Fallback.RetriesPerDeployment = 0
			e := newExecEnv(t, r, func(c *config.Config) {
				c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
				c.Models[0].Capabilities = append(c.Models[0].Capabilities, "vision")
				c.Models[0].Deployments = c.Models[0].Deployments[1:3] // a (cheapest, Gemini), then b
				c.Models[0].Deployments[1].Capabilities = nil
			})
			f := useGemini(t, e)
			res := e.do(EndpointChatCompletions, body, nil)
			if res.err != nil || res.info.Provider != "b" || e.b.count() != 1 {
				t.Fatalf("error %+v, provider %q, b calls %d: want b to serve it", res.err, res.info.Provider, e.b.count())
			}
			if !strings.HasPrefix(res.info.RouteReason, "fallback:") {
				t.Errorf("route reason %q, want a fallback", res.info.RouteReason)
			}
			if n := len(f.Requests()); n != 0 {
				t.Errorf("Gemini received %d requests, want none", n)
			}
		})
		t.Run(name+"/only Gemini", func(t *testing.T) {
			e := newExecEnv(t, fastRouting(), func(c *config.Config) {
				c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
				c.Models[0].Capabilities = append(c.Models[0].Capabilities, "vision")
				c.Models[0].Deployments = c.Models[0].Deployments[1:2]
			})
			f := useGemini(t, e)
			res := e.do(EndpointChatCompletions, body, nil)
			if res.err == nil || res.err.Status != 400 || !strings.Contains(string(res.err.Body), reasons[name]) {
				t.Fatalf("error %+v (%s), want a 400 naming %q", res.err, res.err.Body, reasons[name])
			}
			if n := len(f.Requests()); n != 0 {
				t.Errorf("Gemini received %d requests, want none", n)
			}
		})
	}
}

// TestGeminiToOpenAI checks a tool conversation that starts on Gemini and
// continues on an OpenAI-wire deployment (here by fallback; auto routing
// and substitution do the same): the gemini adapter's ids, which carry
// thought signatures and are far over OpenAI's 40-character limit, reach
// the OpenAI-wire upstream shortened, the same in the assistant call and
// in the tool message, so the turn succeeds instead of failing with 400
// string_above_max_length. The fake OpenAI upstream enforces the limit.
func TestGeminiToOpenAI(t *testing.T) {
	const openaiKey = "sk-gateway-test-0123456789abcdef"
	fu := fakeupstream.New(fakeupstream.Options{APIKey: openaiKey})
	t.Cleanup(fu.Close)
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	e := newExecEnv(t, r, func(c *config.Config) {
		c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
		c.Models[0].Capabilities = append(c.Models[0].Capabilities, "tools")
		c.Models[0].Deployments = c.Models[0].Deployments[1:3] // a (cheapest, Gemini), then b
		c.Models[0].Deployments[1].Capabilities = nil
	})
	f := useGemini(t, e)
	b, err := openai.New(config.Provider{ID: "b", Type: "openai", BaseURL: fu.BaseURL(), APIKey: config.NewSecret(openaiKey)}, providers.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.CloseIdleConnections)
	e.g.current().adapters["b"] = b

	const tools = `"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}]`
	first := e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"weather and time?"}],`+tools+`}`, nil)
	if first.err != nil || first.info.Provider != "a" {
		t.Fatalf("turn 1: error %+v, provider %q; want Gemini to answer", first.err, first.info.Provider)
	}
	var c struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(first.body), &c); err != nil || len(c.Choices) != 1 {
		t.Fatalf("turn 1 body %s: %v", first.body, err)
	}
	var msg struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	_ = json.Unmarshal(c.Choices[0].Message, &msg)
	if len(msg.ToolCalls) != 2 || len(msg.ToolCalls[0].ID) <= openai.MaxToolCallIDLen {
		t.Fatalf("turn 1 tool calls %+v: want two, the first with a signed id over 40 characters", msg.ToolCalls)
	}
	// Signatures of 1 KiB and 16 KiB make ids of 1.4 KB and 21.9 KB; the
	// last call stands for one.
	huge := "gm1_" + strings.Repeat("Zz09-_", 3650)
	history := `{"role":"user","content":"weather and time?"},` + string(c.Choices[0].Message) + `,
		{"role":"tool","tool_call_id":"` + msg.ToolCalls[0].ID + `","content":"18C"},
		{"role":"tool","tool_call_id":"` + msg.ToolCalls[1].ID + `","content":"12:00"},
		{"role":"assistant","tool_calls":[{"id":"` + huge + `","type":"function","function":{"name":"get_time","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"` + huge + `","content":"12:01"}`

	f.Script(fakegemini.Behavior{Status: 503}) // Gemini is down: turn 2 falls back to b
	for _, stream := range []bool{false, true} {
		fu.Reset()
		body := `{"model":"m","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"messages":[` + history + `],` + tools + `}`
		second := e.do(EndpointChatCompletions, body, nil)
		if second.err != nil || second.info.Provider != "b" {
			t.Fatalf("turn 2 (stream %v): error %+v (%s), provider %q; want b to serve it", stream, second.err, errBody(second.err), second.info.Provider)
		}
		reqs := fu.Requests()
		if len(reqs) != 1 {
			t.Fatalf("the OpenAI upstream got %d requests", len(reqs))
		}
		sent := string(reqs[0].Body)
		for _, id := range []string{msg.ToolCalls[0].ID, msg.ToolCalls[1].ID, huge} {
			want := id
			if len(id) > openai.MaxToolCallIDLen {
				want = openai.ShortToolCallID(id)
			}
			if strings.Count(sent, `"`+want+`"`) != 2 {
				t.Errorf("id of %d characters sent as %q %d times, want twice", len(id), want, strings.Count(sent, `"`+want+`"`))
			}
		}
	}
}

// errBody returns e's body for a failure message, or "".
func errBody(e *Error) string {
	if e == nil {
		return ""
	}
	return string(e.Body)
}

// TestGeminiMalformedCallFallsBack checks that Gemini's
// MALFORMED_FUNCTION_CALL, which depends on the request, moves the request
// to the next deployment without a retry on Gemini (retries_per_deployment
// is 1 here) and is recorded as unsupported, an outcome the breaker and
// the provider error-rate alert do not count.
func TestGeminiMalformedCallFallsBack(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 1
	e := newExecEnv(t, r, func(c *config.Config) {
		c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
		c.Models[0].Capabilities = append(c.Models[0].Capabilities, "tools")
		c.Models[0].Deployments = c.Models[0].Deployments[1:3]
		c.Models[0].Deployments[1].Capabilities = nil
	})
	f := useGemini(t, e)
	f.Script(fakegemini.Behavior{FinishReason: "MALFORMED_FUNCTION_CALL"})
	res := e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`, nil)
	if res.err != nil || res.info.Provider != "b" {
		t.Fatalf("error %+v, provider %q: want b to serve it", res.err, res.info.Provider)
	}
	if n := len(f.Requests()); n != 1 {
		t.Errorf("Gemini got %d attempts, want 1 (no retry)", n)
	}
	if res.info.RouteReason != "fallback:unsupported" {
		t.Errorf("route reason %q, want fallback:unsupported", res.info.RouteReason)
	}
}

// TestGeminiBilledFailureCharged checks that an answer Google billed but
// the adapter failed (MALFORMED_FUNCTION_CALL: unsupported, so it falls
// back) is charged its exact usage, in the usage event, the cost metric
// and the budget spend, on top of the deployment that then serves the
// request; and when none does, alone.
func TestGeminiBilledFailureCharged(t *testing.T) {
	// The Gemini answer: 100 prompt, 20 candidates and 30 thinking tokens
	// at 1 input / 2 output (cheaper than b, so it is tried first): 100 × 1
	// + 50 × 2 = 200 per 1M, 200 µUSD. b's answer costs what its event says.
	const geminiMicro = 200
	day := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "10")}}
	for _, tt := range []struct {
		name     string
		fallback bool
		stream   bool
	}{
		{"falls back, JSON", true, false},
		{"falls back, stream", true, true},
		{"Gemini only", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcctEnv(t, day, nil, func(c *config.Config) {
				c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
				c.Models[0].Capabilities = append(c.Models[0].Capabilities, "tools")
				c.Models[0].Deployments = c.Models[0].Deployments[1:3] // a (Gemini), then b
				c.Models[0].Deployments[0].Pricing = config.Pricing{Input: mustPrice(t, "1"), Output: mustPrice(t, "2")}
				c.Models[0].Deployments[1].Capabilities = nil
				if !tt.fallback {
					c.Models[0].Deployments = c.Models[0].Deployments[:1]
				}
			})
			f := useGemini(t, e.execEnv)
			f.Script(fakegemini.Behavior{FinishReason: "MALFORMED_FUNCTION_CALL", Usage: &fakegemini.Usage{Prompt: 100, Candidates: 20, Thoughts: 30}})
			body := `{"model":"m","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
			if tt.stream {
				body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
			}
			res := e.do(EndpointChatCompletions, body, nil)
			ev := e.sink.only(t)
			want := int64(geminiMicro)
			if tt.fallback {
				if res.err != nil || res.info.Provider != "b" || ev.Deployment != "m@b" {
					t.Fatalf("error %+v, provider %q, event deployment %q: want b to serve it", res.err, res.info.Provider, ev.Deployment)
				}
				if ev.Cost.IsZero() || !ev.CostKnown {
					t.Errorf("event cost %s (known %v), want b's exact cost", ev.Cost, ev.CostKnown)
				}
				want += ev.Cost.MicroUSDCeil()
			} else {
				if res.err == nil || res.err.Status != 502 || ev.Deployment != "m@a" || !ev.Cost.IsZero() || !ev.CostKnown {
					t.Fatalf("error %+v, event %+v: want a 502 recorded against m@a with no served cost", res.err, ev)
				}
			}
			if ev.OverheadCost.String() != "0.0002" {
				t.Errorf("event overhead %s, want the billed Gemini attempt's 0.0002", ev.OverheadCost)
			}
			if got := testutil.ToFloat64(e.g.metrics.cost.WithLabelValues("a", "m")); got != 0.0002 {
				t.Errorf("cost_usd_total{a} = %v, want 0.0002", got)
			}
			scope := e.g.current().scopes[principal{"t1", "k1"}][0]
			if spent, _ := e.store.Spent(context.Background(), scope, e.now); spent != want {
				t.Errorf("budget spent %d µUSD, want %d (every billed attempt)", spent, want)
			}
			if n := len(f.Requests()); n != 1 {
				t.Errorf("Gemini got %d attempts, want 1", n)
			}
		})
	}
}

// TestGeminiBilledFailureAfterCommit checks the reviewer's scenario: a
// streamed answer whose first event commits the response and whose last
// ends with MALFORMED_FUNCTION_CALL. The client gets an error event; the
// ledger and the budget charge the final usage Google billed (100 prompt,
// 650 completion tokens at 1 / 2: 1,400 µUSD), once, as the served
// deployment's, known rather than estimated.
func TestGeminiBilledFailureAfterCommit(t *testing.T) {
	day := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "10")}}
	e := newAcctEnv(t, day, nil, func(c *config.Config) {
		c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
		c.Models[0].Deployments = c.Models[0].Deployments[1:2] // a only
		c.Models[0].Deployments[0].Pricing = config.Pricing{Input: mustPrice(t, "1"), Output: mustPrice(t, "2")}
	})
	f := useGemini(t, e.execEnv)
	// The fake streams one event per word, cumulative usage on each, and
	// the finish reason on the last: the first event commits the response.
	f.Script(fakegemini.Behavior{FinishReason: "MALFORMED_FUNCTION_CALL", Usage: &fakegemini.Usage{Prompt: 100, Candidates: 650}})
	res := e.do(EndpointChatCompletions, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if res.status != 200 || len(res.events) < 3 || !strings.Contains(res.events[len(res.events)-1], `"error"`) {
		t.Fatalf("status %d, events %v: want a committed stream ending in an error event", res.status, res.events)
	}
	ev := e.sink.only(t)
	want := usage.Tokens{Input: 100, Output: 650}
	if ev.Tokens != want || !ev.UsageKnown || ev.UsageEstimated || ev.Cost.String() != "0.0014" || !ev.CostKnown || !ev.OverheadCost.IsZero() {
		t.Errorf("event tokens %+v known %v estimated %v cost %s overhead %s; want %+v, known, 0.0014, no overhead",
			ev.Tokens, ev.UsageKnown, ev.UsageEstimated, ev.Cost, ev.OverheadCost, want)
	}
	scope := e.g.current().scopes[principal{"t1", "k1"}][0]
	if spent, _ := e.store.Spent(context.Background(), scope, e.now); spent != 1400 {
		t.Errorf("budget spent %d µUSD, want 1400", spent)
	}
	if got := testutil.ToFloat64(e.g.metrics.cost.WithLabelValues("a", "m")); got != 0.0014 {
		t.Errorf("cost_usd_total{a} = %v, want 0.0014 (charged once)", got)
	}
}

// settleFailStore admits as a memory store does but fails every
// settlement.
type settleFailStore struct{ budget.Store }

func (settleFailStore) Settle(context.Context, budget.Admission, int64) error {
	return budget.ErrUnavailable
}

// TestBilledOverheadNotCacheOverhead checks that a billed attempt's cost,
// which is overhead in the ledger, is not counted as uncharged cache
// overhead when the budget settlement fails: that counter is the
// semantic cache's alone.
func TestBilledOverheadNotCacheOverhead(t *testing.T) {
	day := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "10")}}
	e := newAcctEnv(t, day, settleFailStore{budget.NewMemoryStore()}, func(c *config.Config) {
		c.Providers[0] = config.Provider{ID: "a", Type: "gemini", BaseURL: "http://127.0.0.1:1/v1beta"}
		c.Models[0].Deployments = c.Models[0].Deployments[1:3]
		c.Models[0].Deployments[0].Pricing = config.Pricing{Input: mustPrice(t, "1"), Output: mustPrice(t, "2")}
		c.Models[0].Deployments[1].Capabilities = nil
	})
	f := useGemini(t, e.execEnv)
	f.Script(fakegemini.Behavior{FinishReason: "MALFORMED_FUNCTION_CALL", Usage: &fakegemini.Usage{Prompt: 100, Candidates: 50}})
	if res := e.do(EndpointChatCompletions, chatBody, nil); res.err != nil || res.info.Provider != "b" {
		t.Fatalf("error %+v, provider %q", res.err, res.info.Provider)
	}
	if ev := e.sink.only(t); ev.OverheadCost.IsZero() {
		t.Fatalf("event overhead %s, want the billed attempt's", ev.OverheadCost)
	}
	if got := testutil.ToFloat64(e.g.metrics.overheadUncharged); got != 0 {
		t.Errorf("cache_overhead_uncharged_total = %v, want 0: no cache overhead", got)
	}
	if got := testutil.ToFloat64(e.g.metrics.budgetStoreErrors); got != 1 {
		t.Errorf("budget_store_errors_total = %v, want 1", got)
	}
}
