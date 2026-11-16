package usageapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/auth"
	"switchyard/internal/reqctx"
	"switchyard/internal/usage"
)

var now = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func money(t *testing.T, s string) usage.Money {
	t.Helper()
	m, err := usage.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// seeded returns a memory ledger with events of tenants a and b.
func seeded(t *testing.T) *usage.MemoryLedger {
	t.Helper()
	l := usage.NewMemoryLedger()
	ev := func(tenant, model string, at time.Time, cost, baseline string, status int, edit ...func(*usage.Event)) {
		e := &usage.Event{
			ID: tenant + model + at.String(), Time: at, Tenant: tenant, KeyID: tenant + "-key", RequestedModel: model,
			Status: status, Cost: money(t, cost), CostKnown: true, BaselineCost: money(t, baseline),
			Tokens: usage.Tokens{Input: 10, Output: 5}, CacheStatus: usage.CacheStatusMiss,
		}
		for _, f := range edit {
			f(e)
		}
		l.Record(e)
	}
	// In observe mode, with a shadow deployment that would have cost 0.1.
	ev("a", "m1", now.Add(-time.Hour), "0.25", "0.5", 200, func(e *usage.Event) {
		e.RoutingMode, e.ShadowDeployment = usage.RoutingModeObserve, "m0@p"
		e.ShadowCost, e.ShadowCostKnown = money(t, "0.1"), true
	})
	ev("a", "m1", now.Add(-2*time.Hour), "0.000000000000001", "0.000000000000001", 200)
	ev("a", "m2", now.Add(-26*time.Hour), "1.5", "1.5", 502)
	ev("a", "m1", now.Add(-30*24*time.Hour), "100", "100", 200) // outside the default period
	ev("b", "m1", now.Add(-time.Hour), "7", "7", 200)
	return l
}

func get(t *testing.T, h http.Handler, url string, p *auth.Principal) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), *p))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body, w.Body.String()
}

func TestTenantReport(t *testing.T) {
	h := TenantHandler(seeded(t), Options{Now: func() time.Time { return now }})
	pa := &auth.Principal{TenantID: "a", KeyID: "a-key"}

	code, body, raw := get(t, h, "/switchyard/v1/usage?group_by=model", pa)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	want := map[string]any{
		"object":       "usage_report",
		"period":       map[string]any{"from": "2026-11-04T12:00:00Z", "to": "2026-11-11T12:00:00Z"},
		"tenant":       "a",
		"group_by":     []any{"model"},
		"verification": map[string]any{"min_samples": 30.0, "min_pass_rate": 0.9, "bound": "wilson_lower_90"},
		"totals": withoutShadow(map[string]any{
			"requests": 3.0, "errors": 1.0, "unpriced_requests": 0.0,
			"tokens":   map[string]any{"input": 30.0, "cached_input": 0.0, "cache_write": 0.0, "output": 15.0},
			"cost_usd": "1.750000000000001", "baseline_cost_usd": "2.000000000000001", "overhead_usd": "0",
			"savings_usd":           map[string]any{"routing": "0.25", "cache": "0", "net": "0.25"},
			"projected_savings_usd": "0.4",
			"cache_hit_rate":        0.0, "fallback_rate": 0.0,
		}),
		"groups": []any{
			withoutShadow(map[string]any{
				"model": "m2", "requests": 1.0, "errors": 1.0, "unpriced_requests": 0.0,
				"tokens":   map[string]any{"input": 10.0, "cached_input": 0.0, "cache_write": 0.0, "output": 5.0},
				"cost_usd": "1.5", "baseline_cost_usd": "1.5", "overhead_usd": "0",
				"savings_usd":           map[string]any{"routing": "0", "cache": "0", "net": "0"},
				"projected_savings_usd": "0",
				"cache_hit_rate":        0.0, "fallback_rate": 0.0,
			}),
			withoutShadow(map[string]any{
				"model": "m1", "requests": 2.0, "errors": 0.0, "unpriced_requests": 0.0,
				"tokens":   map[string]any{"input": 20.0, "cached_input": 0.0, "cache_write": 0.0, "output": 10.0},
				"cost_usd": "0.250000000000001", "baseline_cost_usd": "0.500000000000001", "overhead_usd": "0",
				"savings_usd":           map[string]any{"routing": "0.25", "cache": "0", "net": "0.25"},
				"projected_savings_usd": "0.4",
				"cache_hit_rate":        0.0, "fallback_rate": 0.0,
			}),
		},
	}
	if diff := cmp.Diff(want, body); diff != "" {
		t.Errorf("report (-want +got):\n%s", diff)
	}
	if !strings.HasPrefix(raw, `{"object":"usage_report"`) || !strings.Contains(raw, `{"model":"m2","requests":1,`) {
		t.Errorf("groups are not flat with the dimension first: %s", raw)
	}

	// Tenant b sees its own event only, whatever it asks for.
	pb := &auth.Principal{TenantID: "b", KeyID: "b-key"}
	_, body, raw = get(t, h, "/switchyard/v1/usage?tenant=a", pb)
	if tot := body["totals"].(map[string]any); tot["requests"] != 1.0 || tot["cost_usd"] != "7" || body["tenant"] != "b" {
		t.Errorf("tenant b's report: %s", raw)
	}

	// No principal: 401, never a report across tenants.
	if code, _, raw := get(t, h, "/switchyard/v1/usage", nil); code != http.StatusUnauthorized {
		t.Errorf("without a principal: %d %s", code, raw)
	}
}

