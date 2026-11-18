package cache

import (
	"strings"
	"testing"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// chatRequest decodes body, failing the test if it is not a valid request.
func chatRequest(t *testing.T, body string) *schema.ChatRequest {
	t.Helper()
	r, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatRequest(%s): %v", body, err)
	}
	return r
}

// canonical returns the canonical form of body for target.
func canonical(t *testing.T, body, target string) string {
	t.Helper()
	c, err := Canonical(chatRequest(t, body), target)
	if err != nil {
		t.Fatalf("Canonical(%s): %v", body, err)
	}
	return string(c)
}

func TestScopeID(t *testing.T) {
	tests := []struct {
		name                     string
		scope, tenant, key, user string
		want                     string
		wantPrefix               string
	}{
		{"tenant", config.CacheScopeTenant, "acme", "k1", "alice", "t:acme", ""},
		{"empty scope is tenant", "", "acme", "k1", "", "t:acme", ""},
		{"key", config.CacheScopeKey, "acme", "k1", "alice", "t:acme:k:k1", ""},
		{"user", config.CacheScopeUser, "acme", "k1", "alice", "", "t:acme:u:"},
		{"user without user falls back to key", config.CacheScopeUser, "acme", "k1", "", "t:acme:k:k1", ""},
		{"invalid id bytes are escaped", config.CacheScopeKey, "a:k:b", "c%", "", "t:a%3Ak%3Ab:k:c%25", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScopeID(tt.scope, tt.tenant, tt.key, tt.user)
			switch {
			case tt.want != "" && got != tt.want:
				t.Errorf("ScopeID = %q, want %q", got, tt.want)
			case tt.wantPrefix != "" && (!strings.HasPrefix(got, tt.wantPrefix) || len(got) != len(tt.wantPrefix)+64):
				t.Errorf("ScopeID = %q, want %q followed by 64 hex digits", got, tt.wantPrefix)
			}
		})
	}

	t.Run("user hash hides the user and separates users", func(t *testing.T) {
		a := ScopeID(config.CacheScopeUser, "acme", "k1", "alice")
		b := ScopeID(config.CacheScopeUser, "acme", "k1", "bob")
		if a == b || strings.Contains(a, "alice") {
			t.Errorf("user scopes %q and %q", a, b)
		}
		if ScopeID(config.CacheScopeUser, "acme", "k2", "alice") != a {
			t.Error("the user scope depends on the key")
		}
	})

	// Pairs that a naive "t:"+tenant+":k:"+key would join to one string.
	t.Run("no collisions", func(t *testing.T) {
		triples := [][4]string{
			{config.CacheScopeTenant, "a:k:b", "", ""},
			{config.CacheScopeKey, "a", "b", ""},
			{config.CacheScopeKey, "a:k", "b", ""},
			{config.CacheScopeKey, "a", "k:b", ""},
			{config.CacheScopeKey, "a%3Ak", "b", ""},
			{config.CacheScopeKey, "a", "", ""},
			{config.CacheScopeTenant, "a", "", ""},
			{config.CacheScopeKey, "a:u:x", "", ""},
			{config.CacheScopeUser, "a", "k", "x"},
			{config.CacheScopeTenant, "b", "", ""},
		}
		seen := map[string][4]string{}
		for _, tr := range triples {
			s := ScopeID(tr[0], tr[1], tr[2], tr[3])
			if prev, dup := seen[s]; dup {
				t.Errorf("%q and %q both give %q", prev, tr, s)
			}
			seen[s] = tr
		}
	})
}

// baseBody is a request with most output-affecting fields set.
const baseBody = `{"model":"gpt","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],` +
	`"temperature":0.2,"top_p":0.9,"max_tokens":50,"stop":["\n"],"seed":7,"n":1,"logprobs":false,` +
	`"frequency_penalty":0,"presence_penalty":0,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}],` +
	`"tool_choice":"auto","response_format":{"type":"text"},"reasoning_effort":"low"}`

// withField returns baseBody with the top-level member name set to the raw
// JSON value (appended when new, replaced otherwise).
func withField(name, value string) string {
	r, _ := schema.DecodeChatRequest([]byte(baseBody))
	var b strings.Builder
	b.WriteString("{")
	first, done := true, false
	for k, v := range r.Members() {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + k + `":`)
		if k == name {
			b.WriteString(value)
			done = true
		} else {
			b.Write(v)
		}
	}
	if !done {
		b.WriteString(`,"` + name + `":` + value)
	}
	b.WriteString("}")
	return b.String()
}

