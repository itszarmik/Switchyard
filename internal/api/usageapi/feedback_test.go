package usageapi

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	idA      = "019a7000-0000-7000-8000-00000000000a"
	idB      = "019a7000-0000-7000-8000-00000000000b"
	idNone   = "019a7000-0000-7000-8000-0000000000ff"
	sentinel = "COMMENT-SENTINEL-55"
)

// feedbackLedger holds one request of tenant a (idA) and one of b (idB).
func feedbackLedger() *usage.MemoryLedger {
	l := usage.NewMemoryLedger()
	l.Record(&usage.Event{ID: idA, Time: now.Add(-time.Hour), Tenant: "a", Category: "coding", Status: 200, CostKnown: true})
	l.Record(&usage.Event{ID: idB, Time: now.Add(-time.Hour), Tenant: "b", Category: "coding", Status: 200, CostKnown: true})
	return l
}

func post(t *testing.T, h http.Handler, body string, p *auth.Principal, logs *strings.Builder) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/switchyard/v1/feedback", strings.NewReader(body))
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), *p))
	}
	if logs != nil {
		req = req.WithContext(reqctx.WithLogger(req.Context(), slog.New(slog.NewJSONHandler(logs, nil))))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String()
}

func TestFeedback(t *testing.T) {
	pa := &auth.Principal{TenantID: "a", KeyID: "a-key"}
	long := strings.Repeat("é", usage.MaxFeedbackComment)
	tests := []struct {
		name   string
		body   string
		p      *auth.Principal
		status int
		param  string
		code   string
	}{
		{"positive", `{"request_id":"` + idA + `","score":1}`, pa, 202, "", ""},
		{"negative with comment", `{"request_id":"` + idA + `","score":-1,"comment":"` + sentinel + `\nline two"}`, pa, 202, "", ""},
		{"neutral, null comment", `{"request_id":"` + idA + `","score":0,"comment":null}`, pa, 202, "", ""},
		{"500 characters", `{"request_id":"` + idA + `","score":1,"comment":"` + long + `"}`, pa, 202, "", ""},

		{"no principal", `{"request_id":"` + idA + `","score":1}`, nil, 401, "", codeAuth},
		{"unknown request", `{"request_id":"` + idNone + `","score":1}`, pa, 404, "request_id", codeRequestNotFound},
		{"another tenant's request", `{"request_id":"` + idB + `","score":1}`, pa, 404, "request_id", codeRequestNotFound},

		{"not json", `score=1`, pa, 400, "", codeInvalidFeedback},
		{"two objects", `{"request_id":"` + idA + `","score":1}{}`, pa, 400, "", codeInvalidFeedback},
		{"unknown member", `{"request_id":"` + idA + `","score":1,"tenant":"b"}`, pa, 400, "", codeInvalidFeedback},
		{"no request id", `{"score":1}`, pa, 400, "request_id", codeInvalidFeedback},
		{"request id not a uuid", `{"request_id":"` + sentinel + `","score":1}`, pa, 400, "request_id", codeInvalidFeedback},
		{"request id uppercase", `{"request_id":"` + strings.ToUpper(idA) + `","score":1}`, pa, 400, "request_id", codeInvalidFeedback},
		{"request id a number", `{"request_id":7,"score":1}`, pa, 400, "request_id", codeInvalidFeedback},
		{"no score", `{"request_id":"` + idA + `"}`, pa, 400, "score", codeInvalidFeedback},
		{"score 2", `{"request_id":"` + idA + `","score":2}`, pa, 400, "score", codeInvalidFeedback},
		{"score 1.0", `{"request_id":"` + idA + `","score":1.0}`, pa, 400, "score", codeInvalidFeedback},
		{"score a string", `{"request_id":"` + idA + `","score":"1"}`, pa, 400, "score", codeInvalidFeedback},
		{"comment too long", `{"request_id":"` + idA + `","score":1,"comment":"` + long + `x"}`, pa, 400, "comment", codeInvalidFeedback},
		{"comment not a string", `{"request_id":"` + idA + `","score":1,"comment":["` + sentinel + `"]}`, pa, 400, "comment", codeInvalidFeedback},
		{"comment with a control character", `{"request_id":"` + idA + `","score":1,"comment":"` + sentinel + `\u0007"}`, pa, 400, "comment", codeInvalidFeedback},
		{"body too large", `{"request_id":"` + idA + `","score":1,"comment":"` + strings.Repeat(" ", maxFeedbackBody) + `"}`, pa, 400, "", codeInvalidFeedback},
		{"invalid utf-8", "{\"request_id\":\"" + idA + "\",\"score\":1,\"comment\":\"\xff\"}", pa, 400, "", codeInvalidFeedback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs strings.Builder
			h := FeedbackHandler(feedbackLedger(), Options{Now: func() time.Time { return now }})
			code, body, raw := post(t, h, tt.body, tt.p, &logs)
			if code != tt.status {
				t.Fatalf("status %d, want %d: %s", code, tt.status, raw)
			}
			if strings.Contains(raw, sentinel) || strings.Contains(logs.String(), sentinel) {
				t.Errorf("the response or the logs echo the body: %s %s", raw, logs.String())
			}
			if code == http.StatusAccepted {
				if body["status"] != "accepted" {
					t.Errorf("body %s", raw)
				}
				return
			}
			e := body["error"].(map[string]any)
			param, _ := e["param"].(string)
			if e["code"] != tt.code || param != tt.param {
				t.Errorf("error %s, want code %s param %q", raw, tt.code, tt.param)
			}
		})
	}
}

