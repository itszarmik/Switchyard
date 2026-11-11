package scenario

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"switchyard/test/fakeanthropic"
)

// AnthropicAbortedStream streams from an Anthropic upstream that breaks
// off after about 1,000 output tokens of content, before message_delta
// reports the output: only message_start's partial usage (1,000 input
// tokens, 1 output token) has arrived. The ledger must keep the reported
// input and estimate the output from what was relayed, at 4 bytes per
// token: 1,000 × 3 + 1,000 × 15 USD per 1M tokens = 0.018 USD.
func AnthropicAbortedStream(t *testing.T, o Options) {
	text := strings.Repeat("abcd", 1000) // 4,000 bytes: 1,000 tokens
	delta, err := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	if err != nil {
		t.Fatal(err)
	}
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-up\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: " + string(delta) + "\n\n"
	claude := fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey})
	t.Cleanup(claude.Close)
	claude.Script(fakeanthropic.Behavior{Replay: []byte(sse)})
	g := start(t, o, fmt.Sprintf(`providers:
  - {id: claude, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: claude-model
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: claude, upstream_model: claude-up, pricing: {input: "3", output: "15"}}`, claude.BaseURL()), "")
	r := g.Post(t, "/v1/chat/completions", `{"model":"claude-model","max_tokens":2000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if r.Status != 200 || len(r.Events) == 0 {
		t.Fatalf("status %d, %d events", r.Status, len(r.Events))
	}
	if last := r.Events[len(r.Events)-1]; last == "[DONE]" {
		t.Errorf("a broken stream ended with [DONE]")
	}
	g.WaitReport(t, "", 1)
	resp, err := g.get(g.AdminURL + "/admin/v1/usage?tenant=" + g.Tenant)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var rep struct {
		Totals struct {
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
			} `json:"tokens"`
			CostUSD string `json:"cost_usd"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report: %v: %s", err, b)
	}
	if rep.Totals.Tokens.Input != 1000 || rep.Totals.Tokens.Output != 1000 || rep.Totals.CostUSD != "0.018" {
		t.Errorf("recorded %d input and %d output tokens costing %s for 1,000 and about 1,000 relayed; want 0.018",
			rep.Totals.Tokens.Input, rep.Totals.Tokens.Output, rep.Totals.CostUSD)
	}
}
