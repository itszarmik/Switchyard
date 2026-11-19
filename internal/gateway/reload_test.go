package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/reqctx"
)

// reloadEnv is an execEnv that keeps its configuration, so tests can
// reload edited copies of it.
type reloadEnv struct {
	*execEnv
	cfg *config.Config
}

func newReloadEnv(t *testing.T, routing config.Routing) *reloadEnv {
	t.Helper()
	var cfg *config.Config
	e := newExecEnv(t, routing, func(c *config.Config) { cfg = c })
	return &reloadEnv{execEnv: e, cfg: cfg}
}

// reload reloads a copy of the configuration edited by edit and injects
// the scripted adapters into the new snapshot, as newExecEnv does.
func (e *reloadEnv) reload(t *testing.T, edit func(*config.Config)) {
	t.Helper()
	c := *e.cfg
	c.Models = slices.Clone(e.cfg.Models)
	if edit != nil {
		edit(&c)
	}
	if err := e.g.Reload(&c, "v2"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s := e.g.current()
	s.adapters["a"], s.adapters["b"], s.adapters["c"] = e.a, e.b, e.c
}

// TestReloadKeepsHealthState: a breaker opened before a reload is still
// open after it, on the same handle, and routing still avoids it.
func TestReloadKeepsHealthState(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	r.Breaker.ConsecutiveFailures = 1
	r.Breaker.OpenFor = config.Duration(time.Hour)
	e := newReloadEnv(t, r)
	e.a.script(step{err: err503})
	e.chat(false) // opens a's breaker
	before := e.g.current().deps["m@a"]
	if st := before.State(); st != health.StateOpen {
		t.Fatalf("breaker before the reload: %s, want open", st)
	}

	e.reload(t, nil)
	after := e.g.current().deps["m@a"]
	if after != before || after.State() != health.StateOpen {
		t.Errorf("breaker after the reload: same handle %v, state %s; want the same, open", after == before, after.State())
	}
	e.a.script(step{})
	if res := e.chat(false); res.status != 200 || res.info.Provider != "b" {
		t.Errorf("after the reload: status %d, provider %q; want 200 from b (a's breaker is open)", res.status, res.info.Provider)
	}
	if got := e.g.ConfigVersion(); got != "v2" {
		t.Errorf("ConfigVersion = %q, want v2", got)
	}
}

// TestReloadInFlight: a stream that started before a reload which removes
// its model completes on the old snapshot, while a request that starts
// after the reload gets 404.
func TestReloadInFlight(t *testing.T) {
	e := newReloadEnv(t, fastRouting())
	h := &heldAdapter{scripted: e.a, waiting: make(chan struct{}), release: make(chan struct{})}
	e.g.current().adapters["a"] = h
	done := make(chan exchangeResult, 1)
	go func() { done <- e.chat(true) }()
	<-h.waiting

	e.reload(t, func(c *config.Config) {
		c.Models = slices.DeleteFunc(c.Models, func(m config.Model) bool { return m.ID == "m" })
	})
	if res := e.chat(false); res.status != http.StatusNotFound {
		t.Errorf("a new request for the removed model: status %d, want 404", res.status)
	}

	close(h.release)
	res := <-done
	if res.status != 200 || res.info.Provider != "a" || len(res.events) == 0 || res.events[len(res.events)-1] != "[DONE]" {
		t.Errorf("the stream in flight: status %d, provider %q, events %v; want 200 from a ending in [DONE]", res.status, res.info.Provider, res.events)
	}
}

// TestReloadInvalid: a configuration the gateway cannot build is refused,
// and the snapshot in force stays.
func TestReloadInvalid(t *testing.T) {
	e := newReloadEnv(t, fastRouting())
	before := e.g.current()
	c := *e.cfg
	c.Models = append(slices.Clone(e.cfg.Models), config.Model{ID: "x", Deployments: []config.Deployment{
		{Provider: "nope", UpstreamModel: "x"},
	}})
	if err := e.g.Reload(&c, "bad"); err == nil {
		t.Fatal("Reload of a deployment on an unknown provider succeeded")
	}
	if err := e.g.Reload(nil, "nil"); err == nil {
		t.Fatal("Reload(nil) succeeded")
	}
	if e.g.current() != before {
		t.Error("a failed reload replaced the snapshot")
	}
	if res := e.chat(false); res.status != 200 {
		t.Errorf("after the failed reload: status %d, want 200", res.status)
	}
}

// TestReloadConcurrent reloads while requests are served, for the race
// detector.
func TestReloadConcurrent(t *testing.T) {
	e := newReloadEnv(t, fastRouting())
	stop := make(chan struct{})
	served := make(chan int, 1)
	go func() {
		n := 0
		defer func() { served <- n }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A fresh snapshot has real adapters until reload injects the
			// scripted ones, so only the absence of races is checked.
			e.chat(false)
			n++
		}
	}()
	for range 20 {
		c := *e.cfg
		if err := e.g.Reload(&c, "v"); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-served
}

// TestPurgeAudit: every purge writes an audit line with its result and
// remote IP, never the body.
func TestPurgeAudit(t *testing.T) {
	e := newCacheEnv(t, exactOn, nil, CacheOptions{}, nil)
	tests := []struct {
		body, result string
		status       int
	}{
		{`{"tenant":"t1"}`, "ok", 200},
		{`{"tenant":"secret-looking-nope"}`, "rejected", 400},
		{`garbage`, "rejected", 400},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		req := httptest.NewRequest(http.MethodPost, "/admin/v1/cache/purge", strings.NewReader(tt.body))
		req.RemoteAddr = "192.0.2.7:4242"
		req = req.WithContext(reqctx.WithLogger(context.Background(), log))
		w := httptest.NewRecorder()
		e.g.CachePurgeHandler().ServeHTTP(w, req)
		if w.Code != tt.status {
			t.Errorf("purge %s: status %d, want %d", tt.body, w.Code, tt.status)
		}
		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("purge %s: want one JSON audit line, got %q", tt.body, buf.String())
		}
		if line["msg"] != "audit" || line["action"] != "cache.purge" || line["result"] != tt.result || line["remote_ip"] != "192.0.2.7" {
			t.Errorf("purge %s: audit line %v", tt.body, line)
		}
		if strings.Contains(buf.String(), "secret-looking-nope") || strings.Contains(buf.String(), "garbage") {
			t.Errorf("purge %s: the audit line echoes the body: %s", tt.body, buf.String())
		}
	}
}