func TestCanonicalSameKey(t *testing.T) {
	base := canonical(t, baseBody, "gpt-4o")
	tests := []struct {
		name string
		body string
	}{
		{"identical", baseBody},
		{"model field (the target replaces it)", withField("model", `"some-alias"`)},
		{"stream", withField("stream", "true")},
		{"stream false", withField("stream", "false")},
		{"stream_options", withField("stream_options", `{"include_usage":true}`)},
		{"user", withField("user", `"alice"`)},
		{"metadata", withField("metadata", `{"run":"1"}`)},
		{"store", withField("store", "true")},
		{"service_tier", withField("service_tier", `"flex"`)},
		{"whitespace", strings.ReplaceAll(baseBody, ",", ", ")},
		{"member order", `{"tool_choice":"auto","response_format":{"type":"text"},"reasoning_effort":"low","model":"x",` +
			`"messages":[{"content":"be brief","role":"system"},{"content":"hi","role":"user"}],` +
			`"temperature":0.2,"top_p":0.9,"max_tokens":50,"stop":["\n"],"seed":7,"n":1,"logprobs":false,` +
			`"frequency_penalty":0,"presence_penalty":0,"tools":[{"function":{"parameters":{"properties":{"a":{"type":"string"}},"type":"object"},"name":"f"},"type":"function"}]}`},
		{"string escapes", strings.Replace(baseBody, `"be brief"`, `"be \u0062rief"`, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonical(t, tt.body, "gpt-4o"); got != base {
				t.Errorf("canonical form changed:\n got %s\nwant %s", got, base)
			}
		})
	}
}

func TestCanonicalChangesKey(t *testing.T) {
	base := canonical(t, baseBody, "gpt-4o")
	tests := []struct {
		name string
		body string
	}{
		{"messages", strings.Replace(baseBody, `"hi"`, `"hello"`, 1)},
		{"system prompt", strings.Replace(baseBody, `"be brief"`, `"be long"`, 1)},
		{"message role", strings.Replace(baseBody, `"role":"system"`, `"role":"developer"`, 1)},
		{"message order", strings.Replace(baseBody, `[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]`,
			`[{"role":"user","content":"hi"},{"role":"system","content":"be brief"}]`, 1)},
		{"message name", strings.Replace(baseBody, `{"role":"user","content":"hi"}`, `{"role":"user","name":"x","content":"hi"}`, 1)},
		{"tools", strings.Replace(baseBody, `"name":"f"`, `"name":"g"`, 1)},
		{"tool schema", strings.Replace(baseBody, `"type":"string"`, `"type":"number"`, 1)},
		{"tool_choice", withField("tool_choice", `"required"`)},
		{"response_format", withField("response_format", `{"type":"json_object"}`)},
		{"temperature", withField("temperature", "0.3")},
		{"temperature 0", withField("temperature", "0")},
		{"top_p", withField("top_p", "1")},
		{"max_tokens", withField("max_tokens", "51")},
		{"max_completion_tokens", withField("max_completion_tokens", "50")},
		{"stop", withField("stop", `["END"]`)},
		{"seed", withField("seed", "8")},
		{"n", withField("n", "2")},
		{"logprobs", withField("logprobs", "true")},
		{"top_logprobs", withField("top_logprobs", "3")},
		{"frequency_penalty", withField("frequency_penalty", "0.5")},
		{"presence_penalty", withField("presence_penalty", "0.5")},
		{"reasoning_effort", withField("reasoning_effort", `"high"`)},
		{"logit_bias", withField("logit_bias", `{"50256":-100}`)},
		{"parallel_tool_calls", withField("parallel_tool_calls", "false")},
		{"modalities", withField("modalities", `["text"]`)},
		{"unknown pass-through field", withField("x_vendor_option", `{"a":1}`)},
		{"null field", withField("x_vendor_option", `null`)},
	}
	seen := map[string]string{base: "base"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonical(t, tt.body, "gpt-4o")
			if prev, dup := seen[got]; dup {
				t.Errorf("canonical form equals that of %s: %s", prev, got)
			}
			seen[got] = tt.name
		})
	}

	t.Run("target", func(t *testing.T) {
		for _, target := range []string{"gpt-4o-mini", "auto:balanced@default", "auto:cheap@default", "auto:balanced@other"} {
			if got := canonical(t, baseBody, target); got == base {
				t.Errorf("target %q gives the same canonical form as gpt-4o", target)
			}
		}
		if canonical(t, baseBody, "auto:balanced@default") == canonical(t, baseBody, "auto:balanced@other") {
			t.Error("auto targets of different policies share a form")
		}
	})

	t.Run("large integers keep their digits", func(t *testing.T) {
		// Both seeds round to the same float64.
		a := canonical(t, withField("seed", "9007199254740993"), "m")
		b := canonical(t, withField("seed", "9007199254740992"), "m")
		if a == b {
			t.Error("seeds that differ beyond float64 precision share a form")
		}
	})
}

