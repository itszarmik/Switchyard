package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"switchyard/test/fakegemini"
	"switchyard/test/fakeupstream"
)

// The catalog the gateway serves, and the credentials on each side of it.
const (
	// catalogModel is the model clients request. Directly against the fake
	// it is also what the fake answers as.
	catalogModel = "conformance-model"
	// upstreamModel is the deployment's upstream_model: what the gateway
	// sends upstream, and so the model a gateway response names.
	upstreamModel = "conformance-upstream-model"
	// providerKey is the provider credential. The fake requires it, so
	// clients use it directly and the gateway uses it upstream.
	providerKey = "sk-fake-provider-key-0123456789"
	// wrongKey is a well-formed gateway key that is not configured.
	wrongKey = "sy_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	// prompt is the user message of most requests. It must never be logged.
	prompt = "conformance prompt 7f3a9c: say something"

	// forwardedHeader is in the provider's forward_headers; blockedHeader
	// is not.
	forwardedHeader = "X-Conformance-Forwarded"
	blockedHeader   = "X-Conformance-Blocked"

	// callTimeout bounds every SDK call. It is generous so a slow,
	// race-instrumented machine does not make the suite flaky.
	callTimeout = 20 * time.Second
	// receivedTimeout bounds the wait for an upstream request that must
	// already have arrived.
	receivedTimeout = 2 * time.Second
)

// upstreamFake is the method set of *fakeupstream.Server the suite uses.
// The OpenAI views of fakeanthropic and fakegemini have it too, so the
// suite can run against a gateway whose deployment is Anthropic- or
// Gemini-backed.
type upstreamFake interface {
	BaseURL() string
	Reset()
	Script(fakeupstream.Behavior)
	Requests() []fakeupstream.Request
	WaitReceived(ctx context.Context, n int) error
	Await(ctx context.Context, n int) ([]fakeupstream.Request, error)
}

// Upstream wire protocols the suite runs against.
const (
	wireOpenAI    = "openai"
	wireAnthropic = "anthropic"
	wireGemini    = "gemini"
)

// suite runs the conformance cases against one base URL.
type suite struct {
	baseURL  string
	apiKey   string
	upstream upstreamFake
	// anthropic is set when the gateway's deployment is Anthropic-backed:
	// the upstream receives translated Messages API requests.
	anthropic bool
	// gemini is set when the gateway's deployment is Gemini-backed: the
	// upstream receives translated generateContent requests, and the
	// tool-call ids the client sees carry Gemini's ids (see geminiCallID).
	gemini bool
	// gateway is set when baseURL is the gateway rather than the fake.
	// Assertions about gateway headers, forwarding, 404 model_not_found,
	// auth and error mapping apply only then.
	gateway bool
	http    *http.Client
}

// runSuite drives the official SDK against baseURL with apiKey. upstream is
// the fake behind it: baseURL is either upstream.BaseURL() itself or a
// gateway whose only provider is upstream. wire is the upstream's protocol;
// with wireAnthropic or wireGemini, baseURL must be a gateway.
//
// Against Anthropic and Gemini, the cases run unchanged except where the
// protocol makes the OpenAI expectation impossible; each such case says
// why: n > 1 is answered by the gateway (no Anthropic or Gemini deployment
// has n), an upstream 400's code and param have no counterpart, the
// upstream body is a translation (so it is not compared byte for byte),
// and unknown request and response fields cannot cross the translation.
// Against Gemini, tool-call ids are compared by the Gemini id they carry,
// and parallel_tool_calls: false is not honored (the native API has no
// such switch).
func runSuite(t *testing.T, baseURL, apiKey string, upstream upstreamFake, wire string) {
	transport := &http.Transport{MaxIdleConnsPerHost: 4}
	t.Cleanup(transport.CloseIdleConnections)
	s := &suite{
		baseURL:   baseURL,
		apiKey:    apiKey,
		upstream:  upstream,
		anthropic: wire == wireAnthropic,
		gemini:    wire == wireGemini,
		gateway:   strings.TrimSuffix(baseURL, "/") != upstream.BaseURL(),
		http:      &http.Client{Transport: transport},
	}
	if s.translated() && !s.gateway {
		t.Fatal("the Anthropic and Gemini upstreams can only be reached through the gateway")
	}
	t.Run("plain chat", s.testPlainChat)
	t.Run("streaming is incremental", s.testStreamingIncremental)
	t.Run("tool calls", s.testToolCalls)
	t.Run("multi-turn tool calls", s.testMultiTurnTools)
	t.Run("json mode", s.testJSONMode)
	t.Run("n greater than 1", s.testN)
	t.Run("stop sequences", s.testStop)
	t.Run("include_usage", s.testIncludeUsage)
	t.Run("errors", s.testErrors)
	t.Run("error after the stream started", s.testMidStreamError)
	t.Run("unknown request fields", s.testUnknownFields)
	t.Run("unknown response fields", s.testUnknownResponseFields)
	t.Run("models", s.testModels)
}

// translated reports whether the upstream speaks another protocol than
// OpenAI's, so requests and responses are translations.
func (s *suite) translated() bool { return s.anthropic || s.gemini }

// exchange captures one HTTP exchange of the SDK through its middleware.
type exchange struct {
	mu        sync.Mutex
	reqBody   []byte
	reqHeader http.Header
	status    int
	header    http.Header
	respBody  []byte // nil for an event stream, which is left to the SDK
}

func (e *exchange) middleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	e.mu.Lock()
	e.reqBody, e.reqHeader = body, req.Header.Clone()
	e.mu.Unlock()

	resp, err := next(req)
	if err != nil {
		return resp, err
	}
	var respBody []byte
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		respBody, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	e.mu.Lock()
	e.status, e.header, e.respBody = resp.StatusCode, resp.Header.Clone(), respBody
	e.mu.Unlock()
	return resp, nil
}