// TestUnknownCaller: a request whose Principal the snapshot does not know
// (its key or tenant was removed by a reload committed after it
// authenticated) is answered 401 invalid_api_key on every endpoint that
// routes or charges, with no upstream call, no budget charge and no usage
// event. A known Principal is served.
func TestUnknownCaller(t *testing.T) {
	budgets := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay,
		LimitUSD: mustPrice(t, "1"), OnExhausted: config.BudgetBlock}}
	endpoints := []struct{ endpoint, body string }{
		{EndpointChatCompletions, chatBody},
		{EndpointCompletions, `{"model":"m","prompt":"hi"}`},
		{EndpointEmbeddings, `{"model":"m","input":"hi"}`},
		{EndpointExplain, chatBody},
	}
	callers := []struct {
		name string
		p    auth.Principal
	}{
		{"revoked key", auth.Principal{TenantID: "t1", KeyID: "k-revoked"}},
		{"removed tenant", auth.Principal{TenantID: "t-removed", KeyID: "k1"}},
	}
	for _, ep := range endpoints {
		for _, c := range callers {
			t.Run(ep.endpoint+"/"+c.name, func(t *testing.T) {
				e := newAcctEnv(t, budgets, nil, nil)
				res := e.doAs(context.Background(), c.p, ep.endpoint, ep.body, nil)
				if res.status != http.StatusUnauthorized || res.err == nil || res.err.Code != "invalid_api_key" ||
					res.err.Type != apierror.TypeInvalidRequest {
					t.Fatalf("status %d, error %+v; want 401 invalid_api_key", res.status, res.err)
				}
				if got := e.calls(); got != "a=0 b=0 c=0" {
					t.Errorf("upstream calls %s, want none", got)
				}
				if evs := e.sink.events(); len(evs) != 0 {
					t.Errorf("%d usage events, want none", len(evs))
				}
				scope := e.g.current().scopes[principal{"t1", "k1"}][0]
				if spent, err := e.store.Spent(context.Background(), scope, e.now); err != nil || spent != 0 {
					t.Errorf("spent %d (%v), want 0", spent, err)
				}
			})
		}
	}
	t.Run("known caller", func(t *testing.T) {
		e := newAcctEnv(t, budgets, nil, nil)
		if res := e.do(EndpointChatCompletions, chatBody, nil); res.status != http.StatusOK {
			t.Fatalf("status %d %+v", res.status, res.err)
		}
	})
}
