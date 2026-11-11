package gateway

import (
	"maps"
	"slices"
)

// ListModels serves GET /v1/models: every catalog model (chat and
// embedding) in config order, then every alias in name order.
func (g *Gateway) ListModels() []Model {
	models := g.catalog.Models()
	aliases := g.catalog.Aliases()
	out := make([]Model, 0, len(models)+len(aliases))
	for _, m := range models {
		out = append(out, Model{ID: m.ID, Created: g.created})
	}
	for _, name := range slices.Sorted(maps.Keys(aliases)) {
		out = append(out, Model{ID: name, Created: g.created})
	}
	return out
}