// client returns an SDK client for the suite's base URL that records into
// ex. It never retries, so every SDK call is exactly one HTTP request.
func (s *suite) client(ex *exchange, apiKey string) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(s.baseURL),
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
		option.WithHTTPClient(s.http),
		option.WithMiddleware(ex.middleware),
	)
}

// answer is one choice, normalized from a completion or an accumulated
// stream.
type answer struct {
	Content   string
	ToolCalls []toolCall
	Finish    string
}

type toolCall struct{ ID, Name, Args string }

// result is the SDK's view of one call.
type result struct {
	err     error
	id      string
	model   string
	answers []answer
	// usage is nil when the SDK saw no usage.
	usage *openai.CompletionUsage
	// Stream only: chunks received, chunks carrying usage, whether the
	// usage chunk was the last one, the tool calls the accumulator reported
	// finished, and when each chunk arrived.
	chunks, usageChunks, finishedToolCalls int
	usageChunkLast                         bool
	chunkTimes                             []time.Time
	rawChunks                              []string
	ex                                     *exchange
}

// params returns a request for catalogModel with the suite's prompt.
func params() openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model: catalogModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a conformance test."),
			openai.UserMessage(prompt),
		},
	}
}

// call sends p, streaming or not, and normalizes what the SDK returned.
func (s *suite) call(t *testing.T, p openai.ChatCompletionNewParams, stream bool, opts ...option.RequestOption) result {
	t.Helper()
	return s.callWithKey(t, s.apiKey, p, stream, opts...)
}

func (s *suite) callWithKey(t *testing.T, apiKey string, p openai.ChatCompletionNewParams, stream bool, opts ...option.RequestOption) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	r := result{ex: &exchange{}}
	c := s.client(r.ex, apiKey)

	if !stream {
		resp, err := c.Chat.Completions.New(ctx, p, opts...)
		if err != nil {
			r.err = err
			return r
		}
		r.id, r.model = resp.ID, resp.Model
		for i, ch := range resp.Choices {
			if ch.Index != int64(i) {
				t.Errorf("choice %d has index %d", i, ch.Index)
			}
			r.answers = append(r.answers, toAnswer(ch.Message, ch.FinishReason))
		}
		if resp.JSON.Usage.Valid() {
			r.usage = &resp.Usage
		}
		return r
	}

	st := c.Chat.Completions.NewStreaming(ctx, p, opts...)
	defer st.Close()
	var acc openai.ChatCompletionAccumulator
	for st.Next() {
		ch := st.Current()
		r.chunkTimes = append(r.chunkTimes, time.Now())
		r.rawChunks = append(r.rawChunks, ch.RawJSON())
		r.chunks++
		if !acc.AddChunk(ch) {
			t.Errorf("the SDK accumulator rejected chunk %d: %s", r.chunks, ch.RawJSON())
		}
		if _, ok := acc.JustFinishedToolCall(); ok {
			r.finishedToolCalls++
		}
		r.usageChunkLast = false
		if ch.JSON.Usage.Valid() {
			r.usageChunks++
			r.usageChunkLast = true
			if len(ch.Choices) != 0 {
				t.Errorf("usage chunk %d has %d choices, want none", r.chunks, len(ch.Choices))
			}
		}
		if ch.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d has object %q", r.chunks, ch.Object)
		}
		r.id, r.model = ch.ID, ch.Model
	}
	r.err = st.Err()
	for i, ch := range acc.Choices {
		if ch.Index != int64(i) {
			t.Errorf("accumulated choice %d has index %d", i, ch.Index)
		}
		r.answers = append(r.answers, toAnswer(ch.Message, ch.FinishReason))
	}
	if r.usageChunks > 0 {
		r.usage = &acc.Usage
	}
	return r
}

func toAnswer(m openai.ChatCompletionMessage, finish string) answer {
	a := answer{Content: m.Content, Finish: finish}
	for _, tc := range m.ToolCalls {
		a.ToolCalls = append(a.ToolCalls, toolCall{tc.ID, tc.Function.Name, tc.Function.Arguments})
	}
	return a
}

// wantModel is the model a response names: the fake echoes what it
// received, and the gateway sends it the upstream model.
func (s *suite) wantModel() string {
	if s.gateway {
		return upstreamModel
	}
	return catalogModel
}

