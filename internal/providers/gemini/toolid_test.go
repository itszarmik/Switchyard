package gemini

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/anthropic"
	"switchyard/test/fakeanthropic"
)

// idPattern is the charset every tool-call id must keep to: OpenAI
// clients treat ids as opaque strings, and Anthropic's tool_use ids must
// match ^[a-zA-Z0-9_-]+$.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// randomSignature returns a standard base64 signature of n random bytes.
func randomSignature(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestToolCallIDRoundTrip(t *testing.T) {
	tests := []struct {
		name, geminiID, sig string
	}{
		{"no signature", "fc_1", ""},
		{"standard base64 signature", "fc_1", randomSignature(t, 300)},
		{"signature with padding", "abc", base64.StdEncoding.EncodeToString([]byte("x"))},
		// Not canonical standard base64: kept as text, so it still comes
		// back byte for byte.
		{"URL-safe signature", "fc_2", base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe, 1, 2})},
		{"signature that is not base64", "fc_3", "not base64!"},
		{"Gemini id with odd characters", "functions.get:0 é", randomSignature(t, 32)},
		{"made-up id", newCallID(), randomSignature(t, 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := encodeToolCallID(testModel, tt.geminiID, tt.sig)
			if !idPattern.MatchString(id) || !strings.HasPrefix(id, toolIDPrefix) {
				t.Fatalf("id %q is outside [A-Za-z0-9_-] or lacks the %s prefix", id, toolIDPrefix)
			}
			got := decodeToolCallID(id, testModel)
			if !got.ours || got.geminiID != tt.geminiID || got.sig != tt.sig {
				t.Errorf("decoded %+v, want ours, id %q, signature %q", got, tt.geminiID, tt.sig)
			}
			if again := encodeToolCallID(testModel, tt.geminiID, tt.sig); again != id {
				t.Errorf("not deterministic: %q then %q", id, again)
			}
			// The upstream model is compared as Google names it, with or
			// without the models/ prefix.
			if d := decodeToolCallID(id, "models/"+testModel); !d.ours {
				t.Error("the models/ prefix changes the model hash")
			}
		})
	}
}

// TestToolCallIDForeign checks the ids that are not this adapter's for the
// request's model: each is sent as the client wrote it, without a
// signature, so the first call of its step gets the skip value.
func TestToolCallIDForeign(t *testing.T) {
	good := encodeToolCallID(testModel, "fc_1", randomSignature(t, 120))
	rest := strings.TrimPrefix(good, toolIDPrefix)
	raw, _ := base64.RawURLEncoding.DecodeString(rest)
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 1
	tests := map[string]string{
		"OpenAI id":            "call_abc123",
		"Anthropic id":         "toolu_01A09q90qw90lq917835lq9",
		"hand-written":         "1",
		"cut short":            good[:len(good)-10],
		"one bit flipped":      toolIDPrefix + base64.RawURLEncoding.EncodeToString(flipped),
		"prefix only":          toolIDPrefix,
		"not base64 after it":  toolIDPrefix + "!!!!",
		"padded base64":        good + "==",
		"older version prefix": "gm0_" + rest,
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			got := decodeToolCallID(id, testModel)
			if got.ours || got.geminiID != id || got.sig != "" {
				t.Errorf("decoded %+v, want the id as is, not ours, no signature", got)
			}
		})
	}
}

// TestToolCallIDOtherModel checks an id this adapter made for another
// upstream model: it decodes to its Gemini id, which keeps the request
// small and the call and its response matched, but its signature is never
// sent to this model.
func TestToolCallIDOtherModel(t *testing.T) {
	for _, sig := range []string{"", randomSignature(t, 1024)} {
		id := encodeToolCallID("gemini-other", "fc_1", sig)
		got := decodeToolCallID(id, testModel)
		if got != (decodedID{geminiID: "fc_1", ours: true}) {
			t.Errorf("signature of %d bytes: decoded %+v, want fc_1, ours, another model, no signature", len(sig), got)
		}
	}
}

