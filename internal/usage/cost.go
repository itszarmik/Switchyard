package usage

import (
	"math/big"
	"strings"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// Tokens counts a request's tokens by billing class (DESIGN §12.1).
type Tokens struct {
	// Input is uncached input: prompt tokens that were neither read from
	// nor written to the provider's prompt cache.
	Input int64
	// CachedInput was read from the provider's prompt cache.
	CachedInput int64
	// CacheWrite was written to the provider's prompt cache.
	CacheWrite int64
	// Output is completion tokens, including hidden reasoning tokens.
	Output int64
}

// Add returns the class-by-class sum of t and o.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{
		Input:       t.Input + o.Input,
		CachedInput: t.CachedInput + o.CachedInput,
		CacheWrite:  t.CacheWrite + o.CacheWrite,
		Output:      t.Output + o.Output,
	}
}

// FromUsage normalizes OpenAI-wire usage, which every adapter produces. On
// the wire, prompt_tokens includes cached_tokens and cache_write_tokens, so
// Input is prompt_tokens minus both, floored at 0. Output is
// completion_tokens, which already includes reasoning tokens. Negative
// counts are treated as 0. A nil u yields zero Tokens; embeddings, which
// report only prompt_tokens, yield Input = prompt_tokens.
func FromUsage(u *schema.Usage) Tokens {
	if u == nil {
		return Tokens{}
	}
	var t Tokens
	if d := u.PromptTokensDetails; d != nil {
		t.CachedInput = max(d.CachedTokens, 0)
		t.CacheWrite = max(d.CacheWriteTokens, 0)
	}
	t.Input = max(u.PromptTokens-t.CachedInput-t.CacheWrite, 0)
	t.Output = max(u.CompletionTokens, 0)
	return t
}

// EstimateTokens returns the token counts recorded when usage is missing,
// for example a stream that ended before its usage chunk: the gateway's
// input estimate, and output estimated at one token per 4 bytes of the
// output the gateway relayed, rounded up. The caller sets
// Event.UsageEstimated.
func EstimateTokens(inputEstimate, outputBytes int64) Tokens {
	return Tokens{
		Input:  max(inputEstimate, 0),
		Output: (max(outputBytes, 0) + 3) / 4,
	}
}

// Cost prices t at p for a chat (or completions) deployment. The second
// result reports whether the cost is known, which needs both the input and
// the output price. Cached input and cache writes without their own price
// are charged at the input price. An unknown cost is returned as 0. Prices
// are per million tokens and the result is exact. Negative counts are
// treated as 0.
func Cost(t Tokens, p config.Pricing) (Money, bool) {
	return cost(t, p, true)
}

// EmbeddingCost is Cost for an embedding deployment, whose cost is known
// with only the input price set. Output tokens, which embeddings do not
// produce, are charged at the output price; with output tokens and no
// output price, the cost is unknown.
func EmbeddingCost(t Tokens, p config.Pricing) (Money, bool) {
	return cost(t, p, t.Output > 0)
}

// CostFor prices t with Cost, or with EmbeddingCost when modelType is
// config.ModelTypeEmbedding.
func CostFor(modelType string, t Tokens, p config.Pricing) (Money, bool) {
	if modelType == config.ModelTypeEmbedding {
		return EmbeddingCost(t, p)
	}
	return Cost(t, p)
}

// BaselineCost prices t at each of prices, the pricing of every deployment
// of the baseline model, and returns the lowest known cost (DESIGN §12.4).
// It reports false when no deployment has a known cost.
func BaselineCost(modelType string, t Tokens, prices []config.Pricing) (Money, bool) {
	var best Money
	found := false
	for _, p := range prices {
		c, ok := CostFor(modelType, t, p)
		if ok && (!found || c.Cmp(best) < 0) {
			best, found = c, true
		}
	}
	return best, found
}

// cost implements Cost and EmbeddingCost. needOutput says whether an unset
// output price makes the cost unknown.
func cost(t Tokens, p config.Pricing, needOutput bool) (Money, bool) {
	in, ok := nanoPerMillion(p.Input)
	if !ok {
		return Money{}, false
	}
	out, outOK := nanoPerMillion(p.Output)
	if !outOK {
		if needOutput {
			return Money{}, false
		}
		out = new(big.Int)
	}
	cached, ok := nanoPerMillion(p.CachedInput)
	if !ok {
		cached = in
	}
	write, ok := nanoPerMillion(p.CacheWrite)
	if !ok {
		write = in
	}
	// A price of n nano-USD per million tokens is n×1e-9/1e6 USD = n×1e-15
	// USD = n femto-USD per token, so the sum below is exact femto-USD.
	total := new(big.Int)
	var term big.Int
	for _, c := range []struct {
		tokens int64
		price  *big.Int
	}{
		{t.Input, in},
		{t.CachedInput, cached},
		{t.CacheWrite, write},
		{t.Output, out},
	} {
		if c.tokens <= 0 {
			continue
		}
		term.Mul(big.NewInt(c.tokens), c.price)
		total.Add(total, &term)
	}
	return fromFemto(total), true
}

// nanoPerMillion converts a price in USD per million tokens to a whole
// number of nano-USD per million tokens. config.ParsePrice guarantees a
// plain non-negative decimal with at most 9 decimals. It reports false for
// an unset price.
func nanoPerMillion(p config.Price) (*big.Int, bool) {
	s := p.String()
	if s == "" {
		return nil, false
	}
	intPart, frac, _ := strings.Cut(s, ".")
	const nanoDigits = 9
	if len(frac) > nanoDigits {
		// Unreachable for a validated Price; refuse rather than round.
		return nil, false
	}
	v, ok := new(big.Int).SetString(intPart+frac+strings.Repeat("0", nanoDigits-len(frac)), 10)
	if !ok {
		return nil, false
	}
	return v, true
}