// upstreamRequests waits for the n chat requests the upstream must have
// received for the last call, and fails the test if it received more.
func (s *suite) upstreamRequests(t *testing.T, n int) []fakeupstream.Request {
	t.Helper()
	// The client already has its response, which the upstream request
	// precedes, so the request must have been received: fail fast if not.
	// Only the end of its handler may still be racing with this check.
	recvCtx, cancelRecv := context.WithTimeout(context.Background(), receivedTimeout)
	defer cancelRecv()
	if err := s.upstream.WaitReceived(recvCtx, n); err != nil {
		t.Fatalf("upstream received %d requests, want %d", len(s.upstream.Requests()), n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	reqs, err := s.upstream.Await(ctx, n)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	if all := s.upstream.Requests(); len(all) != n {
		t.Fatalf("upstream received %d requests, want %d", len(all), n)
	}
	return reqs
}

// checkSuccess checks a successful call: what the SDK saw, the response
// headers, and what reached the upstream.
func (s *suite) checkSuccess(t *testing.T, r result, stream bool, want []answer) fakeupstream.Request {
	t.Helper()
	if r.err != nil {
		t.Fatalf("call failed: %v", describe(r.err))
	}
	if r.model != s.wantModel() || !strings.HasPrefix(r.id, "chatcmpl-") {
		t.Errorf("response model %q, id %q, want model %q", r.model, r.id, s.wantModel())
	}
	got := r.answers
	if s.gemini {
		got = geminiAnswers(t, got)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("answers (-want +got):\n%s", diff)
	}
	up := s.upstreamRequests(t, 1)[0]
	s.checkForwarded(t, r.ex, up, stream)

	h := r.ex.header
	if stream {
		if r.chunks < 2 {
			t.Errorf("stream had %d chunks, want several", r.chunks)
		}
		if !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
			t.Errorf("stream Content-Type %q", h.Get("Content-Type"))
		}
	} else {
		if !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type %q, want application/json", h.Get("Content-Type"))
		}
		// Golden: the body is the upstream's, byte for byte. A translated
		// Anthropic or Gemini answer is covered by the adapter's golden
		// tests.
		if !s.translated() && !bytes.Equal(r.ex.respBody, up.ResponseBody) {
			t.Errorf("response body differs from the upstream's:\n got %s\nwant %s", r.ex.respBody, up.ResponseBody)
		}
	}
	if !s.gateway {
		return up
	}
	for name, want := range map[string]string{
		"X-Switchyard-Model":               catalogModel,
		"X-Switchyard-Provider":            "fake",
		"X-Switchyard-Upstream-Request-Id": up.RequestID,
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("X-Request-Id") == "" || h.Get("X-Request-Id") == up.RequestID {
		t.Errorf("X-Request-Id = %q, want the gateway's own request ID", h.Get("X-Request-Id"))
	}
	if stream {
		for name, want := range map[string]string{"Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
			if got := h.Get(name); got != want {
				t.Errorf("stream %s = %q, want %q", name, got, want)
			}
		}
	}
	return up
}

// checkForwarded checks what reached the upstream against what the SDK
// sent. Directly, the fake gets the SDK's exact bytes. Through the gateway,
// every field arrives semantically unchanged except model, which becomes the
// upstream model, and, for a stream, stream_options, which gains
// include_usage: true and keeps its other keys. Authorization carries the
// provider key, never the client's.
func (s *suite) checkForwarded(t *testing.T, ex *exchange, up fakeupstream.Request, stream bool) {
	t.Helper()
	if s.anthropic {
		s.checkTranslated(t, up, stream)
		return
	}
	if s.gemini {
		s.checkTranslatedGemini(t, up, stream)
		return
	}
	if got := up.Header.Get("Authorization"); got != "Bearer "+providerKey {
		t.Errorf("upstream Authorization %q, want the provider key", redactAuth(got))
	}
	if !s.gateway {
		if !bytes.Equal(up.Body, ex.reqBody) {
			t.Errorf("upstream body differs from the SDK's:\n got %s\nwant %s", up.Body, ex.reqBody)
		}
		return
	}
	for name, values := range up.Header {
		for _, v := range values {
			if strings.Contains(v, s.apiKey) {
				t.Errorf("upstream header %s carries the client's key", name)
			}
		}
	}

	sent, got := decodeObject(t, ex.reqBody), decodeObject(t, up.Body)
	var model string
	if err := json.Unmarshal(got["model"], &model); err != nil || model != upstreamModel {
		t.Errorf("upstream model %s, want %q", got["model"], upstreamModel)
	}
	for k, v := range sent {
		if k == "model" || (stream && k == "stream_options") {
			continue
		}
		if !jsonEqual(t, v, got[k]) {
			t.Errorf("upstream field %s = %s, want %s", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := sent[k]; !ok && (!stream || k != "stream_options") {
			t.Errorf("upstream body gained field %s = %s", k, got[k])
		}
	}
	if !stream {
		return
	}
	var sentOpts, gotOpts map[string]json.RawMessage
	if raw, ok := sent["stream_options"]; ok {
		if err := json.Unmarshal(raw, &sentOpts); err != nil {
			t.Fatalf("SDK stream_options %s: %v", raw, err)
		}
	}
	if err := json.Unmarshal(got["stream_options"], &gotOpts); err != nil {
		t.Fatalf("upstream stream_options %s: %v", got["stream_options"], err)
	}
	if string(gotOpts["include_usage"]) != "true" {
		t.Errorf("upstream stream_options %s, want include_usage true", got["stream_options"])
	}
	for k, v := range sentOpts {
		if k != "include_usage" && !jsonEqual(t, v, gotOpts[k]) {
			t.Errorf("upstream stream_options.%s = %s, want %s", k, gotOpts[k], v)
		}
	}
}

// checkTranslated checks what reached an Anthropic upstream: the provider
// key in X-Api-Key, never an Authorization header or the client's key, and
// a Messages request for the upstream model. The translation itself is
// covered by the adapter's golden tests.
func (s *suite) checkTranslated(t *testing.T, up fakeupstream.Request, stream bool) {
	t.Helper()
	if got := up.Header.Get("X-Api-Key"); got != providerKey {
		t.Errorf("upstream X-Api-Key %q, want the provider key", redactAuth(got))
	}
	if got := up.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream Authorization %q, want none", redactAuth(got))
	}
	for name, values := range up.Header {
		for _, v := range values {
			if strings.Contains(v, s.apiKey) {
				t.Errorf("upstream header %s carries the client's key", name)
			}
		}
	}
	got := decodeObject(t, up.Body)
	var model string
	if err := json.Unmarshal(got["model"], &model); err != nil || model != upstreamModel {
		t.Errorf("upstream model %s, want %q", got["model"], upstreamModel)
	}
	if _, ok := got["messages"]; !ok {
		t.Errorf("upstream body has no messages: %s", up.Body)
	}
	if gotStream := string(got["stream"]) == "true"; gotStream != stream {
		t.Errorf("upstream stream %s, want %v", got["stream"], stream)
	}
}

// checkTranslatedGemini checks what reached a Gemini upstream: the
// provider key in x-goog-api-key, never in an Authorization header, the
// client's key nowhere, the upstream model's generateContent or
// streamGenerateContent method, and a body with contents and store false.
// The fake rejects a key in the URL. The translation itself is covered by
// the adapter's golden tests.
func (s *suite) checkTranslatedGemini(t *testing.T, up fakeupstream.Request, stream bool) {
	t.Helper()
	if got := up.Header.Get("X-Goog-Api-Key"); got != providerKey {
		t.Errorf("upstream x-goog-api-key %q, want the provider key", redactAuth(got))
	}
	if got := up.Header.Get("Authorization"); got != "" {
		t.Errorf("upstream Authorization %q, want none", redactAuth(got))
	}
	for name, values := range up.Header {
		for _, v := range values {
			if strings.Contains(v, s.apiKey) {
				t.Errorf("upstream header %s carries the client's key", name)
			}
		}
	}
	method := ":generateContent"
	if stream {
		method = ":streamGenerateContent"
	}
	if want := "/v1beta/models/" + upstreamModel + method; up.Path != want {
		t.Errorf("upstream path %s, want %s", up.Path, want)
	}
	got := decodeObject(t, up.Body)
	if _, ok := got["contents"]; !ok || string(got["store"]) != "false" {
		t.Errorf("upstream body has no contents or no store false: %s", up.Body)
	}
}

// geminiAnswers replaces each tool-call id with the Gemini functionCall.id
// it carries, so answers through a Gemini deployment compare with the
// fake's canned ids.
func geminiAnswers(t *testing.T, answers []answer) []answer {
	t.Helper()
	out := make([]answer, len(answers))
	for i, a := range answers {
		out[i] = a
		out[i].ToolCalls = nil
		for _, tc := range a.ToolCalls {
			tc.ID = geminiCallID(t, tc.ID)
			out[i].ToolCalls = append(out[i].ToolCalls, tc)
		}
	}
	return out
}

// geminiCallID decodes the documented id format (DESIGN §6.2): "gm1_",
// then unpadded base64url of a flags byte, a 6-byte model hash, the
// uvarint length of the Gemini id and the id. It fails the test for an id
// outside [A-Za-z0-9_-] or in another format.
func geminiCallID(t *testing.T, id string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(id, "gm1_")
	p, err := base64.RawURLEncoding.DecodeString(rest)
	if !ok || err != nil || strings.Trim(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" || len(p) < 8 {
		t.Errorf("tool-call id %q is not a Gemini adapter id", id)
		return id
	}
	n, k := binary.Uvarint(p[7:])
	if k <= 0 || uint64(len(p)-7-k) < n {
		t.Errorf("tool-call id %q has a bad length", id)
		return id
	}
	return string(p[7+k : 7+k+int(n)])
}

func decodeObject(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %s is not a JSON object: %v", b, err)
	}
	return m
}

// jsonEqual reports whether a and b are the same JSON value, comparing
// numbers by their literal text so precision loss counts as a difference.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	if b == nil {
		return false
	}
	decode := func(raw json.RawMessage) any {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("invalid JSON %s: %v", raw, err)
		}
		return v
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

