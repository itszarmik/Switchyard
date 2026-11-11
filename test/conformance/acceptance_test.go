package conformance

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeanthropic"
	"switchyard/test/fakeupstream"
)

// The P2 acceptance cases, through the gateway binary: capability
// filtering, the cheapest deployment and pinning, the completions shim
// and embeddings. Each upstream is a fake that records what reached it.

// embedStub is an OpenAI-wire upstream that serves only /v1/embeddings and
// records the model of each request.
type embedStub struct {
	*httptest.Server
	mu     sync.Mutex
	models []string
}

func newEmbedStub(t *testing.T) *embedStub {
	t.Helper()
	s := &embedStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.models = append(s.models, body.Model)
		s.mu.Unlock()
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":%q,"usage":{"prompt_tokens":4,"total_tokens":4}}`, body.Model)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *embedStub) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

// acceptanceEnv is a gateway in front of two OpenAI-wire fakes (cheap and
// rich), a fake Messages API and two embedding stubs.
type acceptanceEnv struct {
	g           *harness.Gateway
	key         string
	cheap, rich *fakeupstream.Server
	claude      *fakeanthropic.Server
	embedCheap  *embedStub
	embedRich   *embedStub
}

func startAcceptance(t *testing.T, drop bool) *acceptanceEnv {
	t.Helper()
	e := &acceptanceEnv{
		cheap:      fakeupstream.New(fakeupstream.Options{APIKey: providerKey}),
		rich:       fakeupstream.New(fakeupstream.Options{APIKey: providerKey}),
		claude:     fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey}),
		embedCheap: newEmbedStub(t),
		embedRich:  newEmbedStub(t),
	}
	t.Cleanup(e.cheap.Close)
	t.Cleanup(e.rich.Close)
	t.Cleanup(e.claude.Close)
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: cheap, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: rich, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: claude, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: embed-cheap, type: openai, base_url: %q}
  - {id: embed-rich, type: openai, base_url: %q}
models:
  # Anthropic only, as the catalog declares Claude: no n, no sampling.
  - id: claude-only
    capabilities: [streaming, stream_usage, tools, vision, json_mode, json_schema]
    deployments:
      - {provider: claude, upstream_model: claude-up, pricing: {input: "3", output: "15"}}
  # A model on Anthropic (cheaper, temperature limited to 1) and on an
  # OpenAI-wire host that accepts n and temperature up to 2.
  - id: mixed
    capabilities: [streaming, stream_usage, sampling]
    deployments:
      - {provider: claude, upstream_model: mixed-claude, pricing: {input: "1", output: "2"}}
      - provider: rich
        upstream_model: mixed-rich
        capabilities: [streaming, stream_usage, sampling, temperature_2, n]
        pricing: {input: "5", output: "10"}
  # Text only.
  - id: text-only
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: cheap, upstream_model: text-up, pricing: {input: "1", output: "1"}}
  # Offered by two providers; rich is listed first but costs more.
  - id: duo
    capabilities: [streaming, stream_usage, sampling]
    deployments:
      - {provider: rich, upstream_model: duo-rich, pricing: {input: "4", output: "8"}}
      - {provider: cheap, upstream_model: duo-cheap, pricing: {input: "1", output: "2"}}
  - id: embed-a
    type: embedding
    dimensions: 2
    deployments:
      - {provider: claude, upstream_model: never, pricing: {input: "0.001"}}
      - {provider: embed-rich, upstream_model: embed-a-rich, pricing: {input: "0.5"}}
      - {provider: embed-cheap, upstream_model: embed-a-cheap, pricing: {input: "0.1"}}
  - id: embed-b
    type: embedding
    dimensions: 2
    deployments:
      - {provider: embed-cheap, upstream_model: embed-b-cheap, pricing: {input: "0.01"}}
aliases:
  smart: duo@rich
routing: {drop_unsupported_params: %v}
tenants:
  - {id: %s, keys: [{id: %s, hash: %q}]}
