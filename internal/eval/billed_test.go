package eval

import (
	"context"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// failing is an adapter whose calls fail with err.
type failing struct{ err error }

func (f failing) Chat(context.Context, *providers.Request) (*providers.Response, error) {
	return nil, f.err
}

func (f failing) ChatStream(context.Context, *providers.Request) (providers.Stream, error) {
	return nil, f.err
}

// TestCallChargesBilledFailure: an answer the upstream billed but the
// adapter failed (Gemini's MALFORMED_FUNCTION_CALL, an unsupported kind)
// is charged to the cap gate at its billed usage, or its estimate without
// usage, although its kind alone says it was not billed.
func TestCallChargesBilledFailure(t *testing.T) {
	price := func(s string) config.Price {
		p, err := config.ParsePrice(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	d := &catalog.Deployment{
		UpstreamModel: "up", Model: &catalog.Model{ID: "m", Type: config.ModelTypeChat},
		Prices: config.Pricing{Input: price("1"), Output: price("2")},
	}
	req, err := schema.DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	est, _ := usage.ParseMoney("0.5")
	// 1,000 prompt and 100 completion tokens at 1 / 2 per 1M: 0.0012.
	u := &schema.Usage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	for _, tt := range []struct {
		name string
		err  *providers.Error
		want string
	}{
		{"with usage", &providers.Error{Kind: providers.KindUnsupported, Billed: true, BilledUsage: u}, "0.0012"},
		{"without usage", &providers.Error{Kind: providers.KindUnsupported, Billed: true}, "0.5"},
		{"not billed", &providers.Error{Kind: providers.KindUnsupported}, "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &runner{o: &Options{Timeout: time.Second}, gate: newGate(usage.Money{})}
			_, charged, err := r.call(context.Background(), Target{Deployment: d, Adapter: failing{tt.err}}, req, est)
			if err == nil || charged.String() != tt.want {
				t.Errorf("charged %s, error %v; want %s", charged, err, tt.want)
			}
			if spent, _ := r.gate.totals(); spent.String() != tt.want {
				t.Errorf("gate spent %s, want %s", spent, tt.want)
			}
		})
	}
}