// redactAuth shortens an Authorization value for a failure message.
func redactAuth(v string) string {
	if len(v) > 12 {
		return v[:12] + "…"
	}
	return v
}

// streamModes runs f once without streaming and once with it.
func streamModes(t *testing.T, f func(t *testing.T, stream bool)) {
	t.Run("non-stream", func(t *testing.T) { f(t, false) })
	t.Run("stream", func(t *testing.T) { f(t, true) })
}

// reset gives a case a clean upstream.
func (s *suite) reset(t *testing.T) {
	s.upstream.Reset()
	t.Cleanup(s.upstream.Reset)
}

func (s *suite) testPlainChat(t *testing.T) {
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		r := s.call(t, params(), stream,
			option.WithHeader(forwardedHeader, "yes"), option.WithHeader(blockedHeader, "no"),
			option.WithHeader("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"),
			option.WithHeader("Cookie", "session=client"))
		up := s.checkSuccess(t, r, stream, []answer{{Content: fakeupstream.Text, Finish: "stop"}})
		if stream {
			if r.usage != nil {
				t.Errorf("stream usage %+v without include_usage", r.usage)
			}
		} else {
			checkUsage(t, r.usage)
		}
		if got := up.Header.Get(forwardedHeader); got != "yes" {
			t.Errorf("upstream %s = %q, want yes", forwardedHeader, got)
		}
		if !s.gateway {
			return
		}
		// DESIGN §17: only forward_headers cross, and never the client's
		// credentials or trace context (tracing is off here).
		for _, h := range []string{blockedHeader, "Traceparent", "Cookie"} {
			if got := up.Header.Get(h); got != "" {
				t.Errorf("upstream got %s = %q, which is not in forward_headers", h, got)
			}
		}
		wantAuth := "Bearer " + providerKey
		if s.translated() {
			wantAuth = "" // the key goes in X-Api-Key or x-goog-api-key
		}
		if got := up.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("upstream Authorization %q, want %q: the client's is never forwarded", got, wantAuth)
		}
		for name, values := range up.Header {
			for _, v := range values {
				if strings.Contains(v, s.apiKey) {
					t.Errorf("upstream header %s carries the client's gateway key", name)
				}
			}
		}
	})
}