`, e.cheap.BaseURL(), e.rich.BaseURL(), e.claude.BaseURL(), e.embedCheap.URL+"/v1", e.embedRich.URL+"/v1",
		drop, harness.TenantID, harness.KeyID, hash)
	e.g = harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	return e
}

// reply is one gateway response.
type reply struct {
	status int
	header http.Header
	body   string
	events []string // the data payloads of an event stream
	err    struct {
		Message, Type string
		Code, Param   *string
	}
}

func (e *acceptanceEnv) post(t *testing.T, path, body string) reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.g.PublicURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := reply{status: resp.StatusCode, header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				r.events = append(r.events, data)
			}
		}
		return r
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.body = string(b)
	if r.status >= 400 {
		var env struct{ Error json.RawMessage }
		if json.Unmarshal(b, &env) == nil {
			_ = json.Unmarshal(env.Error, &r.err)
		}
	}
	return r
}

func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// upstreamModels returns the model of every chat request each fake got.
func (e *acceptanceEnv) upstreamModels(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	add := func(name string, reqs []fakeupstream.Request) {
		for _, r := range reqs {
			var b struct{ Model string }
			_ = json.Unmarshal(r.Body, &b)
			out[name] = append(out[name], b.Model)
		}
	}
	add("cheap", e.cheap.Requests())
	add("rich", e.rich.Requests())
	add("claude", e.claude.Requests())
	return out
}

func (e *acceptanceEnv) reset() {
	e.cheap.Reset()
	e.rich.Reset()
	e.claude.Reset()
}

func TestAcceptanceCapabilityFiltering(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := startAcceptance(t, false)
	user := `"messages":[{"role":"user","content":"hello"}]`
	image := `"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`
	tests := []struct {
		name         string
		body         string
		wantStatus   int
		wantCode     string
		wantMessage  string
		wantProvider string
		wantDropped  string
		wantUpstream map[string][]string
	}{
		{
			name: "n=2 on an Anthropic-only model", body: `{"model":"claude-only","n":2,` + user + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: "claude-only@claude (lacks capability n)",
			wantUpstream: map[string][]string{},
		},
		{
			name: "image on a text-only model", body: `{"model":"text-only",` + image + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: "text-only@cheap (lacks capability vision)",
			wantUpstream: map[string][]string{},
		},
		{
			name: "temperature 0.7 goes to the cheaper Anthropic deployment", body: `{"model":"mixed","temperature":0.7,` + user + `}`,
			wantStatus: 200, wantProvider: "claude", wantUpstream: map[string][]string{"claude": {"mixed-claude"}},
		},
		{
			name: "temperature 1.5 is never sent to Anthropic", body: `{"model":"mixed","temperature":1.5,` + user + `}`,
			wantStatus: 200, wantProvider: "rich", wantUpstream: map[string][]string{"rich": {"mixed-rich"}},
		},
		{
			name: "n=2 goes to the capable deployment", body: `{"model":"mixed","n":2,` + user + `}`,
			wantStatus: 200, wantProvider: "rich", wantUpstream: map[string][]string{"rich": {"mixed-rich"}},
		},
		{
			// Without sampling, temperature is always removed (never
			// refused), whatever the drop flag says.
			name: "temperature on a model without sampling is removed", body: `{"model":"text-only","temperature":1.5,` + user + `}`,
			wantStatus: 200, wantProvider: "cheap", wantDropped: "temperature", wantUpstream: map[string][]string{"cheap": {"text-up"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.reset()
			r := e.post(t, "/v1/chat/completions", tc.body)
			if r.status != tc.wantStatus {
				t.Fatalf("status %d, body %s, want %d", r.status, r.body, tc.wantStatus)
			}
			if tc.wantCode != "" {
				if str(r.err.Code) != tc.wantCode || r.err.Type != "invalid_request_error" || !strings.Contains(r.err.Message, tc.wantMessage) {
					t.Errorf("error %s, want code %s naming %q", r.body, tc.wantCode, tc.wantMessage)
				}
				t.Logf("400 message: %s", r.err.Message)
			}
			if got := r.header.Get("X-Switchyard-Provider"); got != tc.wantProvider {
				t.Errorf("X-Switchyard-Provider %q, want %q", got, tc.wantProvider)
			}
			if got := r.header.Get("X-Switchyard-Dropped-Params"); got != tc.wantDropped {
				t.Errorf("X-Switchyard-Dropped-Params %q, want %q", got, tc.wantDropped)
			}
			if got := e.upstreamModels(t); fmt.Sprint(got) != fmt.Sprint(tc.wantUpstream) {
				t.Errorf("upstream requests %v, want %v", got, tc.wantUpstream)
			}
		})
	}
}

func TestAcceptanceDropUnsupportedParams(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := startAcceptance(t, true)
	r := e.post(t, "/v1/chat/completions", `{"model":"claude-only","n":2,"temperature":0.5,"messages":[{"role":"user","content":"hello"}]}`)
	if r.status != 200 || r.header.Get("X-Switchyard-Provider") != "claude" {
		t.Fatalf("status %d, provider %q, body %s", r.status, r.header.Get("X-Switchyard-Provider"), r.body)
	}
	// claude-only lacks n (dropped because the flag is on) and sampling
	// (always dropped).
	if got := r.header.Get("X-Switchyard-Dropped-Params"); got != "n, temperature" {
		t.Errorf("X-Switchyard-Dropped-Params = %q, want %q", got, "n, temperature")
	}
	reqs := e.claude.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d Anthropic requests", len(reqs))
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"n", "temperature"} {
		if _, ok := sent[f]; ok {
			t.Errorf("the Anthropic request has %s: %s", f, reqs[0].Body)
		}
	}
	// Hard requirements are never dropped.
	r = e.post(t, "/v1/chat/completions", `{"model":"text-only","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`)
	if r.status != 400 || str(r.err.Code) != "no_eligible_model" {
		t.Errorf("image with dropping on: status %d, body %s", r.status, r.body)
	}
	checkRequestLines(t, e.g, "claude-only@claude", "n,temperature")
}

