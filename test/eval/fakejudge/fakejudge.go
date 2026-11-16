// Package fakejudge is a scriptable fake OpenAI-wire upstream for the
// evaluation tests: one server answers both the models under evaluation
// and the LLM judge, from a Go function the test supplies.
//
// It serves POST /v1/chat/completions, non-streaming only. A request whose
// last user message holds <answer_a> and <answer_b> sections (the judge's
// prompt) is a judge request: the script sees the two answers in
// presentation order and returns the judge's reply text, typically made
// with Verdict. Any other request is an answer request: the script sees the
// model and the last user message and returns the answer text. Usage is
// reported as one token per 4 bytes of request body and of reply, unless
// the reply sets it, so costs are predictable.
package fakejudge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is what the script sees of one chat completion request.
type Request struct {
	// Model is the request's model field (the upstream model).
	Model string
	// Judge reports a judge request.
	Judge bool
	// A and B are the answers in presentation order, for a judge request.
	A, B string
	// System is the system message, for a judge request.
	System string
	// User is the text of the last user message: the judge's prompt, or the
	// question to answer.
	User string
	// Body is the raw request body.
	Body []byte
}

// Reply is the script's answer to one request.
type Reply struct {
	// Status, when not 0 or 200, fails the request with this status and an
	// OpenAI error envelope.
	Status int
	// Content is the assistant message content.
	Content string
	// FinishReason defaults to "stop".
	FinishReason string
	// PromptTokens and CompletionTokens override the reported usage when
	// either is non-zero.
	PromptTokens, CompletionTokens int
	// NoUsage omits usage from the response.
	NoUsage bool
	// Delay holds the response back, or until the client goes away.
	Delay time.Duration
}

// Script answers requests. It is called concurrently.
type Script func(Request) Reply

// Server is a running fake. Its methods are safe for concurrent use.
type Server struct {
	ts     *httptest.Server
	script Script
	seq    atomic.Int64

	mu       sync.Mutex
	requests []Request
	inflight int
	peak     int
}

// New starts a fake that answers with script. Call Close when done.
func New(script Script) *Server {
	s := &Server{script: script}
	s.ts = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// BaseURL is the OpenAI-style API root, for a provider's base_url.
func (s *Server) BaseURL() string { return s.ts.URL + "/v1" }

// Close shuts the server down.
func (s *Server) Close() { s.ts.Close() }

// Requests returns the requests received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Counts returns how many judge and answer requests arrived.
func (s *Server) Counts() (judge, answers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.Judge {
			judge++
		} else {
			answers++
		}
	}
	return judge, answers
}

// PeakInFlight is the most requests the fake handled at once.
func (s *Server) PeakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// Verdict is a judge reply naming winner ("A", "B" or "tie").
func Verdict(winner string) string {
	return fmt.Sprintf(`{"winner":%q,"reason":"scripted"}`, winner)
}

// Prefer returns a judge reply from score: the answer with the higher
// score wins, equal scores tie.
func Prefer(score func(answer string) int, r Request) string {
	sa, sb := score(r.A), score(r.B)
	switch {
	case sa > sb:
		return Verdict("A")
	case sb > sa:
		return Verdict("B")
	}
	return Verdict("tie")
}

// wireRequest is the part of a request the fake reads.
type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		writeError(w, http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	var wr wireRequest
	if err != nil || json.Unmarshal(body, &wr) != nil {
		writeError(w, http.StatusBadRequest)
		return
	}
	req := Request{Model: wr.Model, Body: body}
	var lastUser string
	for _, m := range wr.Messages {
		text := contentText(m.Content)
		switch m.Role {
		case "system":
			req.System = text
		case "user":
			lastUser = text
		}
	}
	if a, okA := section(lastUser, "answer_a"); okA {
		if b, okB := section(lastUser, "answer_b"); okB {
			req.Judge, req.A, req.B = true, a, b
		}
	}
	req.User = lastUser

	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.inflight++
	s.peak = max(s.peak, s.inflight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()

	rep := s.script(req)
	if rep.Delay > 0 {
		t := time.NewTimer(rep.Delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	if rep.Status != 0 && rep.Status != http.StatusOK {
		writeError(w, rep.Status)
		return
	}
	finish := rep.FinishReason
	if finish == "" {
		finish = "stop"
	}
	prompt, completion := rep.PromptTokens, rep.CompletionTokens
	if prompt == 0 && completion == 0 {
		prompt, completion = max(1, len(body)/4), max(1, len(rep.Content)/4)
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-fake-%d", s.seq.Add(1)),
		"object":  "chat.completion",
		"created": 1767225600,
		"model":   wr.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": rep.Content},
			"finish_reason": finish,
		}},
	}
	if !rep.NoUsage {
		resp["usage"] = map[string]int{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// section returns the text between <tag>\n and \n</tag>.
func section(text, tag string) (string, bool) {
	open, closing := "<"+tag+">\n", "\n</"+tag+">"
	i := strings.Index(text, open)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// contentText returns a string content, or the text parts of an array.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type, Text string
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// writeError writes an OpenAI error envelope for status.
func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"scripted failure %d","type":"server_error","param":null,"code":null}}`, status)
}