func TestCanonicalRejectsInvalid(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"a` + "\xff" + `"}]}`
	r, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Skipf("the decoder already rejects invalid UTF-8: %v", err)
	}
	if _, err := Canonical(r, "m"); err == nil {
		t.Error("Canonical accepted a request that is not valid UTF-8")
	}
}

// TestCanonicalLoneSurrogates: encoding/json decodes every lone UTF-16
// surrogate escape to U+FFFD, so requests that differ upstream would share
// a key; such a request is not cached at all. Valid pairs, escaped
// backslashes and other escapes are keyed as usual.
func TestCanonicalLoneSurrogates(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"lone high surrogate", `"\ud800"`, true},
		{"lone low surrogate", `"\udc00"`, true},
		{"high surrogate before another character", `"a\ud83dz"`, true},
		{"high surrogate at the end", `"a\ud83d"`, true},
		{"two high surrogates", `"\ud83d\ud83d"`, true},
		{"reversed pair", `"\ude00\ud83d"`, true},
		{"in a nested field", `[{"type":"text","text":"\uDBFF"}]`, true},
		{"valid pair", `"\ud83d\ude00"`, false},
		{"upper-case pair", `"\uD83D\uDE00"`, false},
		{"escaped backslash before u", `"\\ud800"`, false},
		{"other escapes", `"tab\t quote\" \u00e9 \u20ac"`, false},
		{"replacement character", `"\ufffd"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":` + tt.content + `}]}`
			r, err := schema.DecodeChatRequest([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			_, cerr := Canonical(r, "m")
			_, _, ok := Partition(r, "m", "t:x", 0, 1000)
			if (cerr != nil) != tt.wantErr || ok == tt.wantErr {
				t.Errorf("Canonical error %v, Partition ok %v; want an error and no partition: %v", cerr, ok, tt.wantErr)
			}
		})
	}
}

func TestExactKey(t *testing.T) {
	c := []byte(`{"messages":[],"model":"m"}`)
	key := ExactKey("t:acme", 3, c)
	const want = "sy:x:v1:t:acme:3:"
	if !strings.HasPrefix(key, want) || len(key) != len(want)+64 {
		t.Fatalf("ExactKey = %q, want %q followed by 64 hex digits", key, want)
	}
	keys := map[string]string{
		key:                                 "base",
		ExactKey("t:globex", 3, c):          "other tenant",
		ExactKey("t:acme:k:k1", 3, c):       "key scope",
		ExactKey("t:acme", 4, c):            "next namespace version",
		ExactKey("t:acme", 3, []byte(`{}`)): "other request",
	}
	if len(keys) != 5 {
		t.Errorf("keys collide: %v", keys)
	}
	if ExactKey("t:acme", 3, c) != key {
		t.Error("ExactKey is not deterministic")
	}
}

