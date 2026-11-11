package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/health"
	"switchyard/internal/reqctx"
)

// providersReport is the body of GET /admin/v1/providers.
type providersReport struct {
	// GeneratedAt is when the snapshot was taken.
	GeneratedAt time.Time `json:"generated_at"`
	// Providers lists every provider and its deployments, sorted by id.
	Providers []health.ProviderSnapshot `json:"providers"`
}

// ProvidersHandler serves GET /admin/v1/providers: for every provider its
// in-flight count and max_concurrency, and for each of its deployments the
// breaker state and the time of its next transition, the rate-limit
// cooldown, the recent request and failure counts, the retry budget and
// the latency statistics. It shows no secrets. It belongs on the admin
// listener.
func (g *Gateway) ProvidersHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		report := providersReport{GeneratedAt: time.Now().UTC(), Providers: g.health.Snapshot().Providers}
		data, err := json.Marshal(report)
		if err != nil {
			ctx := r.Context()
			reqctx.Logger(ctx).ErrorContext(ctx, "encode the provider report", "error", err)
			apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error", "Could not encode the provider report.")
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
}