// checkRequestLines checks that some access-log line names deployment and
// has dropped as its dropped_params.
func checkRequestLines(t *testing.T, g *harness.Gateway, deployment, dropped string) {
	t.Helper()
	for _, line := range g.Logs() {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "request" || m["deployment"] != deployment {
			continue
		}
		if m["dropped_params"] == dropped {
			if _, ok := m["excluded"]; !ok {
				t.Errorf("request line has no excluded count: %s", line)
			}
			return
		}
	}
	t.Errorf("no request line for %s dropping %s", deployment, dropped)
}

func TestAcceptanceCheapestAndPinned(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := startAcceptance(t, false)
	user := `"messages":[{"role":"user","content":"hello"}]`
	for _, tc := range []struct {
		model, provider, upstream, alias string
	}{
		{"duo", "cheap", "duo-cheap", ""},
		{"duo@rich", "rich", "duo-rich", ""},
		{"duo@cheap", "cheap", "duo-cheap", ""},
		{"smart", "rich", "duo-rich", "smart"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			e.reset()
			r := e.post(t, "/v1/chat/completions", `{"model":"`+tc.model+`",`+user+`}`)
			if r.status != 200 || r.header.Get("X-Switchyard-Model") != "duo" ||
				r.header.Get("X-Switchyard-Provider") != tc.provider || r.header.Get("X-Switchyard-Alias") != tc.alias {
				t.Fatalf("status %d, headers %v", r.status, r.header)
			}
			want := map[string][]string{tc.provider: {tc.upstream}}
			if got := e.upstreamModels(t); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("upstream requests %v, want %v", got, want)
			}
		})
	}
}

func TestAcceptanceCompletionsShim(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := startAcceptance(t, false)
	t.Run("non-stream", func(t *testing.T) {
		e.reset()
		r := e.post(t, "/v1/completions", `{"model":"duo","prompt":"Say something","max_tokens":20}`)
		if r.status != 200 || r.header.Get("X-Switchyard-Shim") != "chat" {
			t.Fatalf("status %d, headers %v, body %s", r.status, r.header, r.body)
		}
		var c struct {
			ID, Object string
			Choices    []struct {
				Text         string
				Index        int
				FinishReason string `json:"finish_reason"`
			}
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(r.body), &c); err != nil {
			t.Fatal(err)
		}
		if c.Object != "text_completion" || !strings.HasPrefix(c.ID, "cmpl-") || len(c.Choices) != 1 ||
			c.Choices[0].Text != fakeupstream.Text || c.Choices[0].FinishReason != "stop" || c.Usage.PromptTokens <= 0 {
			t.Errorf("completion %s", r.body)
		}
		reqs := e.cheap.Requests()
		if len(reqs) != 1 || reqs[0].Path != "/v1/chat/completions" {
			t.Fatalf("upstream requests %+v", reqs)
		}
		t.Logf("completion: %s", r.body)
	})
	t.Run("stream", func(t *testing.T) {
		e.reset()
		r := e.post(t, "/v1/completions", `{"model":"claude-only","prompt":"Say something","stream":true}`)
		if r.status != 200 || r.header.Get("X-Switchyard-Shim") != "chat" || r.header.Get("X-Switchyard-Provider") != "claude" {
			t.Fatalf("status %d, headers %v", r.status, r.header)
		}
		var text strings.Builder
		for _, ev := range r.events[:len(r.events)-1] {
			var c struct {
				Object  string
				Choices []struct{ Text string }
			}
			if err := json.Unmarshal([]byte(ev), &c); err != nil || c.Object != "text_completion" {
				t.Fatalf("event %s", ev)
			}
			for _, ch := range c.Choices {
				text.WriteString(ch.Text)
			}
		}
		if last := r.events[len(r.events)-1]; last != "[DONE]" {
			t.Errorf("last event %s", last)
		}
		if text.String() != fakeupstream.Text {
			t.Errorf("streamed text %q, want %q", text.String(), fakeupstream.Text)
		}
		t.Logf("%d events through Anthropic, first %s", len(r.events), r.events[0])
	})
	for _, tc := range []struct{ field, body string }{
		{"echo", `{"model":"duo","prompt":"p","echo":true}`},
		{"suffix", `{"model":"duo","prompt":"p","suffix":"after"}`},
		{"logprobs", `{"model":"duo","prompt":"p","logprobs":1}`},
	} {
		t.Run(tc.field+" is rejected", func(t *testing.T) {
			e.reset()
			r := e.post(t, "/v1/completions", tc.body)
			if r.status != 400 || r.err.Type != "invalid_request_error" || str(r.err.Code) != "unsupported_parameter" || str(r.err.Param) != tc.field {
				t.Errorf("status %d, body %s", r.status, r.body)
			}
			if got := e.upstreamModels(t); len(got) != 0 {
				t.Errorf("upstream requests %v", got)
			}
		})
	}
}

