package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
	"switchyard/test/scenario"
)

// The P6 acceptance cases (docs/PLAN.md) through the gateway binary:
// tenant, key and user scopes; streaming replay parsed by the openai-go
// SDK; the semantic sets end to end; a hung cache Redis; coalescing; the
// exact hit's ledger row; and purging. The key derivation, replay and
// aggregation, and stores are unit-tested in internal/cache.

// cacheEnv is a gateway in front of one fake upstream that also embeds,
// with tenants ta (keys ka1, ka2) and tb (key kb1).
type cacheEnv struct {
	g    *harness.Gateway
	up   *fakeupstream.Server
	keys map[string]string // by key ID
	http *http.Client
}

// cacheModels is the chat model cm, which can stream and call tools, and
// the embedding model emb with dims dimensions.
func cacheModels(baseURL string, dims int) string {
	return fmt.Sprintf(`providers:
  - {id: up, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: cm
    capabilities: [streaming, stream_usage, tools, parallel_tools, sampling]
    deployments:
      - {provider: up, upstream_model: cm-up, pricing: {input: "1", output: "2"}}
  - id: emb
    type: embedding
    dimensions: %d
    deployments:
      - {provider: up, upstream_model: emb-up, pricing: {input: "0.5"}}
routing: {fallback: {retries_per_deployment: 0}}`, baseURL, dims)
}

