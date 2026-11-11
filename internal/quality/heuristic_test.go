package quality

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/schema"
)

// chat builds a decoded request from a body.
func chat(t testing.TB, body map[string]any) *schema.ChatRequest {
	t.Helper()
	if _, ok := body["model"]; !ok {
		body["model"] = "m"
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := schema.DecodeChatRequest(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return req
}

// user builds a request with one user message.
func user(t testing.TB, content string) *schema.ChatRequest {
	t.Helper()
	return chat(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}})
}

func msg(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func fnTool(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}

func newDefault(t testing.TB) *Heuristic {
	t.Helper()
	h, err := NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNewHeuristic(t *testing.T) {
	tests := []struct {
		name    string
		weights map[string]float64
		wantErr string
		check   func(t *testing.T, h *Heuristic)
	}{
		{name: "nil means defaults", weights: nil, check: func(t *testing.T, h *Heuristic) {
			if h.weights != defaultWeights {
				t.Errorf("weights = %v, want defaults", h.weights)
			}
		}},
		{name: "empty means defaults", weights: map[string]float64{}, check: func(t *testing.T, h *Heuristic) {
			if h.weights != defaultWeights {
				t.Errorf("weights = %v, want defaults", h.weights)
			}
		}},
		{name: "override one", weights: map[string]float64{SignalCode: 0.9, WeightBias: 0}, check: func(t *testing.T, h *Heuristic) {
			want := defaultWeights
			want[idxCode], want[idxBias] = 0.9, 0
			if h.weights != want {
				t.Errorf("weights = %v, want %v", h.weights, want)
			}
		}},
		{name: "bounds are inclusive", weights: map[string]float64{SignalLength: -1, SignalMath: 1}},
		{name: "unknown name", weights: map[string]float64{"difficulty": 0.1}, wantErr: `unknown heuristic weight "difficulty"`},
		{name: "above range", weights: map[string]float64{SignalCode: 1.5}, wantErr: "must be in [-1, 1]"},
		{name: "below range", weights: map[string]float64{SignalCode: -1.01}, wantErr: "must be in [-1, 1]"},
		{name: "NaN", weights: map[string]float64{SignalCode: math.NaN()}, wantErr: "must be in [-1, 1]"},
		{name: "infinity", weights: map[string]float64{SignalCode: math.Inf(1)}, wantErr: "must be in [-1, 1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := NewHeuristic(tt.weights)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.check != nil {
				tt.check(t, h)
			}
		})
	}
}

func TestSignalNamesAndDefaults(t *testing.T) {
	names := SignalNames()
	want := []string{
		"length", "code", "math", "reasoning_cues", "constraints", "conversation_depth",
		"tools", "strict_schema", "simple_task", "creative", "bias",
	}
	if diff := cmp.Diff(want, names); diff != "" {
		t.Errorf("SignalNames (-want +got):\n%s", diff)
	}
	names[0] = "mutated"
	if SignalNames()[0] != "length" {
		t.Error("SignalNames returned shared storage")
	}

	w := DefaultWeights()
	if len(w) != len(want) {
		t.Errorf("DefaultWeights has %d entries, want %d", len(w), len(want))
	}
	for _, n := range want {
		v, ok := w[n]
		if !ok {
			t.Errorf("DefaultWeights lacks %q", n)
		}
		if v < -1 || v > 1 {
			t.Errorf("default weight %s = %v, outside [-1, 1]", n, v)
		}
	}
	if w[SignalSimpleTask] >= 0 {
		t.Errorf("simple_task default weight = %v, want negative", w[SignalSimpleTask])
	}
	w[SignalCode] = 99
	if DefaultWeights()[SignalCode] == 99 {
		t.Error("DefaultWeights returned shared storage")
	}
	// The defaults round-trip through NewHeuristic.
	if _, err := NewHeuristic(DefaultWeights()); err != nil {
		t.Errorf("NewHeuristic(DefaultWeights()): %v", err)
	}
}

func TestVocabularyFitsCueBits(t *testing.T) {
	// Every word and phrase gets its own id, so de-duplication within a
	// request is exact only while they fit in cueBits.
	n := len(phrases)
	for _, group := range vocabulary {
		n += len(group.words)
		for _, w := range group.words {
			if w != strings.ToLower(w) || strings.ContainsAny(w, " \t\n") {
				t.Errorf("vocabulary word %q must be one lowercase word", w)
			}
		}
	}
	if n > cueBits {
		t.Errorf("%d vocabulary entries, more than cueBits = %d", n, cueBits)
	}
	for _, p := range phrases {
		if p.text != strings.ToLower(p.text) {
			t.Errorf("phrase %q must be lowercase", p.text)
		}
	}
}

