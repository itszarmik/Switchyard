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
// OpenAI shape. The model is resolved and the deployments planned as for
// chat completions, and the plan executed the same way. A deployment with
// capability completions on an OpenAI-wire provider gets the request
// natively, forwarded losslessly with the model replaced. Any other
// deployment is served through the chat shim: the prompt becomes one user
// message, and the chat answer (or each stream chunk) is translated back
// into a text_completion, marked by X-Switchyard-Shim: chat. Each attempt
// uses whichever its deployment needs. Legacy parameters the shim cannot
// honor (echo, suffix, logprobs, best_of above 1, several prompts) plan
// only native deployments, and get a 400 unsupported_parameter when none
// can serve the request.
func (g *Gateway) Completions(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointCompletions)
	defer rec.done()
	if !g.readTags(ex, rec) {
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
	ctx, cancel, ok := g.withDeadline(ctx, ex, rec)
	defer cancel()
	if !ok {
		return
	}

	res, e := g.resolve(req.Model(), rec.endpoint, chatType)
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return
	}
	rec.info = Info{Model: res.Model.ID, Alias: res.Alias}
	rec.alias = res.Alias
	rec.model = res.Model
	estIn := estimateIn(len(body), 0, 0, res.Model)
	maxTokens, hasMax := req.MaxTokens()
	estOut := estimateOut(maxTokens, hasMax, completionOutputs(req))
	rec.estIn, rec.estOut = estIn, estOut

	if shimErr := req.ShimError(); shimErr != nil {
		// Only a native deployment can serve this request.
		required := req.RequiredCapabilities()
		required[schema.CapCompletions] = struct{}{}
		e := g.buildPlan(&rec.plan, res, required, estIn, estOut, func(d *catalog.Deployment) string {
			if !g.native(d) {
				return "lacks capability completions"
			}
			return ""
		})
		rec.excluded = len(rec.plan.exclusions)
		if e != nil {
			if e.Code == codeNoEligible {
				e = invalidRequest(shimErr)
			}
			rec.respondError(ex, rec.info, e)
			return
		}
		g.execute(ctx, ex, rec, g.completionTry(ex, rec, req, nil))
		return
	}

	chat, err := req.ChatRequest()
	if err != nil {
		// The shim builds the chat request itself; only a field of the
		// wrong type, such as a non-boolean stream, can fail here.
		rec.respondError(ex, rec.info, invalidRequest(err))
		return
	}
	e = g.buildPlan(&rec.plan, res, chat.RequiredCapabilities(), estIn, estOut, nil)
	rec.excluded = len(rec.plan.exclusions)
	if e != nil {
		rec.respondError(ex, rec.info, e)
		return
	}
	g.execute(ctx, ex, rec, g.completionTry(ex, rec, req, chat))
}

// native reports whether d serves legacy completions natively.
func (g *Gateway) native(d *catalog.Deployment) bool {
	_, ok := g.adapters[d.Provider].(providers.Completer)
	return ok && d.ProviderType == "openai" && d.Capabilities.Has(schema.CapCompletions)
}

// completionTry returns the attempt function of a legacy completion: a
// native call on a deployment that serves completions, else the chat shim
// with chat, the request translated (nil when only native deployments are
// planned).
func (g *Gateway) completionTry(ex Exchange, rec *record, req *schema.CompletionRequest, chat *schema.ChatRequest) tryFunc {
	shim := g.chatTry(ex, rec, chat, completionShim{})
	return func(ctx context.Context, e *planEntry, info *Info) attempt {
		d := e.d
		if !g.native(d) {
			info.Shim = ShimChat
			return shim(ctx, e, info)
		}
		info.Shim = ""
		info.Provider = d.Provider
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