func checkUsage(t *testing.T, u *openai.CompletionUsage) {
	t.Helper()
	if u == nil {
		t.Fatal("no usage")
	}
	if u.PromptTokens <= 0 || u.CompletionTokens <= 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
		t.Errorf("usage prompt %d, completion %d, total %d", u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	}
}

// testStreamingIncremental checks that chunks reach the client as the
// upstream sends them, not buffered until the end.
func (s *suite) testStreamingIncremental(t *testing.T) {
	const gap = 40 * time.Millisecond
	s.reset(t)
	s.upstream.Script(fakeupstream.Behavior{ChunkDelay: gap})
	r := s.call(t, params(), true)
	s.checkSuccess(t, r, true, []answer{{Content: fakeupstream.Text, Finish: "stop"}})
	if len(r.chunkTimes) < 6 {
		t.Fatalf("%d chunks, want at least 6", len(r.chunkTimes))
	}
	// The fake spaces the chunks by gap. Buffering would deliver them all
	// at once; allow half the spread for scheduling noise.
	spread := r.chunkTimes[len(r.chunkTimes)-1].Sub(r.chunkTimes[0])
	if min := time.Duration(len(r.chunkTimes)-1) * gap / 2; spread < min {
		t.Errorf("%d chunks arrived within %s, want them spread over at least %s", len(r.chunkTimes), spread, min)
	}
}

func weatherTool(name string) openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        name,
		Description: openai.String("Looks something up."),
		Parameters: shared.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{"location": map[string]any{"type": "string"}},
			"required":   []string{"location"},
		},
	})
}

func (s *suite) testToolCalls(t *testing.T) {
	tests := []struct {
		name     string
		tools    []string
		parallel *bool
		want     []toolCall
	}{
		{
			name:  "one tool",
			tools: []string{"get_weather"},
			want:  []toolCall{{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)}},
		},
		{
			name:  "parallel tool calls",
			tools: []string{"get_weather", "get_time"},
			want: []toolCall{
				{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)},
				{fakeupstream.ToolCallID(0, 1), "get_time", fakeupstream.ToolArguments(1)},
			},
		},
		{
			name:     "parallel tool calls disabled",
			tools:    []string{"get_weather", "get_time"},
			parallel: new(bool),
			want:     []toolCall{{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)}},
		},
	}
	for _, tc := range tests {
		if s.gemini && tc.parallel != nil && !*tc.parallel {
			// Gemini has no switch for parallel calls, so the adapter sends
			// the request without one and the model may call both tools.
			tc.want = tests[1].want
		}
		t.Run(tc.name, func(t *testing.T) {
			streamModes(t, func(t *testing.T, stream bool) {
				s.reset(t)
				p := params()
				for _, name := range tc.tools {
					p.Tools = append(p.Tools, weatherTool(name))
				}
				if tc.parallel != nil {
					p.ParallelToolCalls = openai.Bool(*tc.parallel)
				}
				r := s.call(t, p, stream)
				s.checkSuccess(t, r, stream, []answer{{ToolCalls: tc.want, Finish: "tool_calls"}})
				if stream && r.finishedToolCalls != len(tc.want) {
					t.Errorf("the accumulator finished %d tool calls, want %d", r.finishedToolCalls, len(tc.want))
				}
				for _, call := range r.answers[0].ToolCalls {
					if !json.Valid([]byte(call.Args)) {
						t.Errorf("tool call %s arguments %q are not JSON", call.ID, call.Args)
					}
				}
			})
		})
	}
}

// testMultiTurnTools sends a tool-call turn's answer back as the next
// turn's history, with the tool-call ids exactly as the client got them,
// and checks the upstream accepts it. Against Gemini that proves the
// thought signatures carried in the ids come back through the gateway: the
// fake, like Google, rejects a current-turn step without the signature it
// issued.
func (s *suite) testMultiTurnTools(t *testing.T) {
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		p := params()
		p.Tools = []openai.ChatCompletionToolUnionParam{weatherTool("get_weather"), weatherTool("get_time")}
		first := s.call(t, p, stream)
		if first.err != nil || len(first.answers) != 1 || len(first.answers[0].ToolCalls) != 2 {
			t.Fatalf("turn 1: %v, answers %+v", first.err, first.answers)
		}
		var calls, results []map[string]any
		for _, tc := range first.answers[0].ToolCalls {
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Args}})
			results = append(results, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": `{"ok":true}`})
		}
		history := []any{
			map[string]any{"role": "system", "content": "You are a conformance test."},
			map[string]any{"role": "user", "content": prompt},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
		}
		for _, r := range results {
			history = append(history, r)
		}
		raw, err := json.Marshal(history)
		if err != nil {
			t.Fatal(err)
		}
		second := s.call(t, p, stream, option.WithJSONSet("messages", json.RawMessage(raw)))
		if second.err != nil {
			t.Fatalf("turn 2, with turn 1's ids: %v", describe(second.err))
		}
		reqs := s.upstreamRequests(t, 2)
		sent := string(reqs[1].Body)
		switch {
		case s.gemini:
			sig := fakegemini.Signature(upstreamModel, fakeupstream.ToolCallID(0, 0))
			if !strings.Contains(sent, `"thoughtSignature":"`+sig+`"`) || strings.Contains(sent, "skip_thought_signature_validator") {
				t.Errorf("turn 2 did not carry turn 1's thought signature: %s", sent)
			}
		case s.anthropic:
			if !strings.Contains(sent, `"tool_use_id":"`+fakeupstream.ToolCallID(0, 0)+`"`) {
				t.Errorf("turn 2 lost the tool-call id: %s", sent)
			}
		default:
			if !strings.Contains(sent, `"tool_call_id":"`+fakeupstream.ToolCallID(0, 0)+`"`) {
				t.Errorf("turn 2 lost the tool-call id: %s", sent)
			}
		}
	})
}