// startCacheEnv starts a cacheEnv whose default policy has the cache
// block policy; extra holds further top-level sections (storage,
// semantic_cache, cache), and embed, when set, embeds for the fake.
func startCacheEnv(t *testing.T, policy, extra string, dims int, embed func(string) []float32) *cacheEnv {
	t.Helper()
	e := &cacheEnv{up: fakeupstream.New(fakeupstream.Options{APIKey: providerKey, Embed: embed}), keys: map[string]string{}}
	t.Cleanup(e.up.Close)
	hashes := map[string]string{}
	for _, id := range []string{"ka1", "ka2", "kb1"} {
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		e.keys[id], hashes[id] = key, hash
	}
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
%s
%s
policies:
  - id: default
    cache: %s
tenants:
  - id: ta
    keys: [{id: ka1, hash: %q}, {id: ka2, hash: %q}]
  - id: tb
    keys: [{id: kb1, hash: %q}]
`, cacheModels(e.up.BaseURL(), dims), extra, policy, hashes["ka1"], hashes["ka2"], hashes["kb1"])
	e.g = harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	tr := &http.Transport{MaxIdleConnsPerHost: 200, MaxConnsPerHost: 0}
	t.Cleanup(tr.CloseIdleConnections)
	e.http = &http.Client{Transport: tr, Timeout: callTimeout}
	return e
}

// cacheReply is one gateway response.
type cacheReply struct {
	status  int
	header  http.Header
	body    string
	elapsed time.Duration
}

// cache returns the response's X-Switchyard-Cache.
func (r cacheReply) cache() string { return r.header.Get("X-Switchyard-Cache") }

// post sends a chat completion as key ID kid with the header pairs.
func (e *cacheEnv) post(t testing.TB, kid, body string, header ...string) cacheReply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.keys[kid])
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	start := time.Now()
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return cacheReply{status: resp.StatusCode, header: resp.Header, body: string(b), elapsed: time.Since(start)}
}

// metric sums the samples of name whose labels include all of labels.
func (e *cacheEnv) metric(t testing.TB, name string, labels ...string) float64 {
	t.Helper()
	resp, err := e.http.Get(e.g.AdminURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sum float64
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		ok := true
		for _, l := range labels {
			ok = ok && strings.Contains(line, l)
		}
		if !ok {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("metric line %q: %v", line, err)
		}
		sum += v
	}
	return sum
}

// waitMetric waits until metric name (with labels) reaches n.
func (e *cacheEnv) waitMetric(t testing.TB, n float64, name string, labels ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := e.metric(t, name, labels...)
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s%v = %v, want %v", name, labels, got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitWrites waits until n writes to layer have succeeded.
func (e *cacheEnv) waitWrites(t testing.TB, layer string, n float64) {
	t.Helper()
	e.waitMetric(t, n, "switchyard_cache_writes_total", `layer="`+layer+`"`, `result="ok"`)
}

// chatCalls counts the chat completion requests the upstream received.
func (e *cacheEnv) chatCalls() int {
	n := 0
	for _, r := range e.up.Requests() {
		if strings.HasSuffix(r.Path, "/chat/completions") {
			n++
		}
	}
	return n
}

// TestCacheAcceptanceKeys: two tenants sending the identical request never
// share an entry; the key scope separates keys and the user scope end
// users, within a tenant. The key derivation itself (every
// output-affecting field changes the key; stream and user do not) is
// unit-tested in internal/cache (TestCanonical, TestExactKey, TestScopeID).
func TestCacheAcceptanceKeys(t *testing.T) {
	type send struct {
		kid, user, want string
	}
	tests := []struct {
		name  string
		scope string
		sends []send
	}{
		{"tenant", "tenant", []send{
			{"ka1", "", "miss"}, {"ka1", "", "exact"}, {"ka2", "", "exact"},
			{"kb1", "", "miss"}, {"kb1", "", "exact"},
		}},
		{"key", "key", []send{
			{"ka1", "", "miss"}, {"ka2", "", "miss"}, {"ka1", "", "exact"}, {"ka2", "", "exact"}, {"kb1", "", "miss"},
		}},
		{"user", "user", []send{
			{"ka1", "u1", "miss"}, {"ka2", "u1", "exact"}, {"ka1", "u2", "miss"}, {"kb1", "u1", "miss"},
			// Without a user field the user scope falls back to the key.
			{"ka1", "", "miss"}, {"ka2", "", "miss"}, {"ka1", "", "exact"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := startCacheEnv(t, `{exact: {enabled: true, scope: `+tt.scope+`}}`, "", 4, nil)
			misses := 0
			for i, s := range tt.sends {
				user := ""
				if s.user != "" {
					user = `"user":"` + s.user + `",`
				}
				r := e.post(t, s.kid, `{"model":"cm",`+user+`"messages":[{"role":"user","content":"Say hello."}]}`)
				if r.status != http.StatusOK || r.cache() != s.want {
					t.Fatalf("request %d (%s, user %q): %d %q, want %q", i, s.kid, s.user, r.status, r.cache(), s.want)
				}
				if s.want == "miss" {
					misses++
					e.waitWrites(t, "exact", float64(misses))
				}
			}
			if got := e.chatCalls(); got != misses {
				t.Errorf("%d upstream calls, want %d (one per miss)", got, misses)
			}
		})
	}
}

// sdkStream streams params through the gateway with the openai-go SDK and
// returns the accumulated answer and the response headers.
func sdkStream(t *testing.T, e *cacheEnv, kid string, params openai.ChatCompletionNewParams) (answer, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	c := openai.NewClient(option.WithBaseURL(e.g.PublicURL+"/v1"), option.WithAPIKey(e.keys[kid]), option.WithMaxRetries(0))
	var resp *http.Response
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	st := c.Chat.Completions.NewStreaming(ctx, params, option.WithResponseInto(&resp))
	defer st.Close()
	var acc openai.ChatCompletionAccumulator
	for st.Next() {
		if !acc.AddChunk(st.Current()) {
			t.Fatalf("the SDK accumulator rejected a chunk: %s", st.Current().RawJSON())
		}
	}
	if err := st.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(acc.Choices) != 1 {
		t.Fatalf("%d choices", len(acc.Choices))
	}
	if acc.Usage.TotalTokens == 0 {
		t.Errorf("the stream reported no usage")
	}
	return toAnswer(acc.Choices[0].Message, acc.Choices[0].FinishReason), resp.Header
}

// sdkChat sends params without streaming and returns the answer.
func sdkChat(t *testing.T, e *cacheEnv, kid string, params openai.ChatCompletionNewParams) (answer, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	c := openai.NewClient(option.WithBaseURL(e.g.PublicURL+"/v1"), option.WithAPIKey(e.keys[kid]), option.WithMaxRetries(0))
	var resp *http.Response
	out, err := c.Chat.Completions.New(ctx, params, option.WithResponseInto(&resp))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return toAnswer(out.Choices[0].Message, out.Choices[0].FinishReason), resp.Header
}

// TestCacheAcceptanceStreamingReplay: a streamed miss is aggregated and
// stored; the next identical request, streamed, is an exact hit replayed
// as a stream that the openai-go SDK's ChatCompletionAccumulator folds
// into the same final message, for content and for parallel tool calls.
// A non-streaming miss followed by a streaming hit replays the same
// message too.
func TestCacheAcceptanceStreamingReplay(t *testing.T) {
	content := openai.ChatCompletionNewParams{
		Model:    "cm",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Tell me something.")},
	}
	tools := content
	tools.Messages = []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Weather in Paris and Rome?")}
	tools.Tools = []openai.ChatCompletionToolUnionParam{weatherTool("get_weather"), weatherTool("get_time")}
	tests := []struct {
		name        string
		params      openai.ChatCompletionNewParams
		firstStream bool
	}{
		{"content, stream then stream", content, true},
		{"tool calls, stream then stream", tools, true},
		{"content, non-stream then stream", content, false},
		{"tool calls, non-stream then stream", tools, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := startCacheEnv(t, `{exact: {enabled: true}}`, "", 4, nil)
			var orig answer
			var h http.Header
			if tt.firstStream {
				orig, h = sdkStream(t, e, "ka1", tt.params)
			} else {
				orig, h = sdkChat(t, e, "ka1", tt.params)
			}
			if h.Get("X-Switchyard-Cache") != "miss" {
				t.Fatalf("first: X-Switchyard-Cache %q", h.Get("X-Switchyard-Cache"))
			}
			if (len(tt.params.Tools) > 0) != (len(orig.ToolCalls) == 2) {
				t.Fatalf("first answer %+v", orig)
			}
			e.waitWrites(t, "exact", 1)
			replay, h := sdkStream(t, e, "ka1", tt.params)
			if h.Get("X-Switchyard-Cache") != "exact" || h.Get("X-Switchyard-Attempts") != "0" || h.Get("X-Switchyard-Cost-Usd") != "" {
				t.Errorf("replay headers: cache %q attempts %q cost %q", h.Get("X-Switchyard-Cache"), h.Get("X-Switchyard-Attempts"), h.Get("X-Switchyard-Cost-Usd"))
			}
			if !equalAnswers(orig, replay) {
				t.Errorf("replay %+v, want %+v", replay, orig)
			}
			if calls := e.chatCalls(); calls != 1 {
				t.Errorf("%d upstream calls, want 1", calls)
			}
			t.Logf("original and replay agree: content %q, %d tool calls, finish %s", orig.Content, len(orig.ToolCalls), orig.Finish)
		})
	}
}

// equalAnswers compares two answers field by field.
func equalAnswers(a, b answer) bool {
	return a.Content == b.Content && a.Finish == b.Finish && slices.Equal(a.ToolCalls, b.ToolCalls)
}

// semanticPair is one pair of a test/cache set.
type semanticPair struct{ A, B string }

// loadPairs reads the first n pairs of a test/cache set.
func loadPairs(t *testing.T, name string, n int) []semanticPair {
	t.Helper()
	f, err := os.Open("../cache/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []semanticPair
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(out) < n {
		var p struct{ A, B string }
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, semanticPair{p.A, p.B})
	}
	return out
}

// pairEmbedder scripts the fake embedding model for pairs: pair i's first
// wording is the unit vector u_i, its second u_i·s + w_i·√(1−s²), where s
// is 0.97 for paraphrases and 0.85 for near misses, and the u and w
// vectors are all orthogonal. A first wording shared by two pairs gets
// one u.
type pairEmbedder struct {
	dims int
	vecs map[string][]float32
}

func newPairEmbedder(paraphrases, nearMisses []semanticPair) *pairEmbedder {
	pe := &pairEmbedder{vecs: map[string][]float32{}}
	type second struct {
		text string
		sim  float64
		u    int
	}
	var seconds []second
	base := map[string]int{}
	add := func(pairs []semanticPair, sim float64) {
		for _, p := range pairs {
			u, ok := base[p.A]
			if !ok {
				u = len(base)
				base[p.A] = u
			}
			seconds = append(seconds, second{p.B, sim, u})
		}
	}
	add(paraphrases, 0.97)
	add(nearMisses, 0.85)
	pe.dims = len(base) + len(seconds) + 1
	for text, u := range base {
		v := make([]float32, pe.dims)
		v[u] = 1
		pe.vecs[text] = v
	}
	for i, s := range seconds {
		v := make([]float32, pe.dims)
		v[s.u] = float32(s.sim)
		v[len(base)+i] = float32(math.Sqrt(1 - s.sim*s.sim))
		pe.vecs[s.text] = v
	}
	return pe
}

// embed returns text's vector; any other text is orthogonal to all.
func (pe *pairEmbedder) embed(text string) []float32 {
	if v, ok := pe.vecs[text]; ok {
		return v
	}
	v := make([]float32, pe.dims)
	v[pe.dims-1] = 1
	return v
}

// TestCacheAcceptanceSemantic runs pairs of the committed paraphrase and
// near-miss sets (test/cache) end to end, with the fake upstream's
// scripted embeddings at 0.97 and 0.85 similarity: each pair's first
// wording is a miss and is stored; a paraphrase's second wording is a
// semantic hit at the default threshold (0.95), and a near miss's is a
// miss. The similarity histogram sees both, and every embedding is
// charged as overhead. This proves the mechanics; `make semantic-eval`
// runs the sets against a real embedding model.
func TestCacheAcceptanceSemantic(t *testing.T) {
	paraphrases := loadPairs(t, "paraphrase.jsonl", 6)
	nearMisses := loadPairs(t, "nearmiss.jsonl", 6)
	pe := newPairEmbedder(paraphrases, nearMisses)
	e := startCacheEnv(t, `{exact: {enabled: true}, semantic: {enabled: true}}`, "semantic_cache: {embedding_model: emb}", pe.dims, pe.embed)
	ask := func(text string) string {
		b, _ := json.Marshal(text)
		return `{"model":"cm","messages":[{"role":"user","content":` + string(b) + `}]}`
	}
	stored := map[string]bool{}
	writes := 0
	run := func(pairs []semanticPair, want string) {
		for _, p := range pairs {
			if !stored[p.A] {
				if r := e.post(t, "ka1", ask(p.A)); r.status != http.StatusOK || r.cache() != "miss" {
					t.Fatalf("%q: %d %q, want miss", p.A, r.status, r.cache())
				}
				stored[p.A] = true
				writes++
				e.waitWrites(t, "semantic", float64(writes))
			}
			r := e.post(t, "ka1", ask(p.B), "X-Switchyard-Cache", "no-store")
			if r.status != http.StatusOK || r.cache() != want {
				t.Errorf("%q after %q: %d %q, want %s", p.B, p.A, r.status, r.cache(), want)
			}
		}
	}
	run(paraphrases, "semantic")
	run(nearMisses, "miss")
	// The histogram has the paraphrases' hits at 0.97, and the near
	// misses at 0.85, each a little above in float32 (the first wordings'
	// lookups miss at 0).
	hits := e.metric(t, "switchyard_cache_similarity_bucket", `outcome="hit"`, `le="0.98"`) -
		e.metric(t, "switchyard_cache_similarity_bucket", `outcome="hit"`, `le="0.96"`)
	near := e.metric(t, "switchyard_cache_similarity_bucket", `outcome="miss"`, `le="0.88"`) -
		e.metric(t, "switchyard_cache_similarity_bucket", `outcome="miss"`, `le="0.8"`)
	if int(hits) != len(paraphrases) || int(near) != len(nearMisses) {
		t.Errorf("similarity histogram: %v hits in (0.96, 0.98], %v misses in (0.8, 0.88]; want %d and %d", hits, near, len(paraphrases), len(nearMisses))
	}
	t.Logf("%d paraphrases hit, %d near misses missed; %d upstream chat calls", len(paraphrases), len(nearMisses), e.chatCalls())
}

// hungListener accepts TCP connections and never answers on them, like a
// Redis that has stopped responding.
func hungListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// latencies summarizes request latencies.
type latencies []time.Duration

func (l latencies) p50() time.Duration {
	s := slices.Clone(l)
	slices.Sort(s)
	return s[len(s)/2]
}

func (l latencies) max() time.Duration { return slices.Max(l) }

// TestCacheAcceptanceRedisHung: with the cache Redis a TCP listener that
// accepts and never answers (the state Redis, for namespace versions, is
// up), every request succeeds, each lookup is a counted error, and the
// added latency stays within cache.lookup_timeout plus a margin, against
// the same setup with caching off. test/integration stops a real cache
// Redis mid-run.
func TestCacheAcceptanceRedisHung(t *testing.T) {
	const (
		n       = 100
		timeout = 25 * time.Millisecond
		// margin covers scheduling and the race detector.
		margin = 40 * time.Millisecond
	)
	state := miniredis.RunT(t)
	storage := fmt.Sprintf("storage: {mode: redis, redis: {cache_url: \"redis://%s/0\", state_url: \"redis://%s/0\"}}\ncache: {lookup_timeout: %s}",
		hungListener(t), state.Addr(), timeout)
	measure := func(policy string) (latencies, *cacheEnv) {
		e := startCacheEnv(t, policy, storage, 4, nil)
		var l latencies
		for i := range n {
			r := e.post(t, "ka1", fmt.Sprintf(`{"model":"cm","messages":[{"role":"user","content":"question %d"}]}`, i))
			if r.status != http.StatusOK {
				t.Fatalf("request %d: %d %s", i, r.status, r.body)
			}
			l = append(l, r.elapsed)
		}
		return l, e
	}
	off, _ := measure(`{exact: {enabled: false}}`)
	on, e := measure(`{exact: {enabled: true}}`)
	if got := e.metric(t, "switchyard_cache_lookups_total", `layer="exact"`, `result="error"`); got != n {
		t.Errorf(`cache_lookups_total{layer="exact",result="error"} = %v, want %d`, got, n)
	}
	if got := e.metric(t, "switchyard_cache_lookups_total", `layer="exact"`, `result="hit"`); got != 0 {
		t.Errorf("%v hits from a hung Redis", got)
	}
	addedP50, addedMax := on.p50()-off.p50(), on.max()-off.max()
	t.Logf("hung cache Redis, %d sequential requests, lookup_timeout %s: caching off p50 %s max %s; caching on p50 %s max %s; added p50 %s max %s",
		n, timeout, off.p50(), off.max(), on.p50(), on.max(), addedP50, addedMax)
	if addedP50 > timeout+margin || on.max() > off.max()+timeout+margin+100*time.Millisecond {
		t.Errorf("added latency p50 %s, max %s: more than the lookup timeout %s plus margin", addedP50, addedMax, timeout)
	}
	if on.p50() < timeout {
		t.Errorf("caching on p50 %s is below the lookup timeout: was the hung Redis consulted?", on.p50())
	}
}

// TestCacheAcceptanceCoalescing: 100 identical concurrent non-streaming
// requests make exactly 1 upstream call. The upstream holds the first
// until the other 99 wait on it (switchyard_cache_coalesce_waiting); then
// every request gets 200 with the same content, and 99 are served as
// exact hits.
func TestCacheAcceptanceCoalescing(t *testing.T) {
	const n = 100
	e := startCacheEnv(t, `{exact: {enabled: true}}`, "", 4, nil)
	release := make(chan struct{})
	e.up.Script(fakeupstream.Behavior{Hold: release})
	body := `{"model":"cm","messages":[{"role":"user","content":"Coalesce me."}]}`
	replies := make([]cacheReply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { replies[i] = e.post(t, "ka1", body) })
	}
	// The leader is upstream, and every other request has joined it: a
	// follower is counted waiting only once it has, so none can miss the
	// release and lead a second call.
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := e.up.WaitReceived(ctx, 1); err != nil {
		t.Fatal(err)
	}
	e.waitMetric(t, n-1, "switchyard_cache_coalesce_waiting")
	close(release)
	wg.Wait()
	exact := 0
	for i, r := range replies {
		if r.status != http.StatusOK || r.body != replies[0].body {
			t.Fatalf("reply %d: %d, same body %v", i, r.status, r.body == replies[0].body)
		}
		if r.cache() == "exact" {
			exact++
		}
	}
	if calls := e.chatCalls(); calls != 1 || exact != n-1 {
		t.Errorf("%d upstream calls, %d exact hits; want 1 and %d", calls, exact, n-1)
	}
	if got := e.metric(t, "switchyard_cache_coalesced_total"); got != n-1 {
		t.Errorf("switchyard_cache_coalesced_total = %v, want %d", got, n-1)
	}
	t.Logf("%d concurrent requests: %d upstream call, %d coalesced exact hits", n, e.chatCalls(), exact)
}

// TestCacheAcceptanceLedger: an exact hit costs 0 in the ledger, with
// cache_status exact and its baseline cost as cache savings, and leaves
// routing savings alone, through both usage APIs (memory ledger).
// test/integration runs it again with the Postgres ledger and Redis.
func TestCacheAcceptanceLedger(t *testing.T) {
	scenario.CacheLedger(t, scenario.Options{Race: raceEnabled})
}

// TestCacheAcceptancePurge: purging tenant A through the admin API makes
// its exact and semantic entries miss, while tenant B's still hit
// (memory storage). test/integration runs it again with Redis.
func TestCacheAcceptancePurge(t *testing.T) {
	scenario.CachePurge(t, scenario.Options{Race: raceEnabled})
}
