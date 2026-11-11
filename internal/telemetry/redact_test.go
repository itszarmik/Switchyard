package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// Fake credentials, shaped like the real thing.
const (
	exactSecret = "hunter2-correct-horse"
	openAIKey   = "sk-proj-AbCdEfGhIjKlMnOpQrStUv"
	anthropic   = "sk-ant-api03-AbCdEfGhIjKlMnOp_qr-st"
	gatewayKey  = "sy_AbCdEfGhIjKlMnOpQrStUvWx"
	googleKey   = "AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz012345"
	bearerTok   = "eyJhbGciOi.payload.sig"
)

// logJSON logs through a JSON logger wrapped in the redacting handler and
// returns the decoded records with time removed.
func logJSON(t *testing.T, secrets []string, fn func(l *slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	l := NewLogger(&buf, LogOptions{Level: slog.LevelDebug, Secrets: secrets})
	fn(l)
	var recs []map[string]any
	dec := json.NewDecoder(&buf)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode log output: %v", err)
		}
		delete(m, "time")
		recs = append(recs, m)
	}
	return recs
}

func logOne(t *testing.T, secrets []string, fn func(l *slog.Logger)) map[string]any {
	t.Helper()
	recs := logJSON(t, secrets, fn)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	return recs[0]
}

func TestSensitiveKeys(t *testing.T) {
	tests := []struct {
		key    string
		redact bool
	}{
		{"authorization", true},
		{"Authorization", true},
		{"Proxy-Authorization", true},
		{"cookie", true},
		{"Set-Cookie", true},
		{"api_key", true},
		{"apiKey", true},
		{"X-Api-Key", true},
		{"password", true},
		{"PASSWD", true},
		{"secret", true},
		{"client_secret", true},
		{"token", true},
		{"access_token", true},
		{"refresh-token", true},
		{"id.token", true},
		{"bearer_token", true},
		{"auth_token", true},
		{"dsn", true},
		{"private_key", true},
		// Suffix rules.
		{"openai_api_key", true},
		{"upstream.secret", true},
		{"db_password", true},
		{"admin_passwd", true},
		{"ssh-private-key", true},
		{"a_very_long_key_name_that_does_not_fit_in_the_stack_buffer_api_key", true},
		{"secret_key", true},
		{"aws_secret_access_key", true},
		{"access_key", true},
		{"signing_key", true},
		{"webhook_signing_key", true},
		{"session_token", true},
		{"aws_session_token", true},
		{"credential", true},
		{"credentials", true},
		{"gcp_credentials", true},
		{"passphrase", true},
		{"ssh_passphrase", true},
		{"X-Auth-Token", true},
		{"database_url", true},
		{"DATABASE_URL", true},
		{"replica_database_url", true},
		// Not secrets.
		{"first_token", false},
		{"input_tokens", false},
		{"output_tokens", false},
		{"max_tokens", false},
		{"tokenizer", false},
		{"secretary", false},
		{"model", false},
		{"key_id", false},
		{"access_key_id", false},
		{"credential_id", false},
		{"database", false},
		{"a_very_long_key_name_that_does_not_fit_in_the_stack_buffer_at_all", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			rec := logOne(t, nil, func(l *slog.Logger) {
				l.Info("m", tt.key, "plain-value", "n_"+tt.key, 42)
			})
			want := any("plain-value")
			if tt.redact {
				want = Redacted
			}
			if got := rec[tt.key]; got != want {
				t.Errorf("%s = %v, want %v", tt.key, got, want)
			}
			// Non-string values under a sensitive key are redacted too.
			intKey := "n_" + tt.key
			wantInt := any(float64(42))
			if isSensitiveKey(intKey) {
				wantInt = Redacted
			}
			if got := rec[intKey]; got != wantInt {
				t.Errorf("%s = %v, want %v", intKey, got, wantInt)
			}
		})
	}
}

func TestCredentialPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bearer", "Authorization: Bearer " + bearerTok, "Authorization: Bearer " + Redacted},
		{"bearer lowercase", "got bearer " + bearerTok + " here", "got Bearer " + Redacted + " here"},
		{"bearer tab", "BEARER\t" + bearerTok, "Bearer " + Redacted},
		{"bearer padding", "Bearer abcdefgh12==", "Bearer " + Redacted},
		{"bearer too short", "Bearer abc", "Bearer abc"},
		{"bearer word only", "the bearer of news", "the bearer of news"},
		{"openai project key", "key=" + openAIKey, "key=" + Redacted},
		{"anthropic key", "x " + anthropic + " y", "x " + Redacted + " y"},
		{"sk too short", "sk-short", "sk-short"},
		{"gateway key", "used " + gatewayKey, "used " + Redacted},
		{"sy too short", "sy_abc", "sy_abc"},
		{"google key", googleKey, Redacted},
		{"postgres url", "postgres://app:s3cr3t@db:5432/ledger", "postgres://app:" + Redacted + "@db:5432/ledger"},
		{"redis no user", "redis://:pw123456@cache:6379/0", "redis://:" + Redacted + "@cache:6379/0"},
		{"password with at", "postgres://u:p@ss@db/x", "postgres://u:" + Redacted + "@db/x"},
		{"url without password", "https://example.com:8080/v1?x=1", "https://example.com:8080/v1?x=1"},
		{"url user only", "https://user@example.com/", "https://user@example.com/"},
		{"two urls", "a http://u:p1@h1 b http://v:p2@h2", "a http://u:" + Redacted + "@h1 b http://v:" + Redacted + "@h2"},
		{"clean", "nothing to see here", "nothing to see here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := logOne(t, nil, func(l *slog.Logger) {
				l.Info(tt.in, "v", tt.in, "err", errors.New(tt.in), "s", stringer(tt.in))
			})
			for _, k := range []string{"msg", "v", "err", "s"} {
				if got := rec[k]; got != tt.want {
					t.Errorf("%s = %q, want %q", k, got, tt.want)
				}
			}
		})
	}
}

type stringer string

func (s stringer) String() string { return string(s) }

type valuer struct{ v slog.Value }

func (v valuer) LogValue() slog.Value { return v.v }

type creds struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

type hidden struct {
	Name string
	key  string
}