func TestAcceptanceEmbeddings(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := startAcceptance(t, false)
	for i, tc := range []struct {
		model, provider string
		cheap, rich     []string // the upstream models each stub has seen so far
	}{
		// The Anthropic deployment is cheapest but cannot embed; the cheaper
		// of the other two serves it.
		{"embed-a", "embed-cheap", []string{"embed-a-cheap"}, nil},
		{"embed-a@embed-rich", "embed-rich", []string{"embed-a-cheap"}, []string{"embed-a-rich"}},
		{"embed-b", "embed-cheap", []string{"embed-a-cheap", "embed-b-cheap"}, []string{"embed-a-rich"}},
	} {
		r := e.post(t, "/v1/embeddings", `{"model":"`+tc.model+`","input":["one","two"]}`)
		if r.status != 200 || r.header.Get("X-Switchyard-Provider") != tc.provider {
			t.Fatalf("%d: status %d, headers %v, body %s", i, r.status, r.header, r.body)
		}
		if fmt.Sprint(e.embedCheap.got()) != fmt.Sprint(tc.cheap) || fmt.Sprint(e.embedRich.got()) != fmt.Sprint(tc.rich) {
			t.Errorf("%d: upstream models cheap %v rich %v, want %v and %v", i, e.embedCheap.got(), e.embedRich.got(), tc.cheap, tc.rich)
		}
	}
	// Pinned to the deployment that cannot embed: refused, never another
	// model.
	r := e.post(t, "/v1/embeddings", `{"model":"embed-a@claude","input":"x"}`)
	if r.status != 400 || str(r.err.Code) != "no_eligible_model" || !strings.Contains(r.err.Message, "provider does not serve embeddings") {
		t.Errorf("embed-a@claude: status %d, body %s", r.status, r.body)
	}
	r = e.post(t, "/v1/embeddings", `{"model":"duo","input":"x"}`)
	if r.status != 400 || str(r.err.Code) != "model_not_supported" {
		t.Errorf("chat model: status %d, body %s", r.status, r.body)
	}
	if n := len(e.claude.Requests()); n != 0 {
		t.Errorf("the Anthropic fake got %d requests", n)
	}
	for _, line := range e.g.Logs() {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["path"] == "/v1/embeddings" && m["status"] == float64(200) {
			if m["prompt_tokens"] != float64(4) {
				t.Errorf("embeddings request line prompt_tokens %v, want 4: %s", m["prompt_tokens"], line)
			}
		}
	}
}

// TestAcceptanceImagesAreNotTextTokens sends four base64 images of about
// 1 MB each to a 1M-context vision model. Counted as text at bytes/4 they
// would need about 1.05M tokens and be refused; each image costs about
// 1,600 tokens upstream, so the request fits and is served.
func TestAcceptanceImagesAreNotTextTokens(t *testing.T) {
	claude := fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey})
	t.Cleanup(claude.Close)
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: claude, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: opus
    context_window: 1000000
    max_output_tokens: 128000
    capabilities: [streaming, stream_usage, tools, vision, json_mode, json_schema]
    deployments:
      - {provider: claude, upstream_model: claude-up, pricing: {input: "4", output: "20"}}
tenants:
  - {id: %s, keys: [{id: %s, hash: %q}]}
`, claude.BaseURL(), harness.TenantID, harness.KeyID, hash)
	g := harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	e := &acceptanceEnv{g: g, key: key, claude: claude}
	img := base64.StdEncoding.EncodeToString(make([]byte, 790_000)) // about 1.05 MB of base64 each
	var parts []string
	for range 4 {
		parts = append(parts, `{"type":"image_url","image_url":{"url":"data:image/png;base64,`+img+`"}}`)
	}
	body := `{"model":"opus","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"compare these"},` + strings.Join(parts, ",") + `]}]}`
	if len(body)/4 <= 1_000_000 {
		t.Fatalf("body of %d bytes would fit the context as text; the test needs more", len(body))
	}
	r := e.post(t, "/v1/chat/completions", body)
	if r.status != http.StatusOK {
		t.Fatalf("status %d, code %s: %s", r.status, str(r.err.Code), r.err.Message)
	}
	if n := len(claude.Requests()); n != 1 {
		t.Errorf("upstream received %d requests, want 1", n)
	}
}
