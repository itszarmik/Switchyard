package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/reqctx"
)

// Built-in route patterns.
const (
	healthzPattern  = "GET /healthz"
	readyzPattern   = "GET /readyz"
	metricsPattern  = "GET /metrics"
	catchAllPattern = "/"
)

// defaultCheckTimeout bounds each readiness check.
const defaultCheckTimeout = 2 * time.Second

// readinessCheck is a named dependency check run by /readyz.
type readinessCheck struct {
	name  string
	check func(ctx context.Context) error
}

// notReadyResponse is the /readyz body when the server is not ready.
type notReadyResponse struct {
	Status       string   `json:"status"`
	Reason       string   `json:"reason,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

// handleHealthz reports liveness: the process is up and serving.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

// handleReadyz reports readiness: not shutting down, and every readiness
// check passing.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeJSON(w, http.StatusServiceUnavailable, notReadyResponse{Status: "not_ready", Reason: "shutting_down"})
		return
	}
	s.mu.Lock()
	checks := s.checks
	s.mu.Unlock()

	if failed := s.failedChecks(r.Context(), checks); len(failed) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, notReadyResponse{Status: "not_ready", FailedChecks: failed})
		return
	}
	// ConfigVersion is always sent, even when empty.
	writeJSON(w, http.StatusOK, struct {
		Status        string `json:"status"`
		ConfigVersion string `json:"config_version"`
	}{Status: "ready", ConfigVersion: s.opts.ConfigVersion})
}

// failedChecks runs the checks concurrently and returns the names of those
// that failed, in registration order.
func (s *Server) failedChecks(ctx context.Context, checks []readinessCheck) []string {
	if len(checks) == 0 {
		return nil
	}
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Go(func() { errs[i] = s.runCheck(ctx, c) })
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, checks[i].name)
			reqctx.Logger(ctx).WarnContext(ctx, "readiness check failed", "check", checks[i].name, "error", err)
		}
	}
	return failed
}

// runCheck runs one check with its own timeout. A check that ignores its
// context is abandoned when the timeout expires, and a panicking check
// counts as failed.
func (s *Server) runCheck(ctx context.Context, c readinessCheck) error {
	ctx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("check panicked: %v", p)
			}
		}()
		done <- c.check(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("check did not finish: %w", ctx.Err())
	}
}

// handleNotFound answers requests that match no route, with OpenAI's wording.
func handleNotFound(w http.ResponseWriter, r *http.Request) {
	apierror.Write(w, http.StatusNotFound, apierror.TypeInvalidRequest, "",
		fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path))
}

// writeJSON sends v as an uncacheable JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error", "encode response")
		return
	}
	data = append(data, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