func TestExactSecrets(t *testing.T) {
	tests := []struct {
		name string
		log  func(l *slog.Logger)
		want map[string]any
	}{
		{
			name: "message and string value",
			log:  func(l *slog.Logger) { l.Info("using "+exactSecret, "v", "pre"+exactSecret+"post") },
			want: map[string]any{"msg": "using " + Redacted, "v": "pre" + Redacted + "post"},
		},
		{
			name: "every occurrence",
			log:  func(l *slog.Logger) { l.Info("m", "v", exactSecret+" and "+exactSecret) },
			want: map[string]any{"msg": "m", "v": Redacted + " and " + Redacted},
		},
		{
			name: "error",
			log: func(l *slog.Logger) {
				l.Info("m", "err", fmt.Errorf("connect: %w", errors.New("auth "+exactSecret+" rejected")))
			},
			want: map[string]any{"msg": "m", "err": "connect: auth " + Redacted + " rejected"},
		},
		{
			name: "stringer",
			log:  func(l *slog.Logger) { l.Info("m", "s", stringer("k="+exactSecret)) },
			want: map[string]any{"msg": "m", "s": "k=" + Redacted},
		},
		{
			name: "struct becomes scrubbed JSON",
			log:  func(l *slog.Logger) { l.Info("m", "c", creds{User: "bob", Pass: exactSecret}) },
			want: map[string]any{"msg": "m", "c": `{"user":"bob","pass":"` + Redacted + `"}`},
		},
		{
			name: "map becomes scrubbed JSON",
			log:  func(l *slog.Logger) { l.Info("m", "c", map[string]string{"x": exactSecret}) },
			want: map[string]any{"msg": "m", "c": `{"x":"` + Redacted + `"}`},
		},
		{
			name: "clean struct is kept as is",
			log:  func(l *slog.Logger) { l.Info("m", "c", creds{User: "bob", Pass: "fine"}) },
			want: map[string]any{"msg": "m", "c": map[string]any{"user": "bob", "pass": "fine"}},
		},
		{
			name: "struct with credential pattern",
			log:  func(l *slog.Logger) { l.Info("m", "c", creds{User: "bob", Pass: openAIKey}) },
			want: map[string]any{"msg": "m", "c": `{"user":"bob","pass":"` + Redacted + `"}`},
		},
		{
			name: "unexported field seen by the text handler",
			log:  func(l *slog.Logger) { l.Info("m", "h", hidden{Name: "n", key: exactSecret}) },
			want: map[string]any{"msg": "m", "h": "{Name:n key:" + Redacted + "}"},
		},
		{
			name: "byte slice",
			log:  func(l *slog.Logger) { l.Info("m", "b", []byte("x"+exactSecret)) },
			want: map[string]any{"msg": "m", "b": "x" + Redacted},
		},
		{
			name: "short secrets are ignored",
			log:  func(l *slog.Logger) { l.Info("abc is short", "v", "abc") },
			want: map[string]any{"msg": "abc is short", "v": "abc"},
		},
		{
			name: "secret containing another is removed whole",
			log:  func(l *slog.Logger) { l.Info("m", "v", "x"+exactSecret+"-extended") },
			want: map[string]any{"msg": "m", "v": "x" + Redacted},
		},
	}
	secrets := []string{exactSecret, "abc", exactSecret + "-extended", exactSecret}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := logOne(t, secrets, tt.log)
			delete(rec, "level")
			if diff := cmp.Diff(tt.want, rec); diff != "" {
				t.Errorf("record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGroupsAndChains(t *testing.T) {
	tests := []struct {
		name string
		log  func(l *slog.Logger)
		want map[string]any
	}{
		{
			name: "nested groups",
			log: func(l *slog.Logger) {
				l.Info("m", slog.Group("a",
					slog.String("model", "gpt"),
					slog.Group("b",
						slog.String("api_key", "plain"),
						slog.String("note", "Bearer "+bearerTok),
						slog.Int("input_tokens", 12),
						slog.Group("c", slog.String("deep", exactSecret)),
					),
				))
			},
			want: map[string]any{"msg": "m", "a": map[string]any{
				"model": "gpt",
				"b": map[string]any{
					"api_key":      Redacted,
					"note":         "Bearer " + Redacted,
					"input_tokens": float64(12),
					"c":            map[string]any{"deep": Redacted},
				},
			}},
		},
		{
			name: "group with a sensitive key is redacted whole",
			log: func(l *slog.Logger) {
				l.Info("m", slog.Group("credentials_secret", slog.String("user", "u"), slog.Int("n", 1)))
			},
			want: map[string]any{"msg": "m", "credentials_secret": Redacted},
		},
		{
			name: "With and WithGroup chain",
			log: func(l *slog.Logger) {
				l.With("authorization", "Basic abc", "first_token", "ok").
					WithGroup("upstream").
					With("url", "https://u:pw123456@api.example.com/v1", "provider", "openai").
					WithGroup("req").
					Info("call", "key", openAIKey, "max_tokens", 100)
			},
			want: map[string]any{
				"msg":           "call",
				"authorization": Redacted,
				"first_token":   "ok",
				"upstream": map[string]any{
					"url":      "https://u:" + Redacted + "@api.example.com/v1",
					"provider": "openai",
					"req":      map[string]any{"key": Redacted, "max_tokens": float64(100)},
				},
			},
		},
		{
			name: "WithGroup with a sensitive name redacts everything inside",
			log: func(l *slog.Logger) {
				l.With("model", "m1").WithGroup("password").With("a", "x").Info("m", "b", 2)
			},
			want: map[string]any{
				"msg":      "m",
				"model":    "m1",
				"password": map[string]any{"a": Redacted, "b": Redacted},
			},
		},
		{
			name: "LogValuer resolving to a secret string",
			log:  func(l *slog.Logger) { l.Info("m", "v", valuer{slog.StringValue("t=" + gatewayKey)}) },
			want: map[string]any{"msg": "m", "v": "t=" + Redacted},
		},
		{
			name: "LogValuer resolving to a group",
			log: func(l *slog.Logger) {
				l.Info("m", "v", valuer{slog.GroupValue(slog.String("token", "x"), slog.String("ok", "y"))})
			},
			want: map[string]any{"msg": "m", "v": map[string]any{"token": Redacted, "ok": "y"}},
		},
		{
			name: "LogValuer inside a group",
			log: func(l *slog.Logger) {
				l.Info("m", slog.Group("g", slog.Any("v", valuer{slog.StringValue(exactSecret)})))
			},
			want: map[string]any{"msg": "m", "g": map[string]any{"v": Redacted}},
		},
		{
			name: "attrs before and after the first redaction keep their order",
			log: func(l *slog.Logger) {
				l.Info("m", "a", 1, "b", exactSecret, "c", 3, "d", "Bearer "+bearerTok)
			},
			want: map[string]any{"msg": "m", "a": float64(1), "b": Redacted, "c": float64(3), "d": "Bearer " + Redacted},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := logOne(t, []string{exactSecret}, tt.log)
			delete(rec, "level")
			if diff := cmp.Diff(tt.want, rec); diff != "" {
				t.Errorf("record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAttrOrderPreserved(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, LogOptions{Secrets: []string{exactSecret}})
	l.Info("m", "a", 1, "b", exactSecret, "c", 3)
	out := buf.String()
	ia, ib, ic := strings.Index(out, `"a"`), strings.Index(out, `"b"`), strings.Index(out, `"c"`)
	if ia < 0 || ia >= ib || ib >= ic {
		t.Fatalf("attr order changed: %s", out)
	}
}

// recordingHandler keeps the records it is given.
type recordingHandler struct {
	level slog.Level
	recs  []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.recs = append(h.recs, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestHandleDoesNotMutateRecord(t *testing.T) {
	inner := &recordingHandler{}
	h := NewRedactingHandler(inner, []string{exactSecret})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg "+exactSecret, 0)
	r.AddAttrs(slog.String("a", "ok"), slog.String("b", exactSecret), slog.String("api_key", "x"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	if r.Message != "msg "+exactSecret {
		t.Errorf("caller's message changed to %q", r.Message)
	}
	var orig []string
	r.Attrs(func(a slog.Attr) bool { orig = append(orig, a.String()); return true })
	if want := []string{"a=ok", "b=" + exactSecret, "api_key=x"}; !cmp.Equal(orig, want) {
		t.Errorf("caller's attrs changed: %v", orig)
	}

	if len(inner.recs) != 1 {
		t.Fatalf("inner handler got %d records", len(inner.recs))
	}
	var got []string
	inner.recs[0].Attrs(func(a slog.Attr) bool { got = append(got, a.String()); return true })
	if want := []string{"a=ok", "b=" + Redacted, "api_key=" + Redacted}; !cmp.Equal(got, want) {
		t.Errorf("inner record attrs = %v, want %v", got, want)
	}
	if inner.recs[0].Message != "msg "+Redacted {
		t.Errorf("inner message = %q", inner.recs[0].Message)
	}
}

func TestEnabledDelegates(t *testing.T) {
	h := NewRedactingHandler(&recordingHandler{level: slog.LevelWarn}, nil)
	ctx := context.Background()
	if h.Enabled(ctx, slog.LevelInfo) || !h.Enabled(ctx, slog.LevelWarn) {
		t.Fatal("Enabled does not follow the inner handler")
	}
	if !h.WithGroup("g").WithAttrs([]slog.Attr{slog.Int("a", 1)}).Enabled(ctx, slog.LevelError) {
		t.Fatal("derived handler does not follow the inner handler")
	}
}

type panicStringer struct{}

func (*panicStringer) String() string { panic("boom") }

func TestPanickingValuesPassThrough(t *testing.T) {
	var p *panicStringer
	var buf bytes.Buffer
	l := NewLogger(&buf, LogOptions{})
	l.Info("m", "p", p) // must not panic in the redacting handler
	if !strings.Contains(buf.String(), `"msg":"m"`) {
		t.Fatalf("record not logged: %s", buf.String())
	}
}

// TestSecretsNeverAppear logs every kind of secret through both formats and
// checks that none of them reaches the output.
func TestSecretsNeverAppear(t *testing.T) {
	leaks := []string{exactSecret, openAIKey, anthropic, gatewayKey, googleKey, bearerTok, "pw123456"}
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewLogger(&buf, LogOptions{Format: format, Level: slog.LevelDebug, Secrets: []string{exactSecret}})
			l = l.With("Authorization", "Bearer "+bearerTok, "dsn", "postgres://a:pw123456@h/db").WithGroup("g")
			l.Debug("key "+openAIKey,
				"anthropic", anthropic,
				"err", fmt.Errorf("wrapped: %w", errors.New(gatewayKey)),
				"s", stringer(googleKey),
				"c", creds{Pass: exactSecret},
				"h", hidden{key: exactSecret},
				slog.Group("deep", slog.Group("er", slog.String("x_api_key", "abcdefghijkl"), slog.String("v", exactSecret))),
				"lv", valuer{slog.StringValue("redis://:pw123456@x")},
			)
			out := buf.String()
			for _, s := range append(leaks, "abcdefghijkl") {
				if strings.Contains(out, s) {
					t.Errorf("output contains %q: %s", s, out)
				}
			}
		})
	}
}

// cleanRecord has typical access-log content with nothing to redact.
func cleanRecord() slog.Record {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	r.AddAttrs(
		slog.String("method", "POST"),
		slog.String("path", "/v1/chat/completions"),
		slog.Int("status", 200),
		slog.Int64("bytes", 1234),
		slog.Float64("duration_ms", 12.5),
		slog.String("remote_ip", "10.0.0.1"),
		slog.String("user_agent", "OpenAI/Python 1.50.0"),
		slog.Bool("stream", true),
		slog.Duration("ttft", 150*time.Millisecond),
		slog.Any("err", errors.New("upstream returned 503")),
		slog.Group("usage", slog.Int("input_tokens", 10), slog.Int("first_token", 1)),
	)
	return r
}

func TestFastPathAllocatesNothing(t *testing.T) {
	h := NewRedactingHandler(slog.DiscardHandler, []string{exactSecret, "another-secret-value"})
	r := cleanRecord()
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		_ = h.Handle(ctx, r)
	})
	if allocs != 0 {
		t.Fatalf("Handle allocated %.1f times per clean record, want 0", allocs)
	}
}

// BenchmarkHandle compares the JSON handler alone with the JSON handler
// behind the redacting handler. On a clean record the allocation counts are
// equal, which shows the fast path adds nothing.
func BenchmarkHandle(b *testing.B) {
	secrets := []string{exactSecret, "another-secret-value"}
	dirty := cleanRecord()
	dirty.AddAttrs(slog.String("authorization", "Bearer "+bearerTok))

	cases := []struct {
		name string
		h    slog.Handler
		r    slog.Record
	}{
		{"json/clean", slog.NewJSONHandler(io.Discard, nil), cleanRecord()},
		{"redacting+json/clean", NewRedactingHandler(slog.NewJSONHandler(io.Discard, nil), secrets), cleanRecord()},
		{"redacting+discard/clean", NewRedactingHandler(slog.DiscardHandler, secrets), cleanRecord()},
		{"redacting+json/dirty", NewRedactingHandler(slog.NewJSONHandler(io.Discard, nil), secrets), dirty},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				_ = c.h.Handle(ctx, c.r)
			}
		})
	}
}

// escapedForms returns s as it may appear in log output: raw, JSON-escaped
// with and without HTML escaping, and Go-quoted as the text handler writes
// it.
func escapedForms(t *testing.T, s string) []string {
	t.Helper()
	forms := []string{s, strings.Trim(strconv.Quote(s), `"`)}
	html, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	forms = append(forms, strings.Trim(string(html), `"`))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatal(err)
	}
	return append(forms, strings.Trim(strings.TrimSpace(buf.String()), `"`))
}

// TestEscapedSecrets checks that secrets with characters JSON escapes are
// still removed when they are logged inside composite values.
func TestEscapedSecrets(t *testing.T) {
	secrets := []string{"Tr0ub4dor&3xyz", "pass<word>99", `quo"te-secret`, `back\slash-secret`, "line1\nline2secret"}
	type wrapper struct {
		Note string
		List []string
	}
	for _, format := range []string{"json", "text"} {
		for _, secret := range secrets {
			t.Run(format+"/"+secret, func(t *testing.T) {
				var buf bytes.Buffer
				l := NewLogger(&buf, LogOptions{Format: format, Secrets: []string{secret}})
				l.Info("m",
					slog.Any("struct", creds{User: "bob", Pass: secret}),
					slog.Any("map", map[string]string{"x": secret}),
					slog.Any("slice", []string{"a", secret}),
					slog.Any("nested", wrapper{Note: "n", List: []string{secret}}),
					slog.Any("ptr", &creds{Pass: "x" + secret + "y"}),
				)
				out := buf.String()
				for _, form := range escapedForms(t, secret) {
					if strings.Contains(out, form) {
						t.Errorf("output contains %q:\n%s", form, out)
					}
				}
				if !strings.Contains(out, Redacted) {
					t.Errorf("output has no %s marker:\n%s", Redacted, out)
				}
			})
		}
	}
}

type headerHolder struct {
	Name    string
	Headers http.Header `json:"headers"`
	Pw      string      `json:"password"`
	Secret  string      `json:"s"`
	private map[string]string
}

// TestSensitiveKeysInsideValues checks that map keys and struct fields with
// sensitive names are redacted inside slog.Any values.
func TestSensitiveKeysInsideValues(t *testing.T) {
	const val = "plain-credential-value"
	hdr := http.Header{"Authorization": {"Basic " + val}, "X-Api-Key": {val}, "Accept": {"text/plain"}}
	tests := []struct {
		name string
		v    any
		keep []string // must still appear
	}{
		{"http.Header", hdr, []string{"Accept", "text/plain"}},
		{"map[string]string", map[string]string{"password": val, "user": "bob"}, []string{"bob"}},
		{"map[string][]string", map[string][]string{"Cookie": {val}, "model": {"gpt"}}, []string{"gpt"}},
		{"nested map", map[string]any{"req": map[string]any{"headers": hdr, "path": "/v1"}}, []string{"/v1"}},
		{"struct field by name and json tag", headerHolder{Name: "n", Headers: hdr, Pw: val, Secret: val}, []string{`Accept`}},
		{"unexported field", headerHolder{Name: "n", private: map[string]string{"token": val}}, []string{"n"}},
		{"pointer to struct", &headerHolder{Pw: val}, nil},
		{"slice of maps", []map[string]string{{"api_key": val}}, nil},
		{"named map type", namedMap{"access_token": val}, nil},
	}
	for _, format := range []string{"json", "text"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				var buf bytes.Buffer
				l := NewLogger(&buf, LogOptions{Format: format})
				l.Info("m", slog.Any("v", tt.v))
				out := buf.String()
				if strings.Contains(out, val) {
					t.Errorf("output contains the credential:\n%s", out)
				}
				for _, k := range tt.keep {
					if !strings.Contains(out, k) {
						t.Errorf("output lost %q:\n%s", k, out)
					}
				}
			})
		}
	}

	t.Run("empty sensitive values leave the value untouched", func(t *testing.T) {
		rec := logOne(t, nil, func(l *slog.Logger) {
			l.Info("m", "c", map[string]string{"password": "", "user": "bob"})
		})
		want := map[string]any{"password": "", "user": "bob"}
		if diff := cmp.Diff(want, rec["c"]); diff != "" {
			t.Errorf("value changed (-want +got):\n%s", diff)
		}
	})
}