func (s *suite) testJSONMode(t *testing.T) {
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		p := params()
		p.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONObject: &shared.ResponseFormatJSONObjectParam{}}
		r := s.call(t, p, stream)
		s.checkSuccess(t, r, stream, []answer{{Content: fakeupstream.JSONText, Finish: "stop"}})
		if len(r.answers) == 1 && !json.Valid([]byte(r.answers[0].Content)) {
			t.Errorf("content %q is not JSON", r.answers[0].Content)
		}
	})
}

func (s *suite) testN(t *testing.T) {
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		p := params()
		p.N = openai.Int(3)
		r := s.call(t, p, stream)
		if s.translated() {
			// The Messages API and Gemini have no n: the only deployment
			// lacks the capability, so the gateway answers without the
			// upstream.
			s.checkError(t, r, apiErr{status: 400, typ: "invalid_request_error", code: "no_eligible_model", upstreamNotCalled: true})
			if !strings.Contains(describe(r.err), "lacks capability n") {
				t.Errorf("error %v does not name the missing capability", describe(r.err))
			}
			return
		}
		s.checkSuccess(t, r, stream, []answer{
			{Content: fakeupstream.ChoiceText(0), Finish: "stop"},
			{Content: fakeupstream.ChoiceText(1), Finish: "stop"},
			{Content: fakeupstream.ChoiceText(2), Finish: "stop"},
		})
	})
}

func (s *suite) testStop(t *testing.T) {
	tests := []struct {
		name string
		stop openai.ChatCompletionNewParamsStopUnion
		want string
	}{
		{"string", openai.ChatCompletionNewParamsStopUnion{OfString: openai.String("lazy")}, "The quick brown fox jumps over the "},
		{"array", openai.ChatCompletionNewParamsStopUnion{OfStringArray: []string{"dog", "fox"}}, "The quick brown "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			streamModes(t, func(t *testing.T, stream bool) {
				s.reset(t)
				p := params()
				p.Stop = tc.stop
				r := s.call(t, p, stream)
				s.checkSuccess(t, r, stream, []answer{{Content: tc.want, Finish: "stop"}})
			})
		})
	}
}

func (s *suite) testIncludeUsage(t *testing.T) {
	tests := []struct {
		name      string
		opts      openai.ChatCompletionStreamOptionsParam
		wantUsage bool
	}{
		{"absent", openai.ChatCompletionStreamOptionsParam{}, false},
		{"other stream_options only", openai.ChatCompletionStreamOptionsParam{IncludeObfuscation: openai.Bool(false)}, false},
		{"false", openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(false)}, false},
		{"true", openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true), IncludeObfuscation: openai.Bool(false)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.reset(t)
			p := params()
			p.StreamOptions = tc.opts
			r := s.call(t, p, true)
			s.checkSuccess(t, r, true, []answer{{Content: fakeupstream.Text, Finish: "stop"}})
			if !tc.wantUsage {
				if r.usageChunks != 0 {
					t.Errorf("%d usage chunks, want none: the client did not ask for usage", r.usageChunks)
				}
				return
			}
			if r.usageChunks != 1 || !r.usageChunkLast {
				t.Errorf("%d usage chunks (last: %v), want exactly one, the last", r.usageChunks, r.usageChunkLast)
			}
			checkUsage(t, r.usage)
		})
	}
}

// apiErr is what an SDK error must carry. An empty field is not checked.
type apiErr struct {
	status                int
	typ, code, param      string
	message, retryAfter   string
	transport             bool // want a transport error, not an API error
	upstreamNotCalled     bool // the gateway must answer without the upstream
	upstreamCanceled      bool // the gateway must cancel the upstream request
	bodyMustNotContain    []string
	wwwAuthenticateBearer bool
}

