package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

// routingFieldsConfig is built by hand rather than loaded, so that these
// tests do not depend on validation rules that other phases change.
func routingFieldsConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{ID: "p", Type: "openai", MaxConcurrency: 4},
			{ID: "q", Type: "openai"},
		},
		Models: []config.Model{
			{
				ID: "m", Type: config.ModelTypeChat,
				Quality: map[string]float64{"general": 0.7, "coding": 0.6},
				Deployments: []config.Deployment{
					{Provider: "p", UpstreamModel: "u", QualityOffset: -0.02, LatencyHint: config.LatencyHint{TTFTMs: 900, TokensPerSec: 70}},
					{Provider: "q", UpstreamModel: "u"},
				},
			},
			{ID: "bare", Type: config.ModelTypeChat, Deployments: []config.Deployment{{Provider: "q", UpstreamModel: "b"}}},
		},
		Aliases: map[string]string{"smart": "auto:quality", "any": "auto", "plain": "m"},
	}
}

func TestRoutingFields(t *testing.T) {
	cfg := routingFieldsConfig()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := c.byID["m"]
	if diff := cmp.Diff(map[string]float64{"general": 0.7, "coding": 0.6}, m.Quality); diff != "" {
		t.Errorf("Quality (-want +got):\n%s", diff)
	}
	// The catalog keeps its own copy.
	cfg.Models[0].Quality["general"] = 0.1
	if m.Quality["general"] != 0.7 {
		t.Errorf("Quality aliases the config map")
	}
	if c.byID["bare"].Quality != nil {
		t.Errorf("bare Quality = %v, want nil", c.byID["bare"].Quality)
	}
	tests := []struct {
		id     string
		offset float64
		maxC   int
		hint   LatencyHint
	}{
		{"m@p", -0.02, 4, LatencyHint{TTFT: 900 * time.Millisecond, TokensPerSec: 70}},
		{"m@q", 0, 0, LatencyHint{}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			d := c.deployments[tt.id]
			if d.QualityOffset != tt.offset || d.ProviderMaxConcurrency != tt.maxC || d.LatencyHint != tt.hint {
				t.Errorf("%s: offset %v, max %d, hint %+v; want %v, %d, %+v",
					tt.id, d.QualityOffset, d.ProviderMaxConcurrency, d.LatencyHint, tt.offset, tt.maxC, tt.hint)
			}
		})
	}
}

func TestVirtual(t *testing.T) {
	c, err := New(routingFieldsConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name       string
		virtual    bool
		wantTarget string
		wantVirt   bool
		resolveErr error
	}{
		{name: "auto", virtual: true, wantTarget: "auto", wantVirt: true, resolveErr: ErrVirtualModel},
		{name: "auto:cheap", virtual: true, wantTarget: "auto:cheap", wantVirt: true, resolveErr: ErrVirtualModel},
		{name: "smart", wantTarget: "auto:quality", wantVirt: true, resolveErr: ErrVirtualModel},
		{name: "any", wantTarget: "auto", wantVirt: true, resolveErr: ErrVirtualModel},
		{name: "plain", wantTarget: "m"},
		{name: "m", wantTarget: "m"},
		{name: "autopilot", wantTarget: "autopilot", resolveErr: ErrVirtualModel},
		{name: "nope", wantTarget: "nope", resolveErr: ErrUnknownModel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsVirtual(tt.name); got != tt.virtual {
				t.Errorf("IsVirtual = %v, want %v", got, tt.virtual)
			}
			target, ok := c.VirtualTarget(tt.name)
			if target != tt.wantTarget || ok != tt.wantVirt {
				t.Errorf("VirtualTarget = %q, %v; want %q, %v", target, ok, tt.wantTarget, tt.wantVirt)
			}
			_, err := c.Resolve(tt.name)
			if !errors.Is(err, tt.resolveErr) {
				t.Errorf("Resolve error = %v, want %v", err, tt.resolveErr)
			}
		})
	}
}