// TestSignals checks that each signal fires on a request that should
// trigger it, with the expected category, and stays quiet on a plain one.
func TestSignals(t *testing.T) {
	longProse := strings.Repeat("The quarterly report covers revenue, hiring and the office move. ", 120)
	stackTrace := "Why does this crash?\n\n```\nTraceback (most recent call last):\n  File \"app.py\", line 3, in <module>\n    main()\nZeroDivisionError: division by zero\n```"
	goCode := "Review this:\n\n```go\nfunc add(a, b int) int {\n\treturn a + b\n}\n```"
	tests := []struct {
		name     string
		req      func(t *testing.T) *schema.ChatRequest
		signal   string
		category string
	}{
		{"length", func(t *testing.T) *schema.ChatRequest { return user(t, longProse) }, SignalLength, CategoryGeneral},
		{"code fence", func(t *testing.T) *schema.ChatRequest { return user(t, goCode) }, SignalCode, CategoryCoding},
		{"stack trace", func(t *testing.T) *schema.ChatRequest { return user(t, stackTrace) }, SignalCode, CategoryCoding},
		{"code vocabulary", func(t *testing.T) *schema.ChatRequest {
			return user(t, "How do I parse JSON in Python and write a unit test for it?")
		}, SignalCode, CategoryCoding},
		{"proof", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Prove that the square root of 2 is irrational.")
		}, SignalMath, CategoryReasoning},
		{"latex", func(t *testing.T) *schema.ChatRequest {
			return user(t, `Evaluate $\int_0^1 \frac{x}{\sqrt{1+x^2}} dx$ for me.`)
		}, SignalMath, CategoryReasoning},
		{"reasoning cues", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Walk me through it step by step and discuss the trade-offs of each design.")
		}, SignalReasoningCues, CategoryGeneral},
		{"constraints", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Plan the offsite.\n- must be under $5k\n- at least 20 people\n- never on a Friday\n1. book venue\n2. book food")
		}, SignalConstraints, CategoryGeneral},
		{"conversation depth", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{
				msg("user", "hi"), msg("assistant", "hello"), msg("user", "more"), msg("assistant", "sure"), msg("user", "and?"),
			}})
		}, SignalConversationDepth, CategoryGeneral},
		{"tools", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("user", "Book it.")}, "tools": []any{fnTool("book")}})
		}, SignalTools, CategoryToolUse},
		{"deprecated functions", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("user", "Book it.")}, "functions": []any{map[string]any{"name": "book"}}})
		}, SignalTools, CategoryToolUse},
		{"strict schema", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{
				"messages":        []any{msg("user", "Acme Corp, invoice 12, total 30 dollars, due in May of next year.")},
				"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "schema": map[string]any{"type": "object"}}},
			})
		}, SignalStrictSchema, CategoryExtraction},
		{"simple task cue", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Translate the following paragraph into German, keeping the tone of the original letter intact for the reader.")
		}, SignalSimpleTask, CategoryGeneral},
		{"extraction cue", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Classify this ticket: the app crashes on login.")
		}, SignalSimpleTask, CategoryExtraction},
		{"short input", func(t *testing.T) *schema.ChatRequest { return user(t, "Capital of Peru?") }, SignalSimpleTask, CategoryGeneral},
		{"creative", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Write a short poem and a story about the sea.")
		}, SignalCreative, CategoryCreative},
	}
	h := newDefault(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := h.Assess(tt.req(t), Hints{})
			if !slices.Contains(a.Signals, tt.signal) {
				t.Errorf("signals = %v, want %s among them", a.Signals, tt.signal)
			}
			if a.Category != tt.category {
				t.Errorf("category = %s, want %s (signals %v)", a.Category, tt.category, a.Signals)
			}
			if a.Hinted {
				t.Error("Hinted without hints")
			}
		})
	}
}

