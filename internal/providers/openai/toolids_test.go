package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"switchyard/test/fakeupstream"
)

// geminiID has the length of a gemini adapter id carrying a 256-byte thought
// signature.
var geminiID = "gm1_" + strings.Repeat("A1b2-C3d_", 41)

func TestShortToolCallID(t *testing.T) {
	short := ShortToolCallID(geminiID)
	if len(short) != 37 || !regexp.MustCompile(`^call_[A-Za-z0-9_-]{32}$`).MatchString(short) {
		t.Errorf("ShortToolCallID = %q (%d), want call_ and 32 base64url characters", short, len(short))
	}
	if ShortToolCallID(geminiID) != short || ShortToolCallID(geminiID+"x") == short {
		t.Error("not deterministic, or distinct ids collide")
	}
}

// TestLongToolCallIDsShortened checks that a history with tool-call ids
// longer than OpenAI's 40 characters reaches the upstream with them
// shortened, the same way in the assistant call and in the tool message,
// while short ids, every other member and every other message keep their
// spelling; and that a request without such ids is forwarded byte for
// byte.
func TestLongToolCallIDsShortened(t *testing.T) {
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[]}`))
	})
	a := newAdapter(t, testProvider(u.URL))

	body := `{"model":"m","messages":[{"role":"user","content":"a <b> & 1.0e-7"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"` + geminiID + `","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}},
			{"id":"call_short","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"` + geminiID + `","content":"r <1>"},
		{"role":"tool","tool_call_id":"call_short","content":"s"}],"temperature":1.0e0}`
	if _, err := a.Chat(ctxWithTimeout(t), chatRequest(t, body, nil)); err != nil {
		t.Fatal(err)
	}
	got := u.lastBody()
	short := ShortToolCallID(geminiID)
	if strings.Contains(got, geminiID) || strings.Count(got, `"`+short+`"`) != 2 || strings.Count(got, `"call_short"`) != 2 {
		t.Errorf("upstream body %s: want the long id replaced by %s twice and call_short kept", got, short)
	}
	for _, kept := range []string{`"content":"a <b> & 1.0e-7"`, `"arguments":"{\"x\":1}"`, `"content":"r <1>"`, `"temperature":1.0e0`} {
		if !strings.Contains(got, kept) {
			t.Errorf("upstream body lost %s: %s", kept, got)
		}
	}

	plain := `{"model":"m","messages":[{"role":"user","content":"x <y>"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"1.50"}],  "n" : 1}`
	req := chatRequest(t, plain, nil)
	if _, err := a.Chat(ctxWithTimeout(t), req); err != nil {
		t.Fatal(err)
	}
	if want := string(req.Chat.EncodeFor("upstream-model", false)); u.lastBody() != want {
		t.Errorf("a request without long ids was re-encoded:\n got %s\nwant %s", u.lastBody(), want)
	}
}

// TestLongToolCallIDsAgainstFake runs a history with long ids against the
// fake, which rejects ids over 40 characters as OpenAI does, streaming and
// not.
func TestLongToolCallIDsAgainstFake(t *testing.T) {
	f := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
	t.Cleanup(f.Close)
	a := newAdapter(t, testProvider(f.BaseURL()))
	body := func(stream bool) string {
		b, _ := json.Marshal(stream)
		return `{"model":"m","stream":` + string(b) + `,"messages":[{"role":"user","content":"weather?"},
			{"role":"assistant","tool_calls":[{"id":"` + geminiID + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"` + geminiID + `","content":"18C"}]}`
	}
	if _, err := a.Chat(ctxWithTimeout(t), chatRequest(t, body(false), nil)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, body(true), nil))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	_ = s.Close()

	t.Run("the fake rejects a long id", func(t *testing.T) {
		f.Reset()
		code, resp := postRaw(t, f.BaseURL()+"/chat/completions", body(false))
		if code != 400 || !strings.Contains(resp, "string_above_max_length") {
			t.Errorf("fake answered %d %s", code, resp)
		}
	})
}

// postRaw posts body with the provider key and returns the status and body.
func postRaw(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctxWithTimeout(t), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+providerKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