// withoutShadow adds to totals the shadow fields of requests without any
// shadow evaluation or feedback.
func withoutShadow(totals map[string]any) map[string]any {
	totals["quality"] = []any{}
	totals["verified_savings_usd"] = "0"
	totals["shadow_cost_usd"] = "0"
	totals["feedback"] = map[string]any{"negative": 0.0, "neutral": 0.0, "positive": 0.0}
	return totals
}

func TestQueryParameters(t *testing.T) {
	h := TenantHandler(seeded(t), Options{Now: func() time.Time { return now }})
	pa := &auth.Principal{TenantID: "a", KeyID: "a-key"}
	tests := []struct {
		name, query string
		status      int
		param       string
		requests    float64
		from, to    string
	}{
		{"default period", "", 200, "", 3, "2026-11-04T12:00:00Z", "2026-11-11T12:00:00Z"},
		{"dates, to inclusive", "from=2026-11-11&to=2026-11-11", 200, "", 2, "2026-11-11T00:00:00Z", "2026-11-12T00:00:00Z"},
		{"rfc 3339 with offset", "from=2026-11-11T09:00:00%2B01:00&to=2026-11-11T12:00:00Z", 200, "", 2, "2026-11-11T08:00:00Z", "2026-11-11T12:00:00Z"},
		{"only to", "to=2026-11-10", 200, "", 1, "2026-11-04T00:00:00Z", "2026-11-11T00:00:00Z"},
		{"long span", "from=2025-10-10&to=2026-11-11", 200, "", 4, "2025-10-10T00:00:00Z", "2026-11-12T00:00:00Z"},
		{"too long", "from=2025-10-01&to=2026-11-11", 400, "from", 0, "", ""},
		{"from after to", "from=2026-11-12&to=2026-11-10", 400, "from", 0, "", ""},
		{"bad from", "from=yesterday", 400, "from", 0, "", ""},
		{"bad to", "to=2026-13-01", 400, "to", 0, "", ""},
		{"unknown dimension", "group_by=persona", 400, "", 0, "", ""},
		{"profile and routing mode", "group_by=profile,routing_mode", 200, "", 3, "2026-11-04T12:00:00Z", "2026-11-11T12:00:00Z"},
		{"repeated dimension", "group_by=model,model", 400, "", 0, "", ""},
		{"empty dimension", "group_by=model,,day", 400, "group_by", 0, "", ""},
		{"bad tag", "group_by=tag:Team", 400, "", 0, "", ""},
		{"nine dimensions", "group_by=day,model,deployment,provider,key,endpoint,status_class,cache_status,tag:team", 400, "", 0, "", ""},
		{"four tags", "group_by=tag:a,tag:b,tag:c,tag:d", 400, "", 0, "", ""},
		{"eight dimensions", "group_by=day,model,deployment,provider,key,tag:a,tag:b,tag:c", 200, "", 3, "2026-11-04T12:00:00Z", "2026-11-11T12:00:00Z"},
		{"tag and day", "group_by=tag:team,day", 200, "", 3, "2026-11-04T12:00:00Z", "2026-11-11T12:00:00Z"},
		{"limit", "group_by=model&limit=1", 200, "", 3, "2026-11-04T12:00:00Z", "2026-11-11T12:00:00Z"},
		{"bad limit", "limit=0", 400, "limit", 0, "", ""},
		{"limit not a number", "limit=ten", 400, "limit", 0, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body, raw := get(t, h, "/switchyard/v1/usage?"+tt.query, pa)
			if code != tt.status {
				t.Fatalf("status %d, want %d: %s", code, tt.status, raw)
			}
			if code != http.StatusOK {
				e := body["error"].(map[string]any)
				param, _ := e["param"].(string)
				if e["code"] != codeInvalidQuery || e["type"] != "invalid_request_error" || param != tt.param {
					t.Errorf("error %s", raw)
				}
				for _, v := range []string{"yesterday", "persona", "Team", "ten"} {
					if strings.Contains(e["message"].(string), v) {
						t.Errorf("the message echoes %q: %s", v, raw)
					}
				}
				return
			}
			p := body["period"].(map[string]any)
			if body["totals"].(map[string]any)["requests"] != tt.requests || p["from"] != tt.from || p["to"] != tt.to {
				t.Errorf("report %s", raw)
			}
			if tt.name == "limit" && len(body["groups"].([]any)) != 1 {
				t.Errorf("limit=1 returned %d groups", len(body["groups"].([]any)))
			}
		})
	}
}