// TestFeedbackReported: feedback is stored with the caller's tenant and
// key, the latest replaces earlier, and the usage report counts it.
func TestFeedbackReported(t *testing.T) {
	l := feedbackLedger()
	opts := Options{Now: func() time.Time { return now }}
	fh := FeedbackHandler(l, opts)
	pa := &auth.Principal{TenantID: "a", KeyID: "a-key"}
	for _, score := range []string{"1", "-1"} {
		if code, _, raw := post(t, fh, `{"request_id":"`+idA+`","score":`+score+`}`, pa, nil); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, raw)
		}
	}
	rh := TenantHandler(l, opts)
	_, body, raw := get(t, rh, "/switchyard/v1/usage", pa)
	want := map[string]any{"negative": 1.0, "neutral": 0.0, "positive": 0.0}
	if diff := cmp.Diff(want, body["totals"].(map[string]any)["feedback"]); diff != "" {
		t.Errorf("feedback in the report (-want +got):\n%s\n%s", diff, raw)
	}
	_, body, _ = get(t, rh, "/switchyard/v1/usage", &auth.Principal{TenantID: "b", KeyID: "b-key"})
	if diff := cmp.Diff(map[string]any{"negative": 0.0, "neutral": 0.0, "positive": 0.0}, body["totals"].(map[string]any)["feedback"]); diff != "" {
		t.Errorf("tenant b sees a's feedback:\n%s", diff)
	}
}

// failingStore fails every feedback.
type failingStore struct{}

func (failingStore) RecordFeedback(context.Context, *usage.Feedback) error {
	return errors.New("usage: write feedback: postgres error SQLSTATE 08006")
}

func TestFeedbackStoreFails(t *testing.T) {
	var logs strings.Builder
	h := FeedbackHandler(failingStore{}, Options{})
	code, body, raw := post(t, h, `{"request_id":"`+idA+`","score":1,"comment":"`+sentinel+`"}`, &auth.Principal{TenantID: "a"}, &logs)
	if code != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != codeUnavailable || strings.Contains(raw, "SQLSTATE") {
		t.Errorf("%d %s", code, raw)
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the comment was logged: %s", logs.String())
	}
}

// TestReportQualityFields: shadow verification shows in the totals and in
// groups by category, with the verification rule from Options.
func TestReportQualityFields(t *testing.T) {
	l := feedbackLedger()
	ctx := context.Background()
	if err := l.RecordShadowEval(ctx, &usage.ShadowEval{RequestID: idA, Tenant: "a", Acceptable: true, Outcome: "equivalent", Cost: money(t, "0.002")}); err != nil {
		t.Fatal(err)
	}
	h := TenantHandler(l, Options{Now: func() time.Time { return now }, Verification: usage.Verification{MinSamples: 1, MinPassRate: 0.2}})
	_, body, raw := get(t, h, "/switchyard/v1/usage?group_by=category", &auth.Principal{TenantID: "a"})
	if diff := cmp.Diff(map[string]any{"min_samples": 1.0, "min_pass_rate": 0.2, "bound": "wilson_lower_90"}, body["verification"]); diff != "" {
		t.Errorf("verification (-want +got):\n%s", diff)
	}
	tot := body["totals"].(map[string]any)
	q := tot["quality"].([]any)
	if len(q) != 1 || tot["shadow_cost_usd"] != "0.002" {
		t.Fatalf("totals: %s", raw)
	}
	c := q[0].(map[string]any)
	if c["category"] != "coding" || c["judged"] != 1.0 || c["acceptable"] != 1.0 || c["errors"] != 0.0 || c["pass_rate"] != 1.0 ||
		c["verified"] != true || c["wilson_lower"].(float64) < 0.2 {
		t.Errorf("quality entry: %v", c)
	}
	groups := body["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["category"] != "coding" || len(groups[0].(map[string]any)["quality"].([]any)) != 1 {
		t.Errorf("groups: %s", raw)
	}
}
