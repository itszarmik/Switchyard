package shadow

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"switchyard/internal/schema"
)

func TestSampledDeterministic(t *testing.T) {
	ids := []string{"019a6b2e-1c3d-7e4f-8a9b-0c1d2e3f4a5b", "x", ""}
	for _, id := range ids {
		for _, rate := range []float64{0.01, 0.1, 0.5, 0.99} {
			first := Sampled(id, rate)
			for range 5 {
				if Sampled(id, rate) != first {
					t.Fatalf("Sampled(%q, %v) changed between calls", id, rate)
				}
			}
		}
	}
	for _, id := range ids {
		if Sampled(id, 0) || Sampled(id, -1) || Sampled(id, math.NaN()) {
			t.Errorf("Sampled(%q) is true at rate 0, below 0 or NaN", id)
		}
		if !Sampled(id, 1) || !Sampled(id, 2) {
			t.Errorf("Sampled(%q) is false at rate 1 or above", id)
		}
	}
	// A request sampled at a rate is sampled at every higher rate, so
	// raising the rate only adds requests.
	for i := range 2000 {
		id := fmt.Sprint("req-", i)
		if Sampled(id, 0.05) && !Sampled(id, 0.2) {
			t.Fatalf("%s is sampled at 5%% but not at 20%%", id)
		}
	}
}

func TestSampledRate(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	ids := make([]string, 10_000)
	for i := range ids {
		// UUIDv7-shaped: a shared timestamp prefix and random bits.
		ids[i] = fmt.Sprintf("019a6b2e-%04x-7%03x-%04x-%012x", i>>4, rng.IntN(1<<12), 0x8000|rng.IntN(1<<14), rng.Int64N(1<<48))
	}
	for _, rate := range []float64{0.1, 0.5} {
		n := 0
		for _, id := range ids {
			if Sampled(id, rate) {
				n++
			}
		}
		if got := float64(n) / float64(len(ids)); math.Abs(got-rate) > 0.015 {
			t.Errorf("rate %v: sampled %.4f of 10000 ids, want within ±0.015", rate, got)
		}
	}
	// Sequential IDs too.
	n := 0
	for i := range 10_000 {
		if Sampled(fmt.Sprintf("019a6b2e-0000-7000-8000-%012d", i), 0.1) {
			n++
		}
	}
	if got := float64(n) / 10_000; math.Abs(got-0.1) > 0.015 {
		t.Errorf("sequential ids: sampled %.4f, want 0.1 ± 0.015", got)
	}
}

func TestResponseText(t *testing.T) {
	tests := []struct {
		name, body, want string
		ok               bool
	}{
		{"string content", `{"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"}}]}`, "Hello", true},
		{"text parts", `{"choices":[{"index":0,"message":{"content":[{"type":"text","text":"a"},{"type":"image_url"},{"type":"text","text":"b"}]}}]}`, "ab", true},
		{"refusal", `{"choices":[{"index":0,"message":{"content":null,"refusal":"No."}}]}`, "No.", true},
		{"tool calls", `{"choices":[{"index":0,"message":{"content":"Calling.","tool_calls":[{"id":"1","type":"function","function":{"name":"get","arguments":"{\"q\":1}"}},{"function":{"name":"put","arguments":"{}"}}]}}]}`,
			"Calling.\n[tool call] get({\"q\":1})\n[tool call] put({})", true},
		{"tool call only", `{"choices":[{"index":0,"message":{"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}}]}`, "[tool call] f({})", true},
		{"choice 0 first", `{"choices":[{"index":1,"message":{"content":"one"}},{"index":0,"message":{"content":"zero"}}]}`, "zero", true},
		{"no choices", `{"choices":[]}`, "", false},
		{"not json", `oops`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResponseText([]byte(tt.body))
			if got != tt.want || ok != tt.ok {
				t.Errorf("ResponseText = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func chunk(t *testing.T, s string) *schema.ChatChunk {
	t.Helper()
	c, err := schema.ParseChatChunk([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTextBuilderMatchesResponseText: a streamed answer collects to the
// same text as the equivalent non-streaming answer.
func TestTextBuilderMatchesResponseText(t *testing.T) {
	var b TextBuilder
	for _, c := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Call"}}]}`,
		`{"choices":[{"index":1,"delta":{"content":"other choice"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ing."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"1","function":{"name":"get","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"put","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`{"error":{"message":"boom"}}`,
	} {
		b.Add(chunk(t, c))
	}
	b.Add(nil)
	want, _ := ResponseText([]byte(`{"choices":[{"index":0,"message":{"content":"Calling.","tool_calls":[{"function":{"name":"get","arguments":"{\"q\":1}"}},{"function":{"name":"put","arguments":"{}"}}]}}]}`))
	if got := b.String(); got != want || b.Truncated() {
		t.Errorf("TextBuilder = %q (truncated %v), want %q", got, b.Truncated(), want)
	}
}

func TestTextBuilderCap(t *testing.T) {
	b := TextBuilder{MaxBytes: 10}
	b.Add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"12345"}}]}`))
	b.Add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"6789"}}]}`))
	if b.Truncated() || b.String() != "123456789" {
		t.Fatalf("under the cap: %q, truncated %v", b.String(), b.Truncated())
	}
	b.Add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"ab"}}]}`))
	b.Add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"c"}}]}`))
	if !b.Truncated() || b.String() != "123456789" {
		t.Errorf("over the cap: %q, truncated %v; want the text so far and truncated", b.String(), b.Truncated())
	}
	var d TextBuilder
	d.Add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"`+strings.Repeat("x", DefaultMaxAnswerBytes+1)+`"}}]}`))
	if !d.Truncated() {
		t.Error("the default cap does not apply")
	}
	// Absurd tool-call indexes are ignored rather than allocated.
	var e TextBuilder
	e.Add(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":100000000,"function":{"name":"f"}}]}}]}`))
	if e.String() != "" {
		t.Errorf("got %q", e.String())
	}
}
