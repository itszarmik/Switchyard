package openai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// TestEndpoints checks the legacy completions and embeddings calls: each
// goes to its own path, losslessly with the model replaced, with usage
// parsed and the same headers as chat.
func TestEndpoints(t *testing.T) {
	var paths []string
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"object\":\"text_completion\",\"choices\":[{\"text\":\"a\"}]}\n\n"+
				"data: {\"object\":\"text_completion\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"object":"x","usage":{"prompt_tokens":5,"total_tokens":5}}`)
	})
	a := newAdapter(t, testProvider(u.URL+"/v1"))
	ctx := ctxWithTimeout(t)

	cr, err := schema.DecodeCompletionRequest([]byte(`{"model":"m","prompt":["a","b"],"echo":true,"big":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Completion(ctx, &providers.CompletionRequest{Completion: cr, UpstreamModel: "up", Header: http.Header{}})
	if err != nil || resp.Usage == nil || resp.Usage.PromptTokens != 5 {
		t.Fatalf("Completion: %+v, %v", resp, err)
	}
	if want := `{"model":"up","prompt":["a","b"],"echo":true,"big":12345678901234567890}`; u.lastBody() != want {
		t.Errorf("completion body %s, want %s", u.lastBody(), want)
	}

	cr, _ = schema.DecodeCompletionRequest([]byte(`{"model":"m","prompt":"a","stream":true}`))
	s, err := a.CompletionStream(ctx, &providers.CompletionRequest{Completion: cr, UpstreamModel: "up", Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := collect(t, s)
	if !errors.Is(err, io.EOF) || len(chunks) != 2 || s.Usage() == nil || s.Usage().TotalTokens != 2 {
		t.Errorf("stream chunks %q, err %v, usage %+v", chunks, err, s.Usage())
	}
	if want := `{"model":"up","prompt":"a","stream":true,"stream_options":{"include_usage":true}}`; u.lastBody() != want {
		t.Errorf("stream body %s, want %s", u.lastBody(), want)
	}
	s, err = a.CompletionStream(ctx, &providers.CompletionRequest{Completion: cr, UpstreamModel: "up", Header: http.Header{}, NoStreamUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collect(t, s)
	if want := `{"model":"up","prompt":"a","stream":true}`; u.lastBody() != want {
		t.Errorf("stream body without stream_usage %s, want %s", u.lastBody(), want)
	}

	er, err := schema.DecodeEmbeddingRequest([]byte(`{"model":"m","input":"hi","dimensions":8}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = a.Embed(ctx, &providers.EmbedRequest{Embedding: er, UpstreamModel: "emb-up", Header: http.Header{}})
	if err != nil || resp.Usage == nil || resp.Usage.PromptTokens != 5 {
		t.Fatalf("Embed: %+v, %v", resp, err)
	}
	if want := `{"model":"emb-up","input":"hi","dimensions":8}`; u.lastBody() != want {
		t.Errorf("embed body %s, want %s", u.lastBody(), want)
	}
	if got := u.lastHeader().Get("Authorization"); got != "Bearer "+providerKey {
		t.Errorf("embed Authorization %q", got)
	}

	req := chatRequest(t, `{"model":"m","messages":[{}],"stream":true}`, nil)
	req.NoStreamUsage = true
	s, err = a.ChatStream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collect(t, s)
	if want := `{"model":"upstream-model","messages":[{}],"stream":true}`; u.lastBody() != want {
		t.Errorf("chat body without stream_usage %s, want %s", u.lastBody(), want)
	}

	want := []string{"/v1/completions", "/v1/completions", "/v1/completions", "/v1/embeddings", "/v1/chat/completions"}
	if len(paths) != len(want) {
		t.Fatalf("paths %q", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("path %d = %s, want %s", i, paths[i], want[i])
		}
	}
}

// TestResponseSizeLimits checks the caps on a non-streaming response body:
// embeddings get maxEmbeddingResponseBytes, because a batch of 2,048
// inputs at 3,072 float dimensions is about 88 MB, while chat and
// completions keep maxResponseBytes.
func TestResponseSizeLimits(t *testing.T) {
	if maxEmbeddingResponseBytes != 256<<20 || maxResponseBytes != 64<<20 {
		t.Fatalf("limits are %d and %d", maxEmbeddingResponseBytes, maxResponseBytes)
	}
	// Just over the chat cap: one embedding vector, padded.
	size := maxResponseBytes + 1<<20
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		head := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[`
		tail := `0]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
		_, _ = io.WriteString(w, head)
		chunk := strings.Repeat("0,", 1<<15)
		for n := len(head) + len(tail); n < size; n += len(chunk) {
			_, _ = io.WriteString(w, chunk)
		}
		_, _ = io.WriteString(w, tail)
	})
	a := newAdapter(t, testProvider(u.URL+"/v1"))

	er, err := schema.DecodeEmbeddingRequest([]byte(`{"model":"m","input":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Embed(ctxWithTimeout(t), &providers.EmbedRequest{Embedding: er, UpstreamModel: "up", Header: http.Header{}})
	if err != nil || len(resp.Body) < size || resp.Usage == nil || resp.Usage.PromptTokens != 1 {
		t.Fatalf("Embed of a %d-byte response: %v", size, err)
	}

	_, err = a.Chat(ctxWithTimeout(t), chatRequest(t, `{"model":"m","messages":[{}]}`, nil))
	if pe := wantKind(t, err, providers.KindServerError); !strings.Contains(pe.Message, "exceeds") {
		t.Errorf("Chat of a %d-byte response: %v", size, pe)
	}
	cr, _ := schema.DecodeCompletionRequest([]byte(`{"model":"m","prompt":"a"}`))
	_, err = a.Completion(ctxWithTimeout(t), &providers.CompletionRequest{Completion: cr, UpstreamModel: "up", Header: http.Header{}})
	_ = wantKind(t, err, providers.KindServerError)
}
