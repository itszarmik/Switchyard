package judge

import (
	"encoding/json"
	"errors"
	"strings"

	"switchyard/internal/schema"
)

// errNoChoice is returned by AnswerText for a response without choices.
var errNoChoice = errors.New("judge: the response has no choices")

// AnswerText renders the first choice of a non-streaming chat completion
// body as the text a judge compares: the message content, then each tool
// call as "[tool call] name(arguments)", a refusal as "[refusal] …", and
// a note when the answer was cut off at its token limit. Errors never
// include the body.
func AnswerText(body []byte) (string, error) {
	resp, err := schema.ParseChatResponse(body)
	if err != nil {
		return "", errors.New("judge: the response is not a chat completion")
	}
	if len(resp.Choices) == 0 {
		return "", errNoChoice
	}
	var choice struct {
		Message struct {
			Content   json.RawMessage   `json:"content"`
			Refusal   *string           `json:"refusal"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	if err := json.Unmarshal(resp.Choices[0], &choice); err != nil {
		return "", errors.New("judge: the response's first choice is malformed")
	}
	var parts []string
	if t := contentText(choice.Message.Content); t != "" {
		parts = append(parts, t)
	}
	if r := choice.Message.Refusal; r != nil && *r != "" {
		parts = append(parts, "[refusal] "+*r)
	}
	for _, tc := range choice.Message.ToolCalls {
		parts = append(parts, toolCallText(tc))
	}
	if choice.FinishReason == "length" {
		parts = append(parts, "[the answer was cut off at its output token limit]")
	}
	return strings.Join(parts, "\n"), nil
}

// StripPersistence removes from obj, a chat request's top-level members,
// the fields by which a client asks a provider to keep, tag or bill its
// request in a particular way: metadata, user and service_tier are
// deleted, and store, when present, is set to false. Background calls the
// client never asked for (shadow comparisons, eval answers) send the
// request without them, so that no second deployment stores the prompt
// and answer on the provider side. Judge requests never carry them.
func StripPersistence(obj map[string]json.RawMessage) {
	for _, f := range []string{"metadata", "user", "service_tier"} {
		delete(obj, f)
	}
	if _, ok := obj["store"]; ok {
		obj["store"] = json.RawMessage("false")
	}
}