// TestSignalsQuiet checks signals that must not fire.
func TestSignalsQuiet(t *testing.T) {
	h := newDefault(t)
	tests := []struct {
		name   string
		req    func(t *testing.T) *schema.ChatRequest
		absent []string
	}{
		{"plain prose", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Tell me something interesting about the history of the bicycle, how it spread across Europe in the nineteenth century, and how it changed cities.")
		}, []string{SignalLength, SignalCode, SignalMath, SignalTools, SignalStrictSchema, SignalConversationDepth, SignalCreative, SignalSimpleTask}},
		{"regex escapes are not LaTeX", func(t *testing.T) *schema.ChatRequest {
			return user(t, `What does \d+\s\w mean in a pattern used by our team for logs?`)
		}, []string{SignalMath}},
		{"late cue word is not a simple task", func(t *testing.T) *schema.ChatRequest {
			return user(t, "Write an acrostic sonnet about entropy in strict iambic pentameter whose first letters spell ENTROPY, then explain the meter.")
		}, []string{SignalSimpleTask}},
		{"cue in system prompt only", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{
				msg("system", "Classify and extract whatever the user sends."),
				msg("user", "Here is our long and detailed plan for the coming year, which needs careful review by the board before the March offsite."),
			}})
		}, []string{SignalSimpleTask}},
		{"text response format", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{
				"messages":        []any{msg("user", "Tell me something interesting about the history of the bicycle.")},
				"response_format": map[string]any{"type": "text"},
			})
		}, []string{SignalStrictSchema}},
		{"empty tools", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("user", "Book it.")}, "tools": []any{}})
		}, []string{SignalTools}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := h.Assess(tt.req(t), Hints{})
			for _, s := range tt.absent {
				if slices.Contains(a.Signals, s) {
					t.Errorf("signal %s fired (signals %v)", s, a.Signals)
				}
			}
		})
	}
}

// TestFeatureOrdering checks that each signal moves difficulty the right
// way relative to a plain baseline.
func TestFeatureOrdering(t *testing.T) {
	h := newDefault(t)
	base := "Tell me about the history of the bicycle and how it changed the way people move around cities."
	d := func(req *schema.ChatRequest) float64 { return h.Assess(req, Hints{}).Difficulty }
	baseline := d(user(t, base))

	harder := map[string]*schema.ChatRequest{
		"stack trace": user(t, base+"\n```\npanic: runtime error: index out of range\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:9 +0x1d\n```"),
		"proof":       user(t, base+" Then prove that the number of gears needed grows logarithmically."),
		"reasoning":   user(t, base+" Analyze the trade-offs step by step and justify your design."),
		"constraints": user(t, base+"\n- must cite sources\n- at most 300 words\n- never use jargon\n- include a timeline"),
		"long":        user(t, strings.Repeat(base+" ", 200)),
		"tools forced": chat(t, map[string]any{
			"messages": []any{msg("user", base)}, "tools": []any{fnTool("a"), fnTool("b"), fnTool("c")}, "tool_choice": "required",
		}),
	}
	for name, req := range harder {
		if got := d(req); got <= baseline {
			t.Errorf("%s: difficulty %.3f, want above baseline %.3f", name, got, baseline)
		}
	}
	if got := d(user(t, "Translate: "+base)); got >= baseline {
		t.Errorf("simple task: difficulty %.3f, want below baseline %.3f", got, baseline)
	}

	// More tools and a forced call count for more.
	one := chat(t, map[string]any{"messages": []any{msg("user", base)}, "tools": []any{fnTool("a")}})
	forced := chat(t, map[string]any{"messages": []any{msg("user", base)}, "tools": []any{fnTool("a")}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "a"}}})
	if d(forced) <= d(one) {
		t.Errorf("forced tool choice: %.3f, want above %.3f", d(forced), d(one))
	}
}

