package gateway

import "slices"

// ListModels serves GET /v1/models: every catalog model, in config order.
func (g *Gateway) ListModels() []Model {
	return slices.Clone(g.models)
}
