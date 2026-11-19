package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
)

// limitsConfig returns a config with tenant t, limited to the tenant
// rate_limit tl, whose key "limited" has no override, key "own" overrides
// it with kl (when not nil), and key "free" overrides it with no limits.
func limitsConfig(tl config.RateLimit, kl *config.RateLimit, failMode string) *config.Config {
	return &config.Config{
		RateLimits: config.RateLimits{FailMode: failMode, AuthFailuresPerMinute: 30},
		Tenants: []config.Tenant{{
			ID:        "t",
			RateLimit: tl,
			Keys: []config.APIKey{
				{ID: "limited"},
				{ID: "own", RateLimit: kl},
				{ID: "free", RateLimit: &config.RateLimit{}},
			},
		}},
	}
}

// keyLimit returns the effective rate limit of keyID in c, as
// auth.NewStore resolves it into the Principal: the key's own when set,
// else its tenant's.
func keyLimit(c *config.Config, keyID string) config.RateLimit {
	for _, t := range c.Tenants {
		for _, k := range t.Keys {
			if k.ID != keyID {
				continue
			}
			if k.RateLimit != nil {
				return *k.RateLimit
			}
			return t.RateLimit
		}
	}
	return config.RateLimit{}
}

func TestNewLimits(t *testing.T) {
	tests := []struct {
		name         string
		failMode     string
		wantFailOpen bool
	}{
		{"closed", config.RateLimitFailClosed, false},
		{"open", config.RateLimitFailOpen, true},
		{"default", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewLimits(limitsConfig(config.RateLimit{}, nil, tt.failMode))
			if l.FailOpen() != tt.wantFailOpen || l.AuthFailuresPerMinute() != 30 {
				t.Errorf("FailOpen %v, AuthFailuresPerMinute %d", l.FailOpen(), l.AuthFailuresPerMinute())
			}
		})
	}
	var none *Limits
	if !none.FailOpen() || none.AuthFailuresPerMinute() != 0 {
		t.Error("nil Limits limits something")
	}
}

// enforcerFixture is an Enforcer over a flakyLimiter, with a swappable
// config (its Limits, and the keys' limits its requests authenticate
// with), its registry and a log buffer.
type enforcerFixture struct {
	e       *Enforcer
	limiter *flakyLimiter
	cfg     atomic.Pointer[config.Config]
	limits  atomic.Pointer[Limits]
	reg     *prometheus.Registry
	log     bytes.Buffer
}

func newEnforcerFixture(t *testing.T, c *config.Config) *enforcerFixture {
	t.Helper()
	f := &enforcerFixture{limiter: newFlakyLimiter(), reg: prometheus.NewRegistry()}
	f.reload(c)
	e, err := NewEnforcer(EnforcerOptions{Limiter: f.limiter, Limits: f.limits.Load, Registerer: f.reg})
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	f.e = e
	return f
}

// reload makes c the config in force, as a config reload does: its
// Limits, and its keys' limits for the requests that follow.
func (f *enforcerFixture) reload(c *config.Config) {
	f.cfg.Store(c)
	f.limits.Store(NewLimits(c))
}

// request returns a POST with a body, authenticated as keyID, with its
// effective limit in the config in force, unless it is "", whose context
// logs to f.log.
func (f *enforcerFixture) request(keyID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	ctx := reqctx.WithLogger(r.Context(), slog.New(slog.NewJSONHandler(&f.log, nil)))
	if keyID != "" {
		ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "t", KeyID: keyID, RateLimit: keyLimit(f.cfg.Load(), keyID)})
	}
	return r.WithContext(ctx)
}