// TestToolCallIDLength measures the id with realistic signature sizes.
// Google does not document the size of a thought signature; signatures
// seen in practice are a few hundred bytes to a few kilobytes of base64,
// growing with the thinking behind the step. The id is about 4/3 of the
// decoded signature plus 20 bytes and the Gemini id, so its length is
// close to the signature's own base64 length.
func TestToolCallIDLength(t *testing.T) {
	for _, n := range []int{0, 256, 1024, 4096, 16384} {
		sig := ""
		if n > 0 {
			sig = randomSignature(t, n)
		}
		id := encodeToolCallID("gemini-4-carbon", "fc_7p2k9x", sig)
		// flags, model hash, length, id, check: 1+6+1+9+4 bytes.
		want := len(toolIDPrefix) + base64.RawURLEncoding.EncodedLen(21+n)
		if len(id) != want {
			t.Errorf("signature of %d bytes (%d base64): id of %d bytes, want %d", n, len(sig), len(id), want)
		}
		t.Logf("signature of %5d bytes (%5d as base64): id of %5d bytes", n, len(sig), len(id))
	}
}

func TestNewCallIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := newCallID()
		if seen[id] || !idPattern.MatchString(id) {
			t.Fatalf("id %q repeats or is outside the charset", id)
		}
		seen[id] = true
	}
}

// TestGeminiToClaude checks a conversation that moves from Gemini to
// Claude: the Anthropic adapter accepts the ids this adapter returned. An
// id of at most 64 bytes (a call without a signature) keeps its spelling,
// which already matches the tool_use pattern; a longer one (with a
// signature) is shortened to the same 37-byte hash in the tool_use block
// and in the tool_result that answers it. The Messages API fake accepts
// the request.
func TestGeminiToClaude(t *testing.T) {
	f := fakeanthropic.New(fakeanthropic.Options{APIKey: "sk-ant-test"})
	t.Cleanup(f.Close)
	ap := config.Provider{
		ID: "claude", Type: "anthropic", BaseURL: f.BaseURL(), APIKey: config.NewSecret("sk-ant-test"),
		Timeouts: config.ProviderTimeouts{Connect: config.Duration(testTimeout), Total: config.Duration(testTimeout)},
	}
	claude, err := anthropic.New(ap, providers.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claude.CloseIdleConnections)

	first := encodeToolCallID("gemini-3.8-flash", "fc_7p2k9x", randomSignature(t, 1024))
	second := encodeToolCallID("gemini-3.8-flash", "fc_8q3", "")
	body := `{"model":"m","messages":[{"role":"user","content":"weather and time in Paris?"},
		{"role":"assistant","tool_calls":[
			{"id":"` + first + `","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris\"}"}},
			{"id":"` + second + `","type":"function","function":{"name":"get_time","arguments":"{\"location\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"` + first + `","content":"18C"},
		{"role":"tool","tool_call_id":"` + second + `","content":"12:00"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}]}`
	req := request(t, body)
	req.UpstreamModel = "claude-test"
	if _, err := claude.Chat(ctxWithTimeout(t), req); err != nil {
		t.Fatalf("the Anthropic adapter rejected the Gemini history: %v", err)
	}
	reqs := f.Requests()
	if len(reqs) != 1 {
		t.Fatalf("the fake got %d requests", len(reqs))
	}
	sum := sha256.Sum256([]byte(first))
	for id, want := range map[string]string{
		first:  "call_" + base64.RawURLEncoding.EncodeToString(sum[:])[:32],
		second: second,
	} {
		if n := strings.Count(string(reqs[0].Body), `"`+want+`"`); n != 2 {
			t.Errorf("id %.20s… is sent as %q %d times in the Messages request, want 2 (tool_use and tool_result)", id, want, n)
		}
	}
	if len(second) > 64 || strings.Contains(string(reqs[0].Body), first) {
		t.Errorf("the signed id was sent whole, or the unsigned one is over 64 bytes (%d)", len(second))
	}
}
