package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/sse"
)

// completionIDPrefix starts every chat.completion and chunk id, followed by
// Gemini's responseId. OpenAI clients expect the prefix.
const completionIDPrefix = "chatcmpl-"

// maxCalls bounds the function calls of one response. A real response has
// a handful; an upstream that sends more is broken.
const maxCalls = 1000

// Wire types of the GenerateContentResponse: only what the adapter reads.
// A stream event is the same object, or one with a top-level error.
type (
	generateResponse struct {
		Candidates     []candidate     `json:"candidates"`
		PromptFeedback *promptFeedback `json:"promptFeedback"`
		UsageMetadata  *geminiUsage    `json:"usageMetadata"`
		ModelVersion   string          `json:"modelVersion"`
		ResponseID     string          `json:"responseId"`
		Error          *googleError    `json:"error"`
	}
	candidate struct {
		Content *struct {
			Parts []responsePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	}
	responsePart struct {
		Text             *string `json:"text"`
		Thought          bool    `json:"thought"`
		ThoughtSignature string  `json:"thoughtSignature"`
		FunctionCall     *struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCall"`
	}
	promptFeedback struct {
		BlockReason string `json:"blockReason"`
	}
	// geminiUsage is usageMetadata. In a stream it is cumulative, so each
	// event's replaces the last.
	geminiUsage struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	}
)