// decisions returns the value of switchyard_ratelimit_decisions_total for
// kind and result.
func (f *enforcerFixture) decisions(t *testing.T, kind, result string) float64 {
	t.Helper()
	families, err := f.reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "switchyard_ratelimit_decisions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["kind"] == kind && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// serve runs r through f's Middleware in front of a handler that answers
// 204 and records the request it saw.
func (f *enforcerFixture) serve(r *http.Request) (*httptest.ResponseRecorder, *http.Request) {
	var seen *http.Request
	h := f.e.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, seen
}

// errorBody is the OpenAI error envelope.
type errorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

// checkRejection checks that w is the OpenAI-shaped error with status,
// type, code and Retry-After (empty: none).
func checkRejection(t *testing.T, w *httptest.ResponseRecorder, status int, typ, code, retryAfter string) string {
	t.Helper()
	if w.Code != status {
		t.Errorf("status = %d, want %d", w.Code, status)
	}
	if got := w.Header().Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if body.Error.Type != typ || body.Error.Code == nil || *body.Error.Code != code || body.Error.Param != nil || body.Error.Message == "" {
		t.Errorf("error = %s", w.Body.String())
	}
	return body.Error.Message
}

func TestMiddlewareLimitsRequests(t *testing.T) {
	// 60/min with burst 1: two requests at once, then one a second.
	f := newEnforcerFixture(t, limitsConfig(config.RateLimit{RequestsPerMinute: 60, Burst: 1}, nil, config.RateLimitFailOpen))
	for i := range 2 {
		if w, seen := f.serve(f.request("limited")); w.Code != http.StatusNoContent || seen == nil {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w, seen := f.serve(f.request("limited"))
	if seen != nil {
		t.Error("the handler ran for a refused request")
	}
	msg := checkRejection(t, w, http.StatusTooManyRequests, "requests", CodeRateLimited, "1")
	if !strings.Contains(msg, "retry in 1s") {
		t.Errorf("message = %q", msg)
	}
	// The body was never read: the connection is closed rather than
	// drained (server.AbandonBody).
	if w.Header().Get("Connection") != "close" {
		t.Errorf("Connection = %q, want close", w.Header().Get("Connection"))
	}
	if got := f.decisions(t, kindRequest, resultAllowed); got != 2 {
		t.Errorf("allowed = %v, want 2", got)
	}
	if got := f.decisions(t, kindRequest, resultLimited); got != 1 {
		t.Errorf("limited = %v, want 1", got)
	}

	// A reload that lifts the limit applies to the next request.
	f.reload(limitsConfig(config.RateLimit{}, nil, config.RateLimitFailOpen))
	calls := f.limiter.count()
	for range 5 {
		if w, _ := f.serve(f.request("limited")); w.Code != http.StatusNoContent {
			t.Fatalf("after the reload: %d", w.Code)
		}
	}
	if f.limiter.count() != calls {
		t.Errorf("an unlimited key called the limiter %d times", f.limiter.count()-calls)
	}
}

func TestMiddlewarePassesThrough(t *testing.T) {
	tests := []struct {
		name  string
		keyID string
	}{
		{"unauthenticated", ""},
		{"key without limits", "free"},
		{"unknown key", "nobody"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEnforcerFixture(t, limitsConfig(config.RateLimit{RequestsPerMinute: 1, MaxConcurrentStreams: 1}, nil, config.RateLimitFailClosed))
			f.limiter.set(errDown, false) // any call would fail closed
			for range 3 {
				w, seen := f.serve(f.request(tt.keyID))
				if w.Code != http.StatusNoContent {
					t.Fatalf("status %d", w.Code)
				}
				release, rej := f.e.AcquireStream(seen.Context())
				if rej != nil {
					t.Fatalf("AcquireStream: %v", rej)
				}
				release()
			}
			if n := f.limiter.count(); n != 0 {
				t.Errorf("%d limiter calls, want 0", n)
			}
		})
	}
}

func TestMiddlewareRetryAfterRoundsUp(t *testing.T) {
	// 1/min: the second request waits a minute; 30 s later, 30 s; 29.5 s
	// later, half a second, which is sent as 1.
	f := newEnforcerFixture(t, limitsConfig(config.RateLimit{RequestsPerMinute: 1}, nil, config.RateLimitFailOpen))
	clock := newFakeClock()
	f.limiter.Limiter = NewMemory(clock.Now)
	if w, _ := f.serve(f.request("limited")); w.Code != http.StatusNoContent {
		t.Fatalf("first request: %d", w.Code)
	}
	for _, tt := range []struct {
		advance time.Duration
		want    string
	}{{0, "60"}, {30 * time.Second, "30"}, {29500 * time.Millisecond, "1"}} {
		clock.Advance(tt.advance)
		w, _ := f.serve(f.request("limited"))
		checkRejection(t, w, http.StatusTooManyRequests, "requests", CodeRateLimited, tt.want)
	}
}

func TestMiddlewareFailMode(t *testing.T) {
	tests := []struct {
		name     string
		failMode string
		err      error
		wantCode int
		wantLog  bool
	}{
		{"open", config.RateLimitFailOpen, errDown, http.StatusNoContent, true},
		{"closed", config.RateLimitFailClosed, errDown, http.StatusServiceUnavailable, true},
		{"open, circuit open", config.RateLimitFailOpen, errCircuitOpen, http.StatusNoContent, false},
		{"closed, circuit open", config.RateLimitFailClosed, errCircuitOpen, http.StatusServiceUnavailable, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEnforcerFixture(t, limitsConfig(config.RateLimit{RequestsPerMinute: 60}, nil, tt.failMode))
			f.limiter.set(tt.err, false)
			w, _ := f.serve(f.request("limited"))
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusServiceUnavailable {
				checkRejection(t, w, http.StatusServiceUnavailable, "server_error", CodeUnavailable, "")
			}
			if got := f.decisions(t, kindRequest, resultError); got != 1 {
				t.Errorf("error decisions = %v, want 1", got)
			}
			logged := strings.Contains(f.log.String(), "rate-limit store unavailable")
			if logged != tt.wantLog {
				t.Errorf("logged %v, want %v: %s", logged, tt.wantLog, f.log.String())
			}
			if tt.wantLog && !strings.Contains(f.log.String(), `"level":"WARN"`) {
				t.Errorf("not logged at WARN: %s", f.log.String())
			}
		})
	}
}

func TestAcquireStream(t *testing.T) {
	f := newEnforcerFixture(t, limitsConfig(config.RateLimit{MaxConcurrentStreams: 2}, nil, config.RateLimitFailClosed))
	_, seen := f.serve(f.request("limited"))
	if seen == nil {
		t.Fatal("request refused")
	}
	ctx := seen.Context()
	r1, rej := f.e.AcquireStream(ctx)
	if rej != nil {
		t.Fatalf("first stream: %v", rej)
	}
	r2, rej := f.e.AcquireStream(ctx)
	if rej != nil {
		t.Fatalf("second stream: %v", rej)
	}
	release, rej := f.e.AcquireStream(ctx)
	if rej == nil || release == nil {
		t.Fatal("third stream allowed under a cap of 2")
	}
	release() // a refusal's release is a harmless no-op
	want := &Rejection{
		Status: http.StatusTooManyRequests, Type: "requests", Code: CodeRateLimited, RetryAfter: time.Second,
		Message: "Too many concurrent streams on this API key: the limit is 2. Please retry when one has finished.",
	}
	if diff := cmp.Diff(want, rej); diff != "" {
		t.Errorf("rejection (-want +got):\n%s", diff)
	}
	w := httptest.NewRecorder()
	rej.Write(w)
	checkRejection(t, w, http.StatusTooManyRequests, "requests", CodeRateLimited, "1")

	r1()
	r1() // double release frees one slot only
	r3, rej := f.e.AcquireStream(ctx)
	if rej != nil {
		t.Fatalf("stream after a release: %v", rej)
	}
	if _, rej := f.e.AcquireStream(ctx); rej == nil {
		t.Error("a double release freed a second slot")
	}
	r2()
	r3()
	for result, want := range map[string]float64{resultAllowed: 3, resultLimited: 2, resultError: 0} {
		if got := f.decisions(t, kindStream, result); got != want {
			t.Errorf("stream %s = %v, want %v", result, got, want)
		}
	}
	// The request rate was never asked: the key has none.
	if got := f.decisions(t, kindRequest, resultAllowed); got != 0 {
		t.Errorf("request decisions = %v, want 0", got)
	}
}

func TestAcquireStreamFailMode(t *testing.T) {
	tests := []struct {
		failMode string
		want     *Rejection
	}{
		{config.RateLimitFailOpen, nil},
		{config.RateLimitFailClosed, StoreUnavailable()},
	}
	for _, tt := range tests {
		t.Run(tt.failMode, func(t *testing.T) {
			f := newEnforcerFixture(t, limitsConfig(config.RateLimit{MaxConcurrentStreams: 1}, nil, tt.failMode))
			_, seen := f.serve(f.request("limited"))
			// The fail mode is the one the request started with, even
			// after a reload changes it.
			other := config.RateLimitFailOpen
			if tt.failMode == other {
				other = config.RateLimitFailClosed
			}
			f.reload(limitsConfig(config.RateLimit{MaxConcurrentStreams: 1}, nil, other))
			f.limiter.set(errDown, false)
			release, rej := f.e.AcquireStream(seen.Context())
			if diff := cmp.Diff(tt.want, rej); diff != "" {
				t.Errorf("rejection (-want +got):\n%s", diff)
			}
			if release == nil {
				t.Fatal("nil release")
			}
			release()
			if got := f.decisions(t, kindStream, resultError); got != 1 {
				t.Errorf("stream errors = %v, want 1", got)
			}
		})
	}
}

func TestNewEnforcerErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	opts := EnforcerOptions{Limiter: NewMemory(nil), Limits: func() *Limits { return nil }, Registerer: reg}
	if _, err := NewEnforcer(opts); err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	if _, err := NewEnforcer(opts); err == nil {
		t.Error("registering the metric twice succeeded")
	}
	for _, o := range []EnforcerOptions{
		{Limits: opts.Limits, Registerer: reg},
		{Limiter: opts.Limiter, Registerer: reg},
		{Limiter: opts.Limiter, Limits: opts.Limits},
	} {
		if _, err := NewEnforcer(o); err == nil {
			t.Errorf("NewEnforcer(%+v) succeeded", o)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want int64
	}{{0, 1}, {time.Nanosecond, 1}, {time.Second, 1}, {time.Second + 1, 2}, {59*time.Second + time.Millisecond, 60}}
	for _, tt := range tests {
		if got := RetryAfterSeconds(tt.d); got != tt.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tt.d, got, tt.want)
		}
	}
}

