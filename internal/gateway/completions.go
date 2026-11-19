package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// ShimChat is the X-Switchyard-Shim value of a legacy completion served
// through Chat Completions.
const ShimChat = "chat"

// Completions serves one POST /v1/completions request, in the legacy
// OpenAI shape. The request is routed as for chat completions (the
// assessor reads its prompt as a user message), and the plan executed the
// same way. A deployment with
// capability completions on an OpenAI-wire provider gets the request
// natively, forwarded losslessly with the model replaced. Any other
// deployment is served through the chat shim: the prompt becomes one user
// message, and the chat answer (or each stream chunk) is translated back
// into a text_completion, marked by X-Switchyard-Shim: chat. Each attempt
// uses whichever its deployment needs. Legacy parameters the shim cannot
// honor (echo, suffix, logprobs, best_of above 1, several prompts) plan
// only native deployments, and get a 400 unsupported_parameter when none
// can serve the request.
func (g *snapshot) Completions(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointCompletions)
	defer rec.done()
	if !g.knownCaller(ex, rec) || !g.readTags(ex, rec) {
		return
	}

	body, berr := ex.Body()
	if berr != nil {
		rec.respondError(ex, Info{}, berr)
		return
	}
	req, err := schema.DecodeCompletionRequest(body)
	if err != nil {
		rec.respondError(ex, Info{}, invalidRequest(err))
		return
	}
	rec.requestedModel = req.Model()
	rec.stream = req.Stream()
	markStream(ctx, req.Stream())
	if req.Stream() {
		release, ok := g.acquireStream(ctx, ex, rec)
		if !ok {
			return
		}
		defer release()
	}
	ctx, cancel, ok := g.withDeadline(ctx, ex, rec)
	defer cancel()
	if !ok {
		return
	}

	maxTokens, hasMax := req.MaxTokens()
	rr := routeRequest{
		model: req.Model(), header: ex.Header(),
		tenant: rec.tenant, keyID: rec.keyID, tags: rec.tags,
		size: len(body), maxTokens: maxTokens, hasMax: hasMax, outputs: completionOutputs(req),
		stream: req.Stream(),
	}
	// The chat form of the request is what the shim sends, and what the
	// assessor reads. Only a field of the wrong type, such as a
	// non-boolean stream, makes it fail.
	chat, chatErr := req.ChatRequest()
	rr.chat = chat

	shimErr := req.ShimError()
	if shimErr != nil {
		// Only a native deployment can serve this request.
		rr.required = req.RequiredCapabilities()
		rr.required[schema.CapCompletions] = struct{}{}
		rr.keep = func(d *catalog.Deployment) string {
			if !g.native(d) {
				return "lacks capability completions"
			}
			return ""
		}
	} else {
		if chatErr != nil {
			rec.respondError(ex, Info{}, invalidRequest(chatErr))
			return
		}
		rr.required = chat.RequiredCapabilities()
	}
	rt, e := g.route(ctx, rec.endpoint, &rr, &rec.rt)
	if e != nil {
		if shimErr != nil && e.Code == codeNoEligible {
			e = invalidRequest(shimErr)
		}
		g.failRoute(ex, rec, rt, e)
		return
	}
	g.usePlan(rec, rt)
	if shimErr != nil {
		chat = nil
	}
	g.execute(ctx, ex, rec, g.completionTry(ex, rec, req, chat))
}

// native reports whether d serves legacy completions natively.
func (g *snapshot) native(d *catalog.Deployment) bool {
	_, ok := g.adapters[d.Provider].(providers.Completer)
	return ok && d.ProviderType == "openai" && d.Capabilities.Has(schema.CapCompletions)
}

