package gateway

import (
	"context"
	"maps"
	"slices"

	"switchyard/internal/auth"
	"switchyard/internal/config"
)

// ListModels serves GET /v1/models: every catalog model (chat and
// embedding) in config order, then the virtual models the caller may use,
// then every alias in name order. The virtual models are "auto" and
// "auto:<profile>" for each profile the caller's policy lets it select;
// a policy without a baseline_model allows none. The caller is the
// principal the auth middleware stored in ctx, or the default policy's
// caller without one.
func (g *snapshot) ListModels(ctx context.Context) []Model {
	models := g.catalog.Models()
	aliases := g.catalog.Aliases()
	var virtual []string
	tenant, keyID := config.Tenant{}, ""
	if p, ok := auth.FromContext(ctx); ok {
		tenant, keyID = g.tenants[p.TenantID], p.KeyID
	}
	if profiles, err := g.policy.SelectableProfiles(tenant, keyID); err == nil && len(profiles) > 0 {
		virtual = append(virtual, "auto")
		for _, p := range profiles {
			virtual = append(virtual, "auto:"+p)
		}
	}
	out := make([]Model, 0, len(models)+len(virtual)+len(aliases))
	for _, m := range models {
		out = append(out, Model{ID: m.ID, Created: g.created})
	}
	for _, id := range virtual {
		out = append(out, Model{ID: id, Created: g.created})
	}
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		out = append(out, Model{ID: name, Created: g.created})
	}
	return out
}
