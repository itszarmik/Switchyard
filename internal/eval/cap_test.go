package eval_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/judge"
	"switchyard/internal/usage"
	"switchyard/test/eval/fakejudge"
)

// billed prices what the fake received at its models' catalog prices:
// each request at the input and output tokens bill says the provider
// could bill for it.
func billed(t *testing.T, env *testEnv, bill func(fakejudge.Request) (in, out int64)) usage.Money {
	t.Helper()
	prices := map[string]config.Pricing{}
	for _, m := range env.cat.Models() {
		prices[m.Deployments[0].UpstreamModel] = m.Deployments[0].Prices
	}
	var total usage.Money
	for _, r := range env.srv.Requests() {
		in, out := bill(r)
		c, ok := usage.Cost(usage.Tokens{Input: in, Output: out}, prices[r.Model])
		if !ok {
			t.Fatalf("no price for %s", r.Model)
		}
		total = total.Add(c)
	}
	return total
}

// TestRunJudgeTimeoutCharged: a judge call that times out reached the
// upstream, which may bill it, so it is charged its estimate; the run
// never records less than the provider could bill, and stays within the
// cap counting every call received (security C1, correctness M3b).
func TestRunJudgeTimeoutCharged(t *testing.T) {
	script := func(r fakejudge.Request) fakejudge.Reply {
		rep := standardScript(r)
		if r.Judge {
			rep.Delay = 2 * time.Second // the judge is slow
		}
		return rep
	}
	env := newEnv(t, script, prompts(12), "small", "mid")
	adapters, err := eval.Adapters(env.cfg)
	if err != nil {
		t.Fatal(err)
	}
	jt, err := eval.NewTarget(env.cat, adapters, "judge")
	if err != nil {
		t.Fatal(err)
	}
	j, err := judge.NewLLM(judge.Options{Adapter: jt.Adapter, Deployment: jt.Deployment, MaxTokens: 64, Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	env.opt.Judge = j
	env.opt.MaxCost = money(t, "0.05")
	env.opt.Concurrency = 4
	sum, res, _ := run(t, env)
	if sum.JudgeFailures == 0 || sum.Judged != 0 {
		t.Fatalf("summary %+v; want judge failures only", sum)
	}
	var judgeCharged usage.Money
	for _, r := range res {
		if strings.HasPrefix(r.Error, "judge:") && r.JudgeCostUSD.IsZero() {
			t.Errorf("%s/%s: a timed-out judge call charged nothing", r.ID, r.Model)
		}
		judgeCharged = judgeCharged.Add(r.JudgeCostUSD)
	}
	// What the provider could bill for every request it received: for a
	// judge call, 1 token per 2 bytes of its body (the fake reports 1 per
	// 4) and the output cap.
	couldBill := billed(t, env, func(r fakejudge.Request) (int64, int64) {
		if r.Judge {
			return int64(len(r.Body) / 2), 64
		}
		return int64(max(1, len(r.Body)/4)), int64(max(1, len(goodAnswer)/4))
	})
	if sum.Spent.Cmp(couldBill) < 0 {
		t.Errorf("spent %s, less than the %s the provider could bill for the calls it received", sum.Spent, couldBill)
	}
	if sum.Spent.Cmp(env.opt.MaxCost) > 0 || couldBill.Cmp(env.opt.MaxCost) > 0 {
		t.Errorf("spent %s, billable %s; over the cap %s", sum.Spent, couldBill, env.opt.MaxCost)
	}
	t.Logf("summary %+v; judge charged %s", sum, judgeCharged)
}

// TestRunImageEstimate: images count at 1,600 tokens each in the
// estimates, as in the gateway, so a vision prompt set whose upstream
// bills that much per image stays within the cap, and the dry run's
// figure covers what the run spends (security H1, correctness M3a).
func TestRunImageEstimate(t *testing.T) {
	const images = 4
	script := func(r fakejudge.Request) fakejudge.Reply {
		if r.Judge {
			return fakejudge.Reply{Content: fakejudge.Verdict("tie")}
		}
		// What a provider bills for the images, plus the text.
		return fakejudge.Reply{Content: goodAnswer, PromptTokens: images*1600 + len(r.Body)/4, CompletionTokens: 128}
	}
	var ps []eval.Prompt
	for i := range 8 {
		parts := []any{map[string]any{"type": "text", "text": "describe these"}}
		for k := range images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": fmt.Sprintf("https://example.com/%d-%d.png", i, k)}})
		}
		body, _ := json.Marshal(map[string]any{"model": "x", "messages": []any{map[string]any{"role": "user", "content": parts}}})
		ps = append(ps, eval.Prompt{ID: fmt.Sprintf("img%d", i), Request: body, Category: "general"})
	}
	env := newEnv(t, script, ps, "small")
	env.opt.MaxCost = money(t, "0.3")
	env.opt.Concurrency = 4
	plan, err := eval.PlanRun(env.opt)
	if err != nil {
		t.Fatal(err)
	}
	sum, _, _ := run(t, env)
	if sum.Spent.Cmp(env.opt.MaxCost) > 0 || sum.Judged == 0 || !sum.Stopped {
		t.Errorf("summary %+v: want some pairs judged, then a stop within the cap %s", sum, env.opt.MaxCost)
	}
	// The plan's answers alone cost at least the images the upstream
	// bills: 8 prompts × 2 answers × 6,400 tokens at $1 or $10 per M.
	if floor := money(t, "0.3520"); plan.Expected.Cmp(floor) < 0 {
		t.Errorf("the dry run estimates %s, below the %s the images alone cost", plan.Expected, floor)
	}
	t.Logf("plan expected %s worst %s; summary %+v", plan.Expected, plan.Worst, sum)
}

// TestRunCapStopKeepsPaidAnswers: an answer is admitted with its judge
// calls, and the reference with the prompt's first pair, so a cap stop
// never discards an answer that was paid for: every answer the upstream
// gave is on a written line (correctness L5).
func TestRunCapStopKeepsPaidAnswers(t *testing.T) {
	slow := func(r fakejudge.Request) fakejudge.Reply {
		rep := standardScript(r)
		rep.Delay = 2 * time.Millisecond
		return rep
	}
	for _, cap := range []string{"0.02", "0.035", "0.05"} {
		t.Run(cap, func(t *testing.T) {
			env := newEnv(t, slow, prompts(30), "small", "mid")
			env.opt.Concurrency = 6
			env.opt.MaxCost = money(t, cap)
			sum, res, _ := run(t, env)
			if !sum.Stopped {
				t.Fatalf("summary %+v; want the cap to stop the run", sum)
			}
			answers := map[string]int{}
			for _, r := range env.srv.Requests() {
				if !r.Judge {
					answers[r.Model]++
				}
			}
			written := map[string]int{}
			for _, r := range res {
				if !r.CandidateCostUSD.IsZero() {
					written[r.Model+"-up"]++
				}
				if !r.ReferenceCostUSD.IsZero() {
					written["large-up"]++
				}
			}
			for m, n := range answers {
				if written[m] != n {
					t.Errorf("%s answered %d times, %d answers written", m, n, written[m])
				}
			}
			if !eval.Spent(res).Equal(sum.Spent) {
				t.Errorf("the lines record %s, the run spent %s", eval.Spent(res), sum.Spent)
			}
		})
	}
}

// TestDryRunMatchesRun: the dry run's admitted figure uses the run's own
// judge-call count and output cap, so a cap of that figure never stops
// the run (correctness L4).
func TestDryRunMatchesRun(t *testing.T) {
	env := newEnv(t, standardScript, prompts(6), "small", "mid")
	plan, err := eval.PlanRun(env.opt)
	if err != nil {
		t.Fatal(err)
	}
	env.opt.MaxCost = plan.Worst
	env.opt.Concurrency = 6
	sum, _, _ := run(t, env)
	if sum.Stopped || sum.Judged != sum.Pairs {
		t.Errorf("with the dry run's %s as the cap: %+v; want every pair judged", plan.Worst, sum)
	}
}