func (s *suite) testErrors(t *testing.T) {
	missingModel := "The model `no-such-model` does not exist or you do not have access to it."
	tests := []struct {
		name     string
		behavior *fakeupstream.Behavior
		key      string // default: the suite's key
		edit     func(*openai.ChatCompletionNewParams)
		direct   *apiErr // nil: not run directly
		gateway  *apiErr // nil: same as direct
		// anthropic, when set, replaces gateway for an Anthropic or Gemini
		// upstream.
		anthropic *apiErr
	}{
		{
			name:     "400 from the upstream",
			behavior: ptr(fakeupstream.APIError(400, "invalid_request_error", "invalid_value", "temperature", "Invalid 'temperature': decimal above maximum value.")),
			direct:   &apiErr{status: 400, typ: "invalid_request_error", code: "invalid_value", param: "temperature", message: "Invalid 'temperature': decimal above maximum value."},
			// An Anthropic or Gemini error has a message, but no code or
			// param to carry over.
			anthropic: &apiErr{status: 400, typ: "invalid_request_error", message: "Invalid 'temperature': decimal above maximum value."},
		},
		{
			name:    "400 missing messages",
			edit:    func(p *openai.ChatCompletionNewParams) { p.Messages = nil },
			direct:  &apiErr{status: 400, typ: "invalid_request_error", code: "missing_required_parameter", param: "messages"},
			gateway: &apiErr{status: 400, typ: "invalid_request_error", code: "missing_required_parameter", param: "messages", upstreamNotCalled: true},
		},
		{
			name:    "400 empty messages",
			edit:    func(p *openai.ChatCompletionNewParams) { p.Messages = []openai.ChatCompletionMessageParamUnion{} },
			direct:  &apiErr{status: 400, typ: "invalid_request_error", code: "empty_array", param: "messages"},
			gateway: &apiErr{status: 400, typ: "invalid_request_error", code: "empty_array", param: "messages", upstreamNotCalled: true},
		},
		{
			name:   "401 wrong key",
			key:    wrongKey,
			direct: &apiErr{status: 401, typ: "invalid_request_error", code: "invalid_api_key"},
			gateway: &apiErr{status: 401, typ: "invalid_request_error", code: "invalid_api_key",
				message: "Incorrect API key provided.", upstreamNotCalled: true,
				bodyMustNotContain: []string{wrongKey}, wwwAuthenticateBearer: true},
		},
		{
			name:    "404 unknown model",
			edit:    func(p *openai.ChatCompletionNewParams) { p.Model = "no-such-model" },
			direct:  &apiErr{status: 404, typ: "invalid_request_error", code: "model_not_found", message: missingModel},
			gateway: &apiErr{status: 404, typ: "invalid_request_error", code: "model_not_found", message: missingModel, upstreamNotCalled: true},
		},
		{
			name:     "429 from the upstream",
			behavior: ptr(fakeupstream.RateLimited("7")),
			direct:   &apiErr{status: 429, typ: "requests", code: "rate_limit_exceeded", retryAfter: "7"},
		},
		{
			name:     "upstream 401 is the gateway's credential problem",
			behavior: &fakeupstream.Behavior{Status: 401},
			direct:   &apiErr{status: 401, typ: "invalid_request_error", code: "invalid_api_key"},
			gateway: &apiErr{status: 502, code: "upstream_error",
				bodyMustNotContain: []string{"Incorrect API key", "sk-f", providerKey}},
		},
		{
			name:     "upstream 500",
			behavior: &fakeupstream.Behavior{Status: 500},
			direct:   &apiErr{status: 500, typ: "server_error"},
			gateway:  &apiErr{status: 502, code: "upstream_error"},
		},
		{
			name:     "upstream 503",
			behavior: &fakeupstream.Behavior{Status: 503},
			direct:   &apiErr{status: 503, typ: "server_error"},
			gateway:  &apiErr{status: 502, code: "upstream_error"},
		},
		{
			name:     "upstream connection dropped",
			behavior: &fakeupstream.Behavior{Fault: fakeupstream.FaultDrop},
			direct:   &apiErr{transport: true},
			gateway:  &apiErr{status: 502, code: "upstream_error"},
		},
		{
			name:     "upstream never answers",
			behavior: &fakeupstream.Behavior{Fault: fakeupstream.FaultHang},
			gateway:  &apiErr{status: 504, code: "timeout", upstreamCanceled: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.direct
			if s.gateway && tc.gateway != nil {
				want = tc.gateway
			}
			if s.translated() && tc.anthropic != nil {
				want = tc.anthropic
			}
			if want == nil {
				t.Skip("gateway-only case")
			}
			streamModes(t, func(t *testing.T, stream bool) {
				s.reset(t)
				if tc.behavior != nil {
					s.upstream.Script(*tc.behavior)
				}
				p := params()
				if tc.edit != nil {
					tc.edit(&p)
				}
				key := s.apiKey
				if tc.key != "" {
					key = tc.key
				}
				r := s.callWithKey(t, key, p, stream)
				s.checkError(t, r, *want)
			})
		})
	}
}

func ptr[T any](v T) *T { return &v }

// checkError checks that the SDK reported want.
func (s *suite) checkError(t *testing.T, r result, want apiErr) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("call succeeded with %d answers, want an error", len(r.answers))
	}
	var aerr *openai.Error
	isAPI := errors.As(r.err, &aerr)
	if want.transport {
		if isAPI {
			t.Errorf("got API error %d, want a transport error", aerr.StatusCode)
		}
		return
	}
	if !isAPI {
		t.Fatalf("error %v, want an API error with status %d", describe(r.err), want.status)
	}
	got := apiErr{status: aerr.StatusCode, typ: aerr.Type, code: aerr.Code, param: aerr.Param, message: aerr.Message}
	if aerr.Response != nil {
		got.retryAfter = aerr.Response.Header.Get("Retry-After")
	}
	check := func(field, got, want string) {
		if want != "" && got != want {
			t.Errorf("error %s = %q, want %q", field, got, want)
		}
	}
	if got.status != want.status {
		t.Errorf("error status %d, want %d", got.status, want.status)
	}
	check("type", got.typ, want.typ)
	check("code", got.code, want.code)
	check("param", got.param, want.param)
	check("message", got.message, want.message)
	check("Retry-After", got.retryAfter, want.retryAfter)
	if got.typ == "" || got.message == "" {
		t.Errorf("error type %q, message %q: want an OpenAI error envelope", got.typ, got.message)
	}
	for _, secret := range want.bodyMustNotContain {
		if bytes.Contains(r.ex.respBody, []byte(secret)) {
			t.Errorf("error body contains %q: %s", secret, r.ex.respBody)
		}
	}
	if want.wwwAuthenticateBearer && !strings.HasPrefix(r.ex.header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want Bearer", r.ex.header.Get("WWW-Authenticate"))
	}
	if s.gateway && r.ex.header.Get("X-Request-Id") == "" {
		t.Error("error response has no X-Request-Id")
	}
	switch {
	case want.upstreamNotCalled:
		if n := len(s.upstream.Requests()); n != 0 {
			t.Errorf("upstream received %d requests, want none", n)
		}
	case want.upstreamCanceled:
		up := s.upstreamRequests(t, 1)[0]
		if up.ClientGoneAt.IsZero() {
			t.Error("the gateway did not cancel the upstream request")
		}
	default:
		s.upstreamRequests(t, 1)
	}
}

