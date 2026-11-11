package quality

import (
	"strings"
	"testing"

	"switchyard/internal/schema"
)

// benchPrompt returns a realistic request of about 4 KB of user text: a
// debugging request with prose, a stack trace and a code block, plus a
// short system prompt.
func benchPrompt(tb testing.TB) *schema.ChatRequest {
	tb.Helper()
	var b strings.Builder
	b.WriteString("Our checkout service started failing after the last deploy. Please analyze the root cause step by step, ")
	b.WriteString("and propose a fix that must keep the public API unchanged and should not add a global lock.\n\n")
	b.WriteString("- the error happens only under load\n- it never reproduces locally\n- we run 12 replicas\n\n```\n")
	b.WriteString("panic: runtime error: invalid memory address or nil pointer dereference\n\ngoroutine 4182 [running]:\n")
	for i := 0; i < 12; i++ {
		b.WriteString("checkout/internal/cart.(*Service).Apply(0xc000a1e000, {0xc0004f2a80, 0x24})\n\t/src/internal/cart/service.go:142 +0x1f4\n")
	}
	b.WriteString("```\n\n```go\n")
	for b.Len() < 3900 {
		b.WriteString("func (s *Service) Apply(ctx context.Context, id string) error {\n\tc, err := s.store.Get(ctx, id)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"get cart: %w\", err)\n\t}\n\treturn s.pricing.Apply(c)\n}\n")
	}
	b.WriteString("```\n")
	return chat(tb, map[string]any{"messages": []any{
		msg("system", "You are a senior Go engineer. Be precise."),
		msg("user", b.String()),
	}})
}

// BenchmarkAssess measures Assess on a 4 KB prompt (target: under 50 µs,
// with at most 2 allocations).
func BenchmarkAssess(b *testing.B) {
	h := newDefault(b)
	req := benchPrompt(b)
	if n := len(req.Messages()[1].Content); n < 4000 || n > 5000 {
		b.Fatalf("benchmark prompt is %d bytes, want about 4 KB", n)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		h.Assess(req, Hints{})
	}
}

// BenchmarkAssessShort measures Assess on a one-line question.
func BenchmarkAssessShort(b *testing.B) {
	h := newDefault(b)
	req := user(b, "What is the capital of Australia?")
	b.ReportAllocs()
	for b.Loop() {
		h.Assess(req, Hints{})
	}
}

// TestAssessAllocations guards the allocation budget: the only allocation
// of note is the Signals slice.
func TestAssessAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	h := newDefault(t)
	req := benchPrompt(t)
	h.Assess(req, Hints{}) // warm the pool
	if n := testing.AllocsPerRun(200, func() { h.Assess(req, Hints{}) }); n > 2 {
		t.Errorf("Assess allocates %.1f times per call, want at most 2", n)
	}
}