func TestPartition(t *testing.T) {
	const scope, target = "t:acme", "gpt-4o"
	const plain = `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"What is the capital of France?"}]}`
	tests := []struct {
		name     string
		body     string
		maxChars int
		wantOK   bool
		wantText string
	}{
		{"string content", plain, 2000, true, "What is the capital of France?"},
		{"text parts are joined", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`, 2000, true, "a\nb"},
		{"n is 1", `{"model":"m","n":1,"messages":[{"role":"user","content":"q"}]}`, 2000, true, "q"},
		{"n is null", `{"model":"m","n":null,"messages":[{"role":"user","content":"q"}]}`, 2000, true, "q"},
		{"exactly max chars (runes)", `{"model":"m","messages":[{"role":"user","content":"ééé"}]}`, 3, true, "ééé"},
		{"over max chars", `{"model":"m","messages":[{"role":"user","content":"éééé"}]}`, 3, false, ""},
		{"last message is assistant", `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"}]}`, 2000, false, ""},
		{"last message is a tool result", `{"model":"m","messages":[{"role":"tool","tool_call_id":"x","content":"r"}]}`, 2000, false, ""},
		{"null content", `{"model":"m","messages":[{"role":"user","content":null}]}`, 2000, false, ""},
		{"empty text", `{"model":"m","messages":[{"role":"user","content":"  "}]}`, 2000, false, ""},
		{"image part in last message", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, 2000, false, ""},
		{"image in an earlier message", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"and?"}]}`, 2000, false, ""},
		{"audio in an earlier message", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]},{"role":"user","content":"q"}]}`, 2000, false, ""},
		{"part type matched exactly", `{"model":"m","messages":[{"role":"user","content":[{"TYPE":"text","type":"image_url","text":"a"}]}]}`, 2000, false, ""},
		{"n is 2", `{"model":"m","n":2,"messages":[{"role":"user","content":"q"}]}`, 2000, false, ""},
		{"n is not an integer", `{"model":"m","n":"1","messages":[{"role":"user","content":"q"}]}`, 2000, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, text, ok := Partition(chatRequest(t, tt.body), target, scope, 0, tt.maxChars)
			if ok != tt.wantOK || text != tt.wantText {
				t.Fatalf("Partition = (%q, %q, %v), want text %q, ok %v", p, text, ok, tt.wantText, tt.wantOK)
			}
			if ok && len(p) != 64 {
				t.Errorf("partition %q is not 64 hex digits", p)
			}
		})
	}

	part := func(t *testing.T, body, target, scope string, nsver int64) string {
		t.Helper()
		p, _, ok := Partition(chatRequest(t, body), target, scope, nsver, 2000)
		if !ok {
			t.Fatalf("Partition(%s) is not ok", body)
		}
		return p
	}
	base := part(t, plain, target, scope, 0)

	t.Run("same partition", func(t *testing.T) {
		for name, body := range map[string]string{
			"other last message": strings.Replace(plain, "France", "Germany", 1),
			"stream and user":    strings.Replace(plain, `"model":"m"`, `"model":"m","stream":true,"user":"u"`, 1),
		} {
			if got := part(t, body, target, scope, 0); got != base {
				t.Errorf("%s: partition changed", name)
			}
		}
	})

	t.Run("other partition", func(t *testing.T) {
		others := map[string]string{
			"system prompt":   part(t, strings.Replace(plain, `"s"`, `"t"`, 1), target, scope, 0),
			"earlier message": part(t, strings.Replace(plain, `{"role":"system","content":"s"}`, `{"role":"system","content":"s"},{"role":"user","content":"x"},{"role":"assistant","content":"y"}`, 1), target, scope, 0),
			"last name":       part(t, strings.Replace(plain, `{"role":"user",`, `{"role":"user","name":"bob",`, 1), target, scope, 0),
			"tools":           part(t, strings.Replace(plain, `"model":"m"`, `"model":"m","tools":[{"type":"function","function":{"name":"f"}}]`, 1), target, scope, 0),
			"response_format": part(t, strings.Replace(plain, `"model":"m"`, `"model":"m","response_format":{"type":"json_object"}`, 1), target, scope, 0),
			"temperature":     part(t, strings.Replace(plain, `"model":"m"`, `"model":"m","temperature":0`, 1), target, scope, 0),
			"target":          part(t, plain, "gpt-4o-mini", scope, 0),
			"auto target":     part(t, plain, "auto:balanced@default", scope, 0),
			"tenant":          part(t, plain, target, "t:globex", 0),
			"key scope":       part(t, plain, target, "t:acme:k:k1", 0),
			"nsver":           part(t, plain, target, scope, 1),
		}
		seen := map[string]string{base: "base"}
		for name, p := range others {
			if prev, dup := seen[p]; dup {
				t.Errorf("%s has the same partition as %s", name, prev)
			}
			seen[p] = name
		}
	})
}

func TestDeterministic(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"temperature 0", `{"temperature":0}`, true},
		{"temperature 0.0", `{"temperature":0.0}`, true},
		{"seed", `{"temperature":1,"seed":42}`, true},
		{"seed 0", `{"seed":0}`, true},
		{"temperature absent", `{}`, false},
		{"temperature null", `{"temperature":null}`, false},
		{"temperature 0.7", `{"temperature":0.7}`, false},
		{"seed null", `{"seed":null,"temperature":0.5}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(tt.body, "{", `{"model":"m","messages":[{"role":"user","content":"q"}],`, 1)
			body = strings.Replace(body, ",}", "}", 1)
			if got := Deterministic(chatRequest(t, body)); got != tt.want {
				t.Errorf("Deterministic(%s) = %v, want %v", body, got, tt.want)
			}
		})
	}
}

// BenchmarkExactKey measures the key derivation every cached request pays:
// the canonical form of a typical request and its hash.
func BenchmarkExactKey(b *testing.B) {
	r, err := schema.DecodeChatRequest([]byte(baseBody))
	if err != nil {
		b.Fatal(err)
	}
	scope := ScopeID(config.CacheScopeTenant, "acme", "k1", "")
	b.ReportAllocs()
	for b.Loop() {
		c, err := Canonical(r, "gpt-4o")
		if err != nil {
			b.Fatal(err)
		}
		_ = ExactKey(scope, 0, c)
	}
}