// completionTry returns the attempt function of a legacy completion: a
// native call on a deployment that serves completions, else the chat shim
// with chat, the request translated (nil when only native deployments are
// planned).
func (g *snapshot) completionTry(ex Exchange, rec *record, req *schema.CompletionRequest, chat *schema.ChatRequest) tryFunc {
	shim := g.chatTry(ex, rec, chat, completionShim{})
	return func(ctx context.Context, e *planEntry, info *Info) attempt {
		d := e.d
		if !g.native(d) {
			info.Shim = ShimChat
			return shim(ctx, e, info)
		}
		info.Shim = ""
		info.Provider = d.Provider
		rec.shadow.native = true
		r, dropped := req.Without(dropFields(e.dropped)...)
		info.DroppedParams = dropped
		rec.dropped = dropped
		adapter := g.adapters[d.Provider].(providers.Completer)
		creq := &providers.CompletionRequest{
			Completion: r, UpstreamModel: d.UpstreamModel, Header: ex.Header(),
			NoStreamUsage: !d.Capabilities.Has(schema.CapStreamUsage),
		}
		if r.Stream() {
			return g.streamAttempt(ctx, ex, rec, info, r.IncludeUsage(), nil, func(ctx context.Context) (providers.Stream, error) {
				return adapter.CompletionStream(ctx, creq)
			})
		}
		start := time.Now()
		resp, err := adapter.Completion(ctx, creq)
		return g.responseAttempt(ex, rec, start, resp, err, info, nil)
	}
}

// completionShim translates chat answers into legacy text completions.
type completionShim struct{}

var _ converter = completionShim{}

// Wire shapes the shim reads and writes. Only what it translates exists.
type (
	shimChatResponse struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content *string `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"message"`
			Delta struct {
				Content *string `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage             json.RawMessage `json:"usage,omitempty"`
		SystemFingerprint *string         `json:"system_fingerprint,omitempty"`
	}
	textCompletion struct {
		ID                string           `json:"id"`
		Object            string           `json:"object"`
		Created           int64            `json:"created"`
		Model             string           `json:"model"`
		Choices           []textChoice     `json:"choices"`
		SystemFingerprint *string          `json:"system_fingerprint,omitempty"`
		Usage             *json.RawMessage `json:"usage,omitempty"`
	}
	textChoice struct {
		Text         string          `json:"text"`
		Index        int             `json:"index"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason *string         `json:"finish_reason"`
	}
)

// response translates a chat.completion into a text_completion: each
// choice's message content (or refusal) becomes its text.
func (completionShim) response(body []byte) ([]byte, error) {
	var c shimChatResponse
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	out := c.textCompletion()
	for i, ch := range c.Choices {
		out.Choices[i].Text = text(ch.Message.Content, ch.Message.Refusal)
	}
	return json.Marshal(out)
}

// chunk translates a chat.completion.chunk into a text_completion chunk.
// Chunks that carry neither text, a finish reason nor usage (such as the
// role chunk that opens a chat stream) are skipped. A chunk that is not
// valid JSON is forwarded as is.
func (completionShim) chunk(chunk *schema.ChatChunk) ([]byte, bool) {
	var c shimChatResponse
	if err := json.Unmarshal(chunk.Raw, &c); err != nil {
		return chunk.Raw, true
	}
	out := c.textCompletion()
	useful := len(c.Usage) > 0 && string(c.Usage) != "null"
	for i, ch := range c.Choices {
		out.Choices[i].Text = text(ch.Delta.Content, ch.Delta.Refusal)
		if out.Choices[i].Text != "" || ch.FinishReason != nil {
			useful = true
		}
	}
	if !useful {
		return nil, false
	}
	data, err := json.Marshal(out)
	if err != nil {
		return chunk.Raw, true
	}
	return data, true
}

// textCompletion returns the text_completion skeleton of c: its id (with
// the chatcmpl- prefix changed to cmpl-), created, model, usage and one
// choice per chat choice, without text.
func (c *shimChatResponse) textCompletion() textCompletion {
	out := textCompletion{
		ID:      "cmpl-" + strings.TrimPrefix(c.ID, "chatcmpl-"),
		Object:  "text_completion",
		Created: c.Created, Model: c.Model,
		Choices:           make([]textChoice, len(c.Choices)),
		SystemFingerprint: c.SystemFingerprint,
	}
	if len(c.Usage) > 0 {
		u := c.Usage
		out.Usage = &u
	}
	for i, ch := range c.Choices {
		out.Choices[i] = textChoice{Index: ch.Index, Logprobs: json.RawMessage("null"), FinishReason: ch.FinishReason}
	}
	return out
}

// text returns the content, or the refusal when there is no content.
func text(content, refusal *string) string {
	switch {
	case content != nil && *content != "":
		return *content
	case refusal != nil:
		return *refusal
	}
	return ""
}