func TestAdminReport(t *testing.T) {
	h := AdminHandler(seeded(t), Options{Now: func() time.Time { return now }})
	code, body, raw := get(t, h, "/admin/v1/usage?group_by=key", nil)
	if code != http.StatusOK || body["tenant"] != nil || body["totals"].(map[string]any)["requests"] != 4.0 || len(body["groups"].([]any)) != 2 {
		t.Errorf("all tenants: %d %s", code, raw)
	}
	_, body, raw = get(t, h, "/admin/v1/usage?tenant=b", nil)
	if body["tenant"] != "b" || body["totals"].(map[string]any)["cost_usd"] != "7" {
		t.Errorf("tenant b: %s", raw)
	}
	if code, _, raw := get(t, h, "/admin/v1/usage?tenant=Not%20An%20ID", nil); code != http.StatusBadRequest || strings.Contains(raw, "Not An ID") {
		t.Errorf("bad tenant: %d %s", code, raw)
	}
}

// brokenReporter fails every report, as Postgres does when it is down.
type brokenReporter struct{}

func (brokenReporter) Report(context.Context, usage.Query) (*usage.Report, error) {
	return nil, errors.New("usage: report: postgres error SQLSTATE 08006")
}

func TestReportFails(t *testing.T) {
	h := AdminHandler(brokenReporter{}, Options{})
	code, body, raw := get(t, h, "/admin/v1/usage", nil)
	if code != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != codeUnavailable || strings.Contains(raw, "SQLSTATE") {
		t.Errorf("%d %s", code, raw)
	}
}

// errReporter fails every report with err.
type errReporter struct{ err error }

func (r errReporter) Report(context.Context, usage.Query) (*usage.Report, error) { return nil, r.err }

// TestReportErrorStatuses checks the answer to each kind of report
// failure: a busy store 429 report_busy, a query the database finds too
// complex 400 invalid_query, neither logged as a failure.
func TestReportErrorStatuses(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"busy", usage.ErrReportBusy, http.StatusTooManyRequests, codeBusy},
		{"too complex", fmt.Errorf("%w: the report is too complex for the database", usage.ErrInvalidQuery), http.StatusBadRequest, codeInvalidQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := AdminHandler(errReporter{tt.err}, Options{})
			req := httptest.NewRequest(http.MethodGet, "/admin/v1/usage", nil)
			var logs strings.Builder
			req = req.WithContext(reqctx.WithLogger(req.Context(), slog.New(slog.NewJSONHandler(&logs, nil))))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var body map[string]map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tt.status || body["error"]["code"] != tt.code {
				t.Errorf("%d %s, want %d %s", w.Code, w.Body.String(), tt.status, tt.code)
			}
			if tt.status == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After on report_busy")
			}
			if logs.Len() != 0 {
				t.Errorf("logged: %s", logs.String())
			}
		})
	}
}