func TestWriteRateLimited(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRateLimited(w, 2500*time.Millisecond)
	msg := checkRejection(t, w, http.StatusTooManyRequests, "requests", CodeRateLimited, "3")
	if !strings.Contains(msg, "3s") {
		t.Errorf("message = %q", msg)
	}
}

// TestLimiterClientGone: a limiter call that fails because the client went
// away (its context ended) is neither counted nor treated as a store
// failure, whatever the fail mode: the request ends there, unserved.
func TestLimiterClientGone(t *testing.T) {
	canceled := func(ctx context.Context) context.Context {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx
	}
	for _, failMode := range []string{config.RateLimitFailOpen, config.RateLimitFailClosed} {
		t.Run(failMode, func(t *testing.T) {
			f := newEnforcerFixture(t, limitsConfig(config.RateLimit{RequestsPerMinute: 60, MaxConcurrentStreams: 1}, nil, failMode))
			_, served := f.serve(f.request("limited")) // the store answers
			if served == nil {
				t.Fatal("not served while the store answers")
			}

			f.limiter.set(context.Canceled, false)
			r := f.request("limited")
			w, seen := f.serve(r.WithContext(canceled(r.Context())))
			if seen != nil || w.Code != statusClientClosedRequest {
				t.Errorf("status %d, served %v; want 499, not served", w.Code, seen != nil)
			}
			release, _ := f.e.AcquireStream(canceled(served.Context()))
			release()
			for _, kind := range []string{kindRequest, kindStream} {
				if got := f.decisions(t, kind, resultError); got != 0 {
					t.Errorf("%s error decisions = %v, want 0", kind, got)
				}
			}
			if strings.Contains(f.log.String(), "rate-limit store unavailable") {
				t.Errorf("logged a store failure: %s", f.log.String())
			}
		})
	}
}
