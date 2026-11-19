package gateway

import (
	"context"
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
	// BudgetStore is the budget store's health, when it is a shared store
	// that can fail (Options.BudgetStoreCheck).
	BudgetStore *budgetStoreStatus `json:"budget_store,omitempty"`
}

// budgetStoreStatus is the budget store's health in the provider report.
type budgetStoreStatus struct {
	// Status is "ok" when the store answered, else "unavailable".
	Status string `json:"status"`
	// FailMode is budgets.fail_mode: what requests get while the store is
	// unavailable.
	FailMode string `json:"fail_mode"`
}

// budgetCheckTimeout bounds the budget store check of the provider report.
const budgetCheckTimeout = time.Second

// serveProviders serves GET /admin/v1/providers (see ProvidersHandler).
func (g *snapshot) serveProviders(w http.ResponseWriter, r *http.Request) {
	report := providersReport{GeneratedAt: time.Now().UTC(), Providers: g.health.Snapshot().Providers}
	if g.budgetCheck != nil {
		ctx, cancel := context.WithTimeout(r.Context(), budgetCheckTimeout)
		st := &budgetStoreStatus{Status: "ok", FailMode: g.failMode}
		if g.budgetCheck(ctx) != nil {
			st.Status = "unavailable"
		}
		cancel()
		report.BudgetStore = st
	}
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
}