func TestHints(t *testing.T) {
	h := newDefault(t)
	req := user(t, "Prove that there are infinitely many primes, rigorously and step by step.")
	plain := h.Assess(req, Hints{})
	if plain.Category != CategoryReasoning {
		t.Fatalf("unhinted category = %s, want reasoning", plain.Category)
	}
	tests := []struct {
		name     string
		hints    Hints
		category string
		d        float64
		hinted   bool
	}{
		{"none", Hints{}, plain.Category, plain.Difficulty, false},
		{"task", Hints{Task: CategoryCreative}, CategoryCreative, plain.Difficulty, true},
		{"unknown task is ignored", Hints{Task: "poetry"}, plain.Category, plain.Difficulty, false},
		{"difficulty", Hints{Difficulty: 0.2, HasDifficulty: true}, plain.Category, 0.2, true},
		{"zero difficulty", Hints{Difficulty: 0, HasDifficulty: true}, plain.Category, 0, true},
		{"difficulty without flag is ignored", Hints{Difficulty: 0.2}, plain.Category, plain.Difficulty, false},
		{"both", Hints{Task: CategoryGeneral, Difficulty: 0.9, HasDifficulty: true}, CategoryGeneral, 0.9, true},
		{"difficulty above 1 is clamped", Hints{Difficulty: 7, HasDifficulty: true}, plain.Category, 1, true},
		{"difficulty below 0 is clamped", Hints{Difficulty: -3, HasDifficulty: true}, plain.Category, 0, true},
		{"NaN difficulty is clamped", Hints{Difficulty: math.NaN(), HasDifficulty: true}, plain.Category, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := h.Assess(req, tt.hints)
			if a.Category != tt.category || a.Difficulty != tt.d || a.Hinted != tt.hinted {
				t.Errorf("got {%s %v hinted=%v}, want {%s %v hinted=%v}", a.Category, a.Difficulty, a.Hinted, tt.category, tt.d, tt.hinted)
			}
			// Hints never hide the signals.
			if diff := cmp.Diff(plain.Signals, a.Signals); diff != "" {
				t.Errorf("signals changed under hints (-plain +hinted):\n%s", diff)
			}
		})
	}
}

func TestClamping(t *testing.T) {
	heavy := user(t, "Prove it step by step, analyze the trade-offs and design it.\n- must\n- must\n- must\n```go\nfunc f() { panic(\"x\") }\n```\npanic: x\n\ngoroutine 1 [running]:\n\t/src/f.go:1")
	all := func(w float64) map[string]float64 {
		m := map[string]float64{}
		for _, n := range SignalNames() {
			m[n] = w
		}
		return m
	}
	tests := []struct {
		name    string
		weights map[string]float64
		req     *schema.ChatRequest
		want    float64
	}{
		{"saturates at 1", all(1), heavy, 1},
		{"floors at 0", all(-1), heavy, 0},
		{"bias alone", map[string]float64{
			SignalLength: 0, SignalCode: 0, SignalMath: 0, SignalReasoningCues: 0, SignalConstraints: 0, SignalConversationDepth: 0,
			SignalTools: 0, SignalStrictSchema: 0, SignalSimpleTask: 0, SignalCreative: 0, WeightBias: 0.42,
		}, heavy, 0.42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := NewHeuristic(tt.weights)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.Assess(tt.req, Hints{}).Difficulty; math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("difficulty = %v, want %v", got, tt.want)
			}
		})
	}
	// With the defaults, every sanity-set request stays in [0, 1].
	h := newDefault(t)
	for _, e := range loadSanity(t) {
		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			t.Fatal(err)
		}
		if d := h.Assess(req, Hints{}).Difficulty; d < 0 || d > 1 || math.IsNaN(d) {
			t.Errorf("%s: difficulty %v outside [0, 1]", e.ID, d)
		}
	}
}

func TestNilRequest(t *testing.T) {
	a := newDefault(t).Assess(nil, Hints{})
	if a.Category != CategoryGeneral || a.Difficulty != 0 || a.Signals != nil || a.Hinted {
		t.Errorf("Assess(nil) = %+v, want the zero general assessment", a)
	}
}

