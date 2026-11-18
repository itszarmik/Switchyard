package fakeupstream_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/test/fakeupstream"
)

// embedLen embeds a string as [len, 1, -0.5]: a vector that tells inputs
// apart and has exact float32 values.
func embedLen(s string) []float32 { return []float32{float32(len(s)), 1, -0.5} }

// embeddingList is the part of an embeddings response the tests check.
type embeddingList struct {
	Object string `json:"object"`
	Model  string `json:"model"`
	Data   []struct {
		Object    string          `json:"object"`
		Index     int             `json:"index"`
		Embedding json.RawMessage `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// vectors decodes every embedding of l, float or base64.
func (l embeddingList) vectors(t *testing.T) [][]float32 {
	t.Helper()
	var out [][]float32
	for _, d := range l.Data {
		var v []float32
		if err := json.Unmarshal(d.Embedding, &v); err != nil {
			var s string
			if err := json.Unmarshal(d.Embedding, &s); err != nil {
				t.Fatalf("embedding %s is neither floats nor base64", d.Embedding)
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil || len(b)%4 != 0 {
				t.Fatalf("embedding %q is not base64 float32s", s)
			}
			for i := 0; i < len(b); i += 4 {
				v = append(v, math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))
			}
		}
		out = append(out, v)
	}
	return out
}

func postEmbeddings(t *testing.T, s *fakeupstream.Server, body string) response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/embeddings", "", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return r
}

func TestEmbeddings(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		behavior   fakeupstream.Behavior
		wantStatus int
		wantCode   string
		wantVecs   [][]float32
		wantTokens int
	}{
		{
			name: "string input", body: `{"model":"e","input":"hello"}`,
			wantStatus: 200, wantVecs: [][]float32{{5, 1, -0.5}}, wantTokens: 2,
		},
		{
			name: "array input", body: `{"model":"e","input":["a","abcdefghi"],"encoding_format":"float"}`,
			wantStatus: 200, wantVecs: [][]float32{{1, 1, -0.5}, {9, 1, -0.5}}, wantTokens: 1 + 3,
		},
		{
			name: "base64", body: `{"model":"e","input":"abcd","encoding_format":"base64"}`,
			wantStatus: 200, wantVecs: [][]float32{{4, 1, -0.5}}, wantTokens: 1,
		},
		{
			name: "usage override", body: `{"model":"e","input":"abcd"}`,
			behavior:   fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 42}},
			wantStatus: 200, wantVecs: [][]float32{{4, 1, -0.5}}, wantTokens: 42,
		},
		{
			name: "scripted error", body: `{"model":"e","input":"abcd"}`,
			behavior:   fakeupstream.Behavior{Status: 503},
			wantStatus: 503,
		},
		{
			name: "unknown model", body: `{"model":"nope","input":"x"}`,
			wantStatus: 404, wantCode: "model_not_found",
		},
		{
			name: "missing input", body: `{"model":"e"}`,
			wantStatus: 400, wantCode: "missing_required_parameter",
		},
		{
			name: "empty array", body: `{"model":"e","input":[]}`,
			wantStatus: 400, wantCode: "empty_array",
		},
		{
			name: "token input", body: `{"model":"e","input":[1,2,3]}`,
			wantStatus: 400, wantCode: "invalid_type",
		},
		{
			name: "bad encoding", body: `{"model":"e","input":"x","encoding_format":"int8"}`,
			wantStatus: 400, wantCode: "invalid_value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, fakeupstream.Options{Models: []string{"e"}, Embed: embedLen})
			s.Script(tc.behavior)
			r := postEmbeddings(t, s, tc.body)
			if r.status != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", r.status, tc.wantStatus, r.body)
			}
			if r.header.Get("X-Request-Id") == "" {
				t.Error("no x-request-id")
			}
			if r.status != 200 {
				env := decodeEnvelope(t, r)
				if tc.wantCode != "" && deref(env.Error.Code) != tc.wantCode {
					t.Errorf("code %q, want %q", deref(env.Error.Code), tc.wantCode)
				}
				return
			}
			var l embeddingList
			if err := json.Unmarshal(r.body, &l); err != nil {
				t.Fatalf("decoding %s: %v", r.body, err)
			}
			if l.Object != "list" || l.Model != "e" {
				t.Errorf("object %q model %q, want list e", l.Object, l.Model)
			}
			for i, d := range l.Data {
				if d.Object != "embedding" || d.Index != i {
					t.Errorf("data[%d] object %q index %d", i, d.Object, d.Index)
				}
			}
			if diff := cmp.Diff(tc.wantVecs, l.vectors(t)); diff != "" {
				t.Errorf("vectors (-want +got):\n%s", diff)
			}
			if l.Usage.PromptTokens != tc.wantTokens || l.Usage.TotalTokens != tc.wantTokens {
				t.Errorf("usage %+v, want %d tokens", l.Usage, tc.wantTokens)
			}
			if reqs := s.Requests(); len(reqs) != 1 || reqs[0].Path != "/v1/embeddings" || reqs[0].Status != 200 {
				t.Errorf("recorded %+v", reqs)
			}
		})
	}
}

// TestEmbeddingsHold checks that a held embeddings request waits for its
// release, as chat completions do.
func TestEmbeddingsHold(t *testing.T) {
	s := newServer(t, fakeupstream.Options{Embed: embedLen})
	release := make(chan struct{})
	s.Script(fakeupstream.Behavior{Hold: release})
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	type result struct {
		r   response
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/embeddings", "", `{"model":"e","input":"x"}`)
		done <- result{r, err}
	}()

	if err := s.WaitReceived(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		t.Fatalf("answered before release: %d %v", res.r.status, res.err)
	default:
	}
	close(release)
	if res := <-done; res.err != nil || res.r.status != 200 {
		t.Errorf("after release: status %d, %v; want 200", res.r.status, res.err)
	}
}