// OpenAI-shaped output types, in OpenAI's field order.
type (
	completion struct {
		ID      string        `json:"id"`
		Object  string        `json:"object"`
		Created int64         `json:"created"`
		Model   string        `json:"model"`
		Choices []choice      `json:"choices"`
		Usage   *schema.Usage `json:"usage"`
	}
	choice struct {
		Index        int             `json:"index"`
		Message      choiceMessage   `json:"message"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason string          `json:"finish_reason"`
	}
	choiceMessage struct {
		Role      string     `json:"role"`
		Content   *string    `json:"content"`
		Refusal   *string    `json:"refusal"`
		ToolCalls []toolCall `json:"tool_calls,omitempty"`
	}
	toolCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function functionArgs `json:"function"`
	}
	functionArgs struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	}
)

// jsonNull is the JSON null literal.
var jsonNull = json.RawMessage("null")

// normalize maps Gemini usage onto OpenAI's (DESIGN §6.2, §12.1):
// prompt_tokens is promptTokenCount, which includes cached tokens, plus
// toolUsePromptTokenCount (billed as input); cached_tokens is
// cachedContentTokenCount; completion_tokens is candidatesTokenCount plus
// thoughtsTokenCount, since Google bills thinking as output and excludes
// it from candidatesTokenCount; reasoning_tokens is thoughtsTokenCount.
// The ledger then charges cached tokens at cached_input, the rest of the
// prompt at input, and completion_tokens once at output.
func (u *geminiUsage) normalize() *schema.Usage {
	if u == nil {
		return nil
	}
	out := &schema.Usage{
		PromptTokens:            u.PromptTokenCount + u.ToolUsePromptTokenCount,
		CompletionTokens:        u.CandidatesTokenCount + u.ThoughtsTokenCount,
		PromptTokensDetails:     &schema.PromptTokensDetails{CachedTokens: u.CachedContentTokenCount},
		CompletionTokensDetails: &schema.CompletionTokensDetails{ReasoningTokens: u.ThoughtsTokenCount},
	}
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	return out
}

// finishReason maps a Gemini finishReason onto OpenAI's finish_reason.
// hasCalls says whether the response has function calls: Gemini finishes
// with STOP whether or not it called functions. It returns a non-nil
// failure for the reasons that mean the answer is not one:
//
//   - MALFORMED_FUNCTION_CALL and MALFORMED_RESPONSE: the model produced an
//     invalid call or response. Google ties these to the request (its
//     prompt or schemas), so they are KindUnsupported: the request falls
//     back to another deployment, but is not retried on this one and does
//     not count toward its circuit breaker or the provider error-rate
//     alert, so one client's requests cannot open the breaker for every
//     tenant. As the final answer it is a 502.
//   - MISSING_THOUGHT_SIGNATURE: the request's history lacked a signature,
//     a client error, as the 400 Google returns for the same problem.
//
// UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS, unknown and other reasons are
// stop (or tool_calls), as in the anthropic adapter.
func finishReason(reason string, hasCalls bool) (string, *finishFailure) {
	switch reason {
	case "MAX_TOKENS", "CONTINUATION":
		return "length", nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "ESCALATION",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_OTHER", "IMAGE_RECITATION":
		return "content_filter", nil
	case "MALFORMED_FUNCTION_CALL", "MALFORMED_RESPONSE":
		return "", &finishFailure{reason: reason, kind: providers.KindUnsupported}
	case "MISSING_THOUGHT_SIGNATURE":
		return "", &finishFailure{reason: reason, kind: providers.KindClientError}
	}
	// STOP, OTHER, LANGUAGE, UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS,
	// NO_IMAGE, FINISH_REASON_UNSPECIFIED and new values.
	if hasCalls {
		return "tool_calls", nil
	}
	return "stop", nil
}

// finishFailure is a finishReason that fails the call.
type finishFailure struct {
	reason string
	kind   providers.Kind
}

// pendingCall is a function call of the response being read.
type pendingCall struct {
	geminiID string
	name     string
	args     json.RawMessage
	sig      string
}

// accumulator collects a response from one GenerateContentResponse or the
// events of a stream: the text, the function calls (a call whose id
// repeats is one call split over events, and its args are merged), the
// finish reason and the last usage.
type accumulator struct {
	provider string
	model    string // the upstream model, for the tool-call ids
	events   int
	id       string // responseId
	version  string // modelVersion
	text     strings.Builder
	calls    []*pendingCall
	finished bool
	blocked  bool   // promptFeedback.blockReason, no candidates
	reason   string // the Gemini finishReason
	usage    *geminiUsage
	sawCand  bool
	madeID   string // the completion id's suffix when Gemini sent no responseId
}

func newAccumulator(provider, model string) *accumulator {
	return &accumulator{provider: provider, model: model}
}

// responseID is the upstream response id, as received.
func (acc *accumulator) responseID() string { return acc.id }

// apply adds one GenerateContentResponse. It returns the new text, in
// order, and an error for a finish reason that fails the call. Text parts
// marked thought (summaries, which the adapter never asks for) are
// skipped, and so are text signatures: Google recommends returning them
// but does not check them, and an OpenAI message has no place for them.
func (acc *accumulator) apply(gr *generateResponse) ([]string, error) {
	acc.events++
	if acc.id == "" {
		acc.id = gr.ResponseID
	}
	if acc.version == "" {
		acc.version = gr.ModelVersion
	}
	if gr.UsageMetadata != nil {
		u := *gr.UsageMetadata
		acc.usage = &u
	}
	if acc.finished {
		return nil, nil // only usage may follow the finish
	}
	if len(gr.Candidates) == 0 {
		if gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "" {
			acc.finished, acc.blocked = true, true
		}
		return nil, nil
	}
	acc.sawCand = true
	cand := gr.Candidates[0]
	var texts []string
	if cand.Content != nil {
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				if err := acc.addCall(p.FunctionCall.ID, p.FunctionCall.Name, p.FunctionCall.Args, p.ThoughtSignature); err != nil {
					return nil, err
				}
			case p.Text != nil && !p.Thought && *p.Text != "":
				texts = append(texts, *p.Text)
				acc.text.WriteString(*p.Text)
			}
		}
	}
	if cand.FinishReason != "" {
		if _, f := finishReason(cand.FinishReason, false); f != nil {
			return nil, acc.failure(f)
		}
		acc.finished, acc.reason = true, cand.FinishReason
	}
	return texts, nil
}

// addCall records a function call, or merges it into the earlier call
// with the same Gemini id. A call without an id gets a new one.
func (acc *accumulator) addCall(id, name string, args json.RawMessage, sig string) error {
	if id != "" {
		for _, c := range acc.calls {
			if c.geminiID == id {
				c.args = mergeArgs(c.args, args)
				if c.name == "" {
					c.name = name
				}
				if c.sig == "" {
					c.sig = sig
				}
				return nil
			}
		}
	}
	if len(acc.calls) >= maxCalls {
		return &providers.Error{
			Kind: providers.KindServerError, Provider: acc.provider, UpstreamRequestID: acc.id,
			Message: fmt.Sprintf("upstream sent more than %d function calls", maxCalls),
		}
	}
	if id == "" {
		id = newCallID()
	}
	acc.calls = append(acc.calls, &pendingCall{geminiID: id, name: name, args: mergeArgs(nil, args), sig: sig})
	return nil
}

// mergeArgs returns the compact args object of a call, with the keys of
// next added to (or replacing) those of prev. Args that are not an object
// count as empty.
func mergeArgs(prev, next json.RawMessage) json.RawMessage {
	var p, n map[string]json.RawMessage
	_ = json.Unmarshal(prev, &p)
	if json.Unmarshal(next, &n) != nil || len(n) == 0 {
		if prev != nil {
			return prev
		}
		return compactObject(next)
	}
	if len(p) == 0 {
		return compactObject(next)
	}
	for k, v := range n {
		p[k] = v
	}
	b, err := marshal(p)
	if err != nil {
		return prev
	}
	return b
}

// compactObject returns args as a compact JSON object, or {} when it is
// absent or not an object.
func compactObject(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Compact(&buf, trimmed) != nil {
		return json.RawMessage(`{}`)
	}
	return buf.Bytes()
}

// failure is the error of a finish reason that fails the call. The answer
// was billed, so the error says so and carries its usage (see
// providers.Error.Billed).
func (acc *accumulator) failure(f *finishFailure) *providers.Error {
	// Google answered, and billed, the call: the error carries the usage
	// so far (cumulative in a stream), so that the gateway charges it.
	e := &providers.Error{
		Kind: f.kind, Provider: acc.provider, UpstreamRequestID: acc.id,
		Billed: true, BilledUsage: acc.usage.normalize(),
	}
	switch f.kind {
	case providers.KindClientError:
		msg := "the conversation history is missing a thought signature (finish reason MISSING_THOUGHT_SIGNATURE); " +
			"send the assistant tool calls back with the ids the gateway returned"
		e.Status = http.StatusBadRequest
		e.Message = "upstream rejected the request: " + msg
		e.Body = openAIEnvelope(msg, "invalid_request_error", "messages", "missing_thought_signature")
	default:
		msg := fmt.Sprintf("the model did not produce a valid answer (finish reason %s)", f.reason)
		e.Status = http.StatusBadGateway
		e.Message = "upstream " + msg
		e.Body = openAIEnvelope(msg, "server_error", "", strings.ToLower(f.reason))
	}
	return e
}

// finish returns the OpenAI finish_reason of a finished response.
func (acc *accumulator) finish() string {
	if acc.blocked {
		return "content_filter"
	}
	reason, _ := finishReason(acc.reason, len(acc.calls) > 0)
	return reason
}

// end finishes a non-streaming response: one without a finish reason
// ends as if it had STOP, unless it had no candidate at all, which is not
// an answer.
func (acc *accumulator) end() error {
	if acc.finished {
		return nil
	}
	if !acc.sawCand {
		return &providers.Error{
			Kind: providers.KindServerError, Provider: acc.provider, UpstreamRequestID: acc.id,
			Message: "upstream sent a response without candidates",
		}
	}
	acc.finished = true
	return nil
}

// toolCalls returns the calls with their client-facing ids.
func (acc *accumulator) toolCalls() []toolCall {
	var out []toolCall
	for _, c := range acc.calls {
		out = append(out, toolCall{
			ID: encodeToolCallID(acc.model, c.geminiID, c.sig), Type: "function",
			Function: functionArgs{Name: c.name, Arguments: string(c.args)},
		})
	}
	return out
}

// completionID is the chat.completion id: the prefix and Gemini's
// responseId, or a made-up id when Gemini sent none.
func (acc *accumulator) completionID() string {
	if acc.id != "" {
		return completionIDPrefix + acc.id
	}
	if acc.madeID == "" {
		acc.madeID = strings.TrimPrefix(newCallID(), "call_")
	}
	return completionIDPrefix + acc.madeID
}

// modelName is the model a response names: Gemini's modelVersion, or the
// upstream model when it sent none.
func (acc *accumulator) modelName() string {
	if acc.version != "" {
		return acc.version
	}
	return strings.TrimPrefix(acc.model, "models/")
}

// completion encodes the finished response as a chat.completion body and
// returns it with its normalized usage. A blocked prompt is an answer with
// empty content and finish_reason content_filter.
func (acc *accumulator) completion(now time.Time) ([]byte, *schema.Usage, error) {
	calls := acc.toolCalls()
	msg := choiceMessage{Role: "assistant", ToolCalls: calls}
	if acc.text.Len() > 0 || len(calls) == 0 {
		s := acc.text.String()
		msg.Content = &s
	}
	usage := acc.usage.normalize()
	out := completion{
		ID: acc.completionID(), Object: "chat.completion", Created: now.Unix(), Model: acc.modelName(),
		Choices: []choice{{Index: 0, Message: msg, Logprobs: jsonNull, FinishReason: acc.finish()}},
		Usage:   usage,
	}
	b, err := marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return b, usage, nil
}

// Errors of eventReader.next.
var (
	errEventTooLarge = sse.ErrEventTooLarge
	errNotJSON       = errors.New("gemini: event is not JSON")
)

// eventReader reads the GenerateContentResponse objects of a
// streamGenerateContent?alt=sse body: "data: <json>" events separated by
// blank lines, with no event names and no [DONE] marker; the body's end
// ends the stream.
type eventReader struct {
	r *sse.Reader
}

// newEventReader reads body. A blank line is added after the body, so that
// a last event without its trailing blank line still counts: Google's SDKs
// accept one. A body cut short by a broken connection still fails, since
// the read error comes first.
func newEventReader(body io.Reader) *eventReader {
	return &eventReader{r: sse.NewReader(io.MultiReader(body, strings.NewReader("\n\n")), sse.ReaderOptions{})}
}

// next returns the next event, io.EOF at the end of the body, errNotJSON
// for an event that is not a JSON object, or the read error.
func (er *eventReader) next() (*generateResponse, error) {
	ev, err := er.r.Next()
	if err != nil {
		return nil, err
	}
	if string(bytes.TrimSpace(ev.Data)) == "[DONE]" {
		return nil, io.EOF // not sent by this endpoint, but unambiguous
	}
	var gr generateResponse
	if json.Unmarshal(ev.Data, &gr) != nil {
		return nil, errNotJSON
	}
	return &gr, nil
}
