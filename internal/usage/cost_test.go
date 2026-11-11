package usage

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

func mustPrice(t testing.TB, s string) config.Price {
	t.Helper()
	if s == "" {
		return config.Price{}
	}
	p, err := config.ParsePrice(s)
	if err != nil {
		t.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}

// pricing builds a config.Pricing from input, output, cached input and
// cache write prices; "" leaves a price unset.
func pricing(t testing.TB, in, out, cached, write string) config.Pricing {
	t.Helper()
	return config.Pricing{
		Input:       mustPrice(t, in),
		Output:      mustPrice(t, out),
		CachedInput: mustPrice(t, cached),
		CacheWrite:  mustPrice(t, write),
	}
}

func TestFromUsage(t *testing.T) {
	tests := []struct {
		name string
		u    *schema.Usage
		want Tokens
	}{
		{"nil usage", nil, Tokens{}},
		{"plain chat", &schema.Usage{PromptTokens: 1234, CompletionTokens: 567, TotalTokens: 1801}, Tokens{Input: 1234, Output: 567}},
		{
			"cached tokens are part of prompt_tokens",
			&schema.Usage{PromptTokens: 2000, CompletionTokens: 10, PromptTokensDetails: &schema.PromptTokensDetails{CachedTokens: 1500}},
			Tokens{Input: 500, CachedInput: 1500, Output: 10},
		},
		{
			"cache writes are part of prompt_tokens",
			&schema.Usage{PromptTokens: 3000, CompletionTokens: 20, PromptTokensDetails: &schema.PromptTokensDetails{CacheWriteTokens: 2500}},
			Tokens{Input: 500, CacheWrite: 2500, Output: 20},
		},
		{
			"cached and cache write",
			&schema.Usage{PromptTokens: 4000, CompletionTokens: 30, PromptTokensDetails: &schema.PromptTokensDetails{CachedTokens: 1000, CacheWriteTokens: 2000}},
			Tokens{Input: 1000, CachedInput: 1000, CacheWrite: 2000, Output: 30},
		},
		{
			"reasoning tokens stay in output",
			&schema.Usage{PromptTokens: 100, CompletionTokens: 900, CompletionTokensDetails: &schema.CompletionTokensDetails{ReasoningTokens: 800}},
			Tokens{Input: 100, Output: 900},
		},
		{
			"empty details",
			&schema.Usage{PromptTokens: 100, CompletionTokens: 1, PromptTokensDetails: &schema.PromptTokensDetails{}},
			Tokens{Input: 100, Output: 1},
		},
		{
			"inconsistent details floor input at zero",
			&schema.Usage{PromptTokens: 100, PromptTokensDetails: &schema.PromptTokensDetails{CachedTokens: 80, CacheWriteTokens: 40}},
			Tokens{Input: 0, CachedInput: 80, CacheWrite: 40},
		},
		{"embeddings report prompt tokens only", &schema.Usage{PromptTokens: 42, TotalTokens: 42}, Tokens{Input: 42}},
		{
			"negative counts are zero",
			&schema.Usage{PromptTokens: -5, CompletionTokens: -1, PromptTokensDetails: &schema.PromptTokensDetails{CachedTokens: -3, CacheWriteTokens: -2}},
			Tokens{},
		},
		{"all zero", &schema.Usage{}, Tokens{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, FromUsage(tt.u)); diff != "" {
				t.Errorf("FromUsage (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in, bytes int64
		want      Tokens
	}{
		{0, 0, Tokens{}},
		{100, 1, Tokens{Input: 100, Output: 1}},
		{100, 4, Tokens{Input: 100, Output: 1}},
		{100, 5, Tokens{Input: 100, Output: 2}},
		{-1, -1, Tokens{}},
	}
	for _, tt := range tests {
		if got := EstimateTokens(tt.in, tt.bytes); got != tt.want {
			t.Errorf("EstimateTokens(%d, %d) = %+v, want %+v", tt.in, tt.bytes, got, tt.want)
		}
	}
}

func TestCost(t *testing.T) {
	tests := []struct {
		name      string
		tokens    Tokens
		p         config.Pricing
		want      string
		wantKnown bool
	}{
		{
			// 1234 × 0.15 / 1e6 + 567 × 0.6 / 1e6 = 0.0001851 + 0.0003402.
			"input and output", Tokens{Input: 1234, Output: 567}, pricing(t, "0.15", "0.6", "", ""),
			"0.0005253", true,
		},
		{
			// 1000 × 3 + 2000 × 0.3 + 500 × 3.75 + 100 × 15, per million.
			"every class priced", Tokens{Input: 1000, CachedInput: 2000, CacheWrite: 500, Output: 100},
			pricing(t, "3", "15", "0.3", "3.75"),
			"0.006975", true,
		},
		{
			// Cached input and cache writes fall back to the input price:
			// (1000 + 2000 + 500) × 2.5 + 100 × 10, per million.
			"cached and cache write fall back to input", Tokens{Input: 1000, CachedInput: 2000, CacheWrite: 500, Output: 100},
			pricing(t, "2.5", "10", "", ""),
			"0.00975", true,
		},
		{
			"cached priced, cache write falls back", Tokens{CachedInput: 1_000_000, CacheWrite: 1_000_000},
			pricing(t, "1", "2", "0.1", ""),
			"1.1", true,
		},
		{
			"smallest price", Tokens{Input: 1}, pricing(t, "0.000000001", "0", "", ""),
			"0.000000000000001", true,
		},
		{
			"nine-decimal prices stay exact", Tokens{Input: 123_456_789, CachedInput: 7, Output: 987_654_321},
			pricing(t, "0.123456789", "9.87654321", "0.000000003", ""),
			// 123456789 × 0.123456789 = 15241578.750190521
			// 7 × 0.000000003 = 0.000000021
			// 987654321 × 9.87654321 = 9754610577.89971041
			// The sum, divided by 1e6 (checked with Python's decimal module).
			"9769.852156649900952", true,
		},
		{"zero tokens", Tokens{}, pricing(t, "1", "1", "", ""), "0", true},
		{"zero prices are known", Tokens{Input: 10, Output: 10}, pricing(t, "0", "0", "", ""), "0", true},
		{"negative tokens count as zero", Tokens{Input: -10, Output: 1_000_000}, pricing(t, "1", "2", "", ""), "2", true},
		{"no input price", Tokens{Input: 10, Output: 10}, pricing(t, "", "1", "1", "1"), "0", false},
		{"no output price", Tokens{Input: 10, Output: 10}, pricing(t, "1", "", "", ""), "0", false},
		{"no output price and no output", Tokens{Input: 10}, pricing(t, "1", "", "", ""), "0", false},
		{"nothing priced", Tokens{Input: 10}, config.Pricing{}, "0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := Cost(tt.tokens, tt.p)
			if got.String() != tt.want || known != tt.wantKnown {
				t.Errorf("Cost = %s, %v; want %s, %v", got, known, tt.want, tt.wantKnown)
			}
			cf, cfKnown := CostFor(config.ModelTypeChat, tt.tokens, tt.p)
			if !cf.Equal(got) || cfKnown != known {
				t.Errorf("CostFor(chat) = %s, %v; Cost = %s, %v", cf, cfKnown, got, known)
			}
		})
	}
}

func TestEmbeddingCost(t *testing.T) {
	tests := []struct {
		name      string
		tokens    Tokens
		p         config.Pricing
		want      string
		wantKnown bool
	}{
		{"input only", Tokens{Input: 1234}, pricing(t, "0.02", "", "", ""), "0.00002468", true},
		{"output price ignored without output", Tokens{Input: 1_000_000}, pricing(t, "0.1", "5", "", ""), "0.1", true},
		{"no input price", Tokens{Input: 10}, pricing(t, "", "1", "", ""), "0", false},
		{"output tokens without a price", Tokens{Input: 10, Output: 1}, pricing(t, "1", "", "", ""), "0", false},
		{"output tokens with a price", Tokens{Input: 1_000_000, Output: 1_000_000}, pricing(t, "1", "2", "", ""), "3", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := EmbeddingCost(tt.tokens, tt.p)
			if got.String() != tt.want || known != tt.wantKnown {
				t.Errorf("EmbeddingCost = %s, %v; want %s, %v", got, known, tt.want, tt.wantKnown)
			}
			cf, cfKnown := CostFor(config.ModelTypeEmbedding, tt.tokens, tt.p)
			if !cf.Equal(got) || cfKnown != known {
				t.Errorf("CostFor(embedding) = %s, %v; EmbeddingCost = %s, %v", cf, cfKnown, got, known)
			}
		})
	}
}

func TestBaselineCost(t *testing.T) {
	tok := Tokens{Input: 1_000_000, CachedInput: 1_000_000, Output: 1_000_000}
	tests := []struct {
		name      string
		modelType string
		prices    []config.Pricing
		want      string
		wantKnown bool
	}{
		{"no deployments", config.ModelTypeChat, nil, "0", false},
		{"only unpriced", config.ModelTypeChat, []config.Pricing{pricing(t, "1", "", "", "")}, "0", false},
		{
			// 1 + 1 + 4 = 6 versus 2 + 0.1 + 3 = 5.1: the cheapest depends on
			// the token mix, not on the input price alone.
			"cheapest for these tokens", config.ModelTypeChat,
			[]config.Pricing{pricing(t, "1", "4", "", ""), pricing(t, "2", "3", "0.1", ""), pricing(t, "", "", "", "")},
			"5.1", true,
		},
		{"embedding", config.ModelTypeEmbedding, []config.Pricing{pricing(t, "0.5", "", "", ""), pricing(t, "0.25", "", "", "")}, "0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := BaselineCost(tt.modelType, tok, tt.prices)
			if got.String() != tt.want || known != tt.wantKnown {
				t.Errorf("BaselineCost = %s, %v; want %s, %v", got, known, tt.want, tt.wantKnown)
			}
		})
	}
	emb, ok := BaselineCost(config.ModelTypeEmbedding, Tokens{Input: 1_000_000},
		[]config.Pricing{pricing(t, "0.5", "", "", ""), pricing(t, "0.25", "", "", "")})
	if !ok || emb.String() != "0.25" {
		t.Errorf("embedding baseline = %s, %v; want 0.25, true", emb, ok)
	}
}