// testMidStreamError checks a failure after the stream has started: the
// SDK sees the chunks sent so far, then an error.
func (s *suite) testMidStreamError(t *testing.T) {
	s.reset(t)
	// Against Anthropic the fault counts Messages API events, which are
	// not one to one with chunks: message_start, content_block_start, a
	// ping and two text deltas give three chunks.
	after := 3
	switch {
	case s.anthropic:
		after = 5
	case s.gemini:
		// One Gemini event per word: the first gives the role and the
		// first content chunk, the second one more.
		after = 2
	}
	s.upstream.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop, FaultAfterChunks: after})
	r := s.call(t, params(), true)
	if r.err == nil {
		t.Fatalf("stream ended cleanly after %d chunks, want an error", r.chunks)
	}
	if r.chunks != 3 {
		t.Errorf("%d chunks before the error, want 3", r.chunks)
	}
	s.upstreamRequests(t, 1)
	if !s.gateway {
		return
	}
	var serr *ssestream.StreamError
	if !errors.As(r.err, &serr) {
		t.Fatalf("error %v, want an error event in the stream", describe(r.err))
	}
	var ev struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(serr.Event.Data, &ev); err != nil {
		t.Fatalf("error event %s: %v", serr.Event.Data, err)
	}
	if ev.Error.Type != "server_error" || ev.Error.Code != "upstream_error" || ev.Error.Message == "" {
		t.Errorf("error event %s, want type server_error, code upstream_error and a message", serr.Event.Data)
	}
}

func (s *suite) testUnknownFields(t *testing.T) {
	if s.translated() {
		t.Skip("OpenAI-only: unknown request fields cannot reach an Anthropic or Gemini upstream; the adapter translates only the fields it maps")
	}
	// A big integer and an exponent survive only lossless forwarding.
	const extension = `{"nested":{"big":12345678901234567890,"tiny":1.0e-7,"list":[1,"two",null,true,{}]},"text":"caf\u00e9 <&>"}`
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		r := s.call(t, params(), stream,
			option.WithJSONSet("x_conformance_extension", json.RawMessage(extension)),
			option.WithJSONSet("x_conformance_flag", true))
		up := s.checkSuccess(t, r, stream, []answer{{Content: fakeupstream.Text, Finish: "stop"}})
		got := decodeObject(t, up.Body)
		if !jsonEqual(t, json.RawMessage(extension), got["x_conformance_extension"]) {
			t.Errorf("upstream x_conformance_extension = %s, want %s", got["x_conformance_extension"], extension)
		}
		if string(got["x_conformance_flag"]) != "true" {
			t.Errorf("upstream x_conformance_flag = %s, want true", got["x_conformance_flag"])
		}
	})
}

// testUnknownResponseFields checks the golden pass-through of response
// fields no SDK or gateway knows: a non-streaming body arrives byte for
// byte (checkSuccess compares it with what the upstream sent), and every
// stream chunk keeps them too.
func (s *suite) testUnknownResponseFields(t *testing.T) {
	if s.translated() {
		t.Skip("OpenAI-only: the adapter builds chat.completion objects from the Message or GenerateContentResponse, so unknown upstream fields are not passed through")
	}
	extra := map[string]json.RawMessage{
		// A big integer and an exponent survive only lossless forwarding.
		"x_upstream_extension": json.RawMessage(`{"big":12345678901234567890,"tiny":1.0e-7,"list":[1,"two",null,true,{}],"text":"caf\u00e9 <&>"}`),
		"x_upstream_flag":      json.RawMessage(`true`),
	}
	streamModes(t, func(t *testing.T, stream bool) {
		s.reset(t)
		s.upstream.Script(fakeupstream.Behavior{ExtraFields: extra})
		r := s.call(t, params(), stream)
		s.checkSuccess(t, r, stream, []answer{{Content: fakeupstream.Text, Finish: "stop"}})
		objects := r.rawChunks
		if !stream {
			objects = []string{string(r.ex.respBody)}
		}
		if len(objects) == 0 {
			t.Fatal("no response objects")
		}
		for i, raw := range objects {
			got := decodeObject(t, []byte(raw))
			for name, want := range extra {
				if !jsonEqual(t, want, got[name]) {
					t.Errorf("response object %d: %s = %s, want %s", i, name, got[name], want)
				}
			}
		}
	})
}

func (s *suite) testModels(t *testing.T) {
	s.reset(t)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	ex := &exchange{}
	c := s.client(ex, s.apiKey)
	page, err := c.Models.List(ctx)
	if err != nil {
		t.Fatalf("list models: %v", describe(err))
	}
	var found *openai.Model
	for i := range page.Data {
		if page.Data[i].ID == catalogModel {
			found = &page.Data[i]
		}
	}
	if found == nil || found.Object != "model" || found.Created <= 0 {
		t.Fatalf("models %s, want %s", ex.respBody, catalogModel)
	}
	if !s.gateway {
		return
	}
	if len(page.Data) != 1 || found.OwnedBy != "switchyard" {
		t.Errorf("models %s, want only %s, owned by switchyard", ex.respBody, catalogModel)
	}
	if n := len(s.upstream.Requests()); n != 0 {
		t.Errorf("listing models sent %d upstream requests, want none", n)
	}
}

// describe adds an API error's details, which its Error method omits.
func describe(err error) string {
	var aerr *openai.Error
	if errors.As(err, &aerr) {
		return aerr.Error() + ": " + aerr.RawJSON()
	}
	return err.Error()
}