// TestDeterminism checks that identical requests give identical
// assessments, including when assessed concurrently through the shared
// buffer pool.
func TestDeterminism(t *testing.T) {
	h := newDefault(t)
	entries := loadSanity(t)
	reqs := make([]*schema.ChatRequest, len(entries))
	want := make([]Assessment, len(entries))
	for i, e := range entries {
		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			t.Fatal(err)
		}
		reqs[i] = req
		want[i] = h.Assess(req, Hints{})
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8*len(reqs))
	for range 8 {
		wg.Go(func() {
			for i, req := range reqs {
				if got := h.Assess(req, Hints{}); !cmp.Equal(got, want[i]) {
					errs <- entries[i].ID + ": " + cmp.Diff(want[i], got)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// A second Heuristic agrees with the first.
	h2 := newDefault(t)
	for i, req := range reqs {
		if got := h2.Assess(req, Hints{}); !cmp.Equal(got, want[i]) {
			t.Errorf("%s: second heuristic differs: %s", entries[i].ID, cmp.Diff(want[i], got))
		}
	}
}

// TestSignalsNeverCarryContent checks that Signals holds only signal names,
// in SignalNames order, whatever the request says.
func TestSignalsNeverCarryContent(t *testing.T) {
	const canary = "zq-canary-7731"
	names := SignalNames()
	reqs := []*schema.ChatRequest{
		user(t, canary+" prove step by step ```go\nfunc "+canary+"() {}\n```\npanic: "+canary),
		user(t, "length code math bias "+canary),
		chat(t, map[string]any{
			"messages":        []any{msg("system", canary), msg("user", "Translate "+canary)},
			"tools":           []any{fnTool(canary)},
			"tool_choice":     "required",
			"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": canary}},
		}),
	}
	for _, e := range loadSanity(t) {
		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, req)
	}
	h := newDefault(t)
	for i, req := range reqs {
		a := h.Assess(req, Hints{Task: CategoryCoding})
		last := -1
		for _, s := range a.Signals {
			idx := slices.Index(names, s)
			if idx < 0 || s == WeightBias {
				t.Errorf("request %d: signal %q is not a signal name", i, s)
			}
			if idx <= last {
				t.Errorf("request %d: signals %v not in SignalNames order", i, a.Signals)
			}
			last = idx
			if strings.Contains(s, canary) {
				t.Errorf("request %d: signal %q carries content", i, s)
			}
		}
		if strings.Contains(a.Category, canary) {
			t.Errorf("request %d: category carries content", i)
		}
	}
}

// TestTextReading checks what Assess reads: content strings and text
// parts, the last user message and system prompts only, and at most
// maxText bytes.
func TestTextReading(t *testing.T) {
	h := newDefault(t)
	proof := "Prove that the series diverges, rigorously."
	tests := []struct {
		name string
		req  func(t *testing.T) *schema.ChatRequest
		math bool
	}{
		{"string content", func(t *testing.T) *schema.ChatRequest { return user(t, proof) }, true},
		{"text parts", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("user", []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
				map[string]any{"type": "text", "text": proof},
			})}})
		}, true},
		{"escaped text", func(t *testing.T) *schema.ChatRequest {
			// P is P; the raw JSON spells the cue word with escapes.
			data := []byte(`{"model":"m","messages":[{"role":"user","content":"Prove that 😀 the series diverges.\nOK"}]}`)
			req, err := schema.DecodeChatRequest(data)
			if err != nil {
				t.Fatal(err)
			}
			return req
		}, true},
		{"system prompt is read", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("system", proof), msg("user", "Go ahead with the next part please.")}})
		}, true},
		{"developer prompt is read", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("developer", proof), msg("user", "Go ahead with the next part please.")}})
		}, true},
		{"earlier user turns are not read", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("user", proof), msg("assistant", "ok"), msg("user", "Thanks, that is all for today.")}})
		}, false},
		{"assistant turns are not read", func(t *testing.T) *schema.ChatRequest {
			return chat(t, map[string]any{"messages": []any{msg("assistant", proof), msg("user", "Thanks, that is all for today.")}})
		}, false},
		{"text beyond the cap is not read", func(t *testing.T) *schema.ChatRequest {
			return user(t, strings.Repeat("a", maxText)+" "+proof)
		}, false},
		{"text within the cap is read", func(t *testing.T) *schema.ChatRequest {
			return user(t, strings.Repeat("a", maxText-200)+" "+proof)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := h.Assess(tt.req(t), Hints{})
			if got := slices.Contains(a.Signals, SignalMath); got != tt.math {
				t.Errorf("math fired = %v, want %v (signals %v)", got, tt.math, a.Signals)
			}
		})
	}
}

// TestBufferCleared checks that no request text is left in the pooled
// buffer after Assess.
func TestBufferCleared(t *testing.T) {
	h := newDefault(t)
	h.Assess(user(t, "secret-ish words that must not linger in memory"), Hints{})
	bp := h.bufs.Get().(*[]byte)
	defer h.bufs.Put(bp)
	full := (*bp)[:cap(*bp)]
	for i, c := range full {
		if c != 0 {
			t.Fatalf("buffer byte %d = %q, want it zeroed", i, c)
		}
	}
}