type namedMap map[string]string

type namedBytes []byte

type panicky struct{ msg string }

func (p panicky) String() string { panic(p.msg) }

type panickyErr struct{ msg string }

func (p panickyErr) Error() string { panic(p.msg) }

// TestRedactorEdgeCases covers panicking String and Error methods and named
// byte slices.
func TestRedactorEdgeCases(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewLogger(&buf, LogOptions{Format: format, Secrets: []string{exactSecret}})
			l.Info("m",
				"stringer", panicky{msg: "boom " + exactSecret},
				"error", panickyErr{msg: "boom " + openAIKey},
				"nested", struct{ P panicky }{panicky{msg: "boom " + gatewayKey}},
				"bytes", namedBytes("x"+exactSecret),
			)
			out := buf.String()
			for _, s := range []string{exactSecret, openAIKey, gatewayKey, "boom"} {
				if strings.Contains(out, s) {
					t.Errorf("output contains %q:\n%s", s, out)
				}
			}
			if !strings.Contains(out, "!PANIC") {
				t.Errorf("output has no !PANIC marker:\n%s", out)
			}
		})
	}
	t.Run("named byte slice is scrubbed as text", func(t *testing.T) {
		rec := logOne(t, []string{exactSecret}, func(l *slog.Logger) {
			l.Info("m", "b", namedBytes("x"+exactSecret))
		})
		if rec["b"] != "x"+Redacted {
			t.Errorf("b = %v, want %q", rec["b"], "x"+Redacted)
		}
	})
}
