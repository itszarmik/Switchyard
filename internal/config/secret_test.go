package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const plaintext = "sk-very-secret-value-0123456789"

func secretConfig() *Config {
	return &Config{
		Version: 1,
		Storage: Storage{
			Redis:  RedisStorage{CacheURL: NewSecret("redis://:" + plaintext + "@c:6379"), StateURL: NewSecret(plaintext + "-state")},
			Ledger: Ledger{DSN: NewSecret("postgres://u:" + plaintext + "@db/x")},
		},
		Providers: []Provider{{ID: "p", APIKey: NewSecret(plaintext)}},
	}
}

func TestSecretMethods(t *testing.T) {
	s := NewSecret(plaintext)
	if s.Value() != plaintext || !s.IsSet() {
		t.Fatal("Value/IsSet")
	}
	var empty Secret
	if empty.IsSet() || empty.Value() != "" {
		t.Fatal("empty Secret")
	}
	if NewSecret("").IsSet() || NewSecret("").Value() != "" {
		t.Fatal("NewSecret(\"\")")
	}
	if c := s; c.Value() != plaintext {
		t.Fatal("copied Secret lost its value")
	}

	tests := []struct {
		name  string
		got   func(Secret) string
		set   string
		unset string
	}{
		{"String", Secret.String, redacted, ""},
		{"GoString", Secret.GoString, redacted, ""},
		{"%v", func(s Secret) string { return fmt.Sprintf("%v", s) }, redacted, ""},
		{"%+v", func(s Secret) string { return fmt.Sprintf("%+v", s) }, redacted, ""},
		{"%#v", func(s Secret) string { return fmt.Sprintf("%#v", s) }, redacted, ""},
		{"%s", func(s Secret) string { return fmt.Sprintf("%s", s) }, redacted, ""},
		{"%q", func(s Secret) string { return fmt.Sprintf("%q", s) }, `"[REDACTED]"`, `""`},
		{"%x", func(s Secret) string { return fmt.Sprintf("%x", s) }, redacted, ""},
		{"%d", func(s Secret) string { return fmt.Sprintf("%d", s) }, redacted, ""},
		{"%12s", func(s Secret) string { return fmt.Sprintf("%12s", s) }, "  " + redacted, strings.Repeat(" ", 12)},
		{"pointer %v", func(s Secret) string { return fmt.Sprintf("%v", &s) }, redacted, ""},
		{"LogValue", func(s Secret) string { return s.LogValue().String() }, redacted, ""},
		{"MarshalJSON", func(s Secret) string { b, _ := s.MarshalJSON(); return string(b) }, `"[REDACTED]"`, `""`},
		{"MarshalText", func(s Secret) string { b, _ := s.MarshalText(); return string(b) }, redacted, ""},
		{"MarshalYAML", func(s Secret) string { v, _ := s.MarshalYAML(); return fmt.Sprint(v) }, redacted, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got(s); got != tt.set {
				t.Errorf("set: got %q, want %q", got, tt.set)
			}
			if got := tt.got(empty); got != tt.unset {
				t.Errorf("empty: got %q, want %q", got, tt.unset)
			}
		})
	}
}

func TestSecretUnmarshalYAML(t *testing.T) {
	tests := []struct {
		src     string
		want    string
		wantErr bool
	}{
		{`k: abc`, "abc", false},
		{`k: "quoted"`, "quoted", false},
		{`k: 12345`, "12345", false},
		{`k: ~`, "", false},
		{`k: [a]`, "", true},
		{`k: {a: b}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			var v struct {
				K Secret `yaml:"k"`
			}
			err := yaml.Unmarshal([]byte(tt.src), &v)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if v.K.Value() != tt.want {
				t.Errorf("value = %q, want %q", v.K.Value(), tt.want)
			}
		})
	}
}

func TestSecretsNeverLeak(t *testing.T) {
	cfg := secretConfig()
	var outputs []struct{ name, out string }
	add := func(name, out string) { outputs = append(outputs, struct{ name, out string }{name, out}) }

	// A struct holding a Secret in an unexported field: fmt cannot call the
	// Secret's methods through it and prints the fields by reflection.
	type holder struct {
		name string
		key  Secret
	}
	h := holder{name: "h", key: cfg.Providers[0].APIKey}
	snap := &Snapshot{cfg: cfg, version: "v"}
	// %p, %t and %d are bad verbs for these types; fmt then prints the value
	// by reflection without calling any method.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%p", "%t", "%d"} {
		add("Secret "+verb, fmt.Sprintf(verb, cfg.Providers[0].APIKey))
		add("*Secret "+verb, fmt.Sprintf(verb, &cfg.Providers[0].APIKey))
		add("Provider "+verb, fmt.Sprintf(verb, cfg.Providers[0]))
		add("*Provider "+verb, fmt.Sprintf(verb, &cfg.Providers[0]))
		add("Config "+verb, fmt.Sprintf(verb, *cfg))
		add("*Config "+verb, fmt.Sprintf(verb, cfg))
		add("*Snapshot "+verb, fmt.Sprintf(verb, snap))
		add("Snapshot "+verb, fmt.Sprintf(verb, *snap))
		add("unexported field "+verb, fmt.Sprintf(verb, h))
		add("*unexported field "+verb, fmt.Sprintf(verb, &h))
	}

	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	add("json.Marshal", string(j))

	y, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	add("yaml.Marshal", string(y))

	for _, h := range []struct {
		name string
		mk   func(*bytes.Buffer) slog.Handler
	}{
		{"slog JSON", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
		{"slog text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
	} {
		var buf bytes.Buffer
		l := slog.New(h.mk(&buf))
		l.Info("config", "cfg", cfg, "provider", cfg.Providers[0], "key", cfg.Providers[0].APIKey)
		l.Info("group", slog.Group("g", slog.Any("cfg", *cfg)))
		add(h.name, buf.String())
	}

	for _, o := range outputs {
		t.Run(o.name, func(t *testing.T) {
			if strings.Contains(o.out, plaintext) {
				t.Errorf("output leaks the secret:\n%s", o.out)
			}
		})
	}

	// Sanity check: the JSON output shows the placeholder.
	if !strings.Contains(string(j), `"[REDACTED]"`) {
		t.Errorf("json output lacks the placeholder: %s", j)
	}
}

func TestSecretsNeverLeakFromLoadErrors(t *testing.T) {
	env := map[string]string{"SECRET": plaintext, "REDIS": "redis://:" + plaintext + "@bad host:6379"}
	tests := []struct {
		name string
		root string
		want want
	}{
		{
			name: "type error on an expanded secret reference",
			root: "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    max_concurrency: ${SECRET}\n",
			want: errAt("providers[0].max_concurrency", "expected an integer, got the value of ${SECRET}"),
		},
		{
			name: "duration from a secret reference",
			root: "version: 1\nserver:\n  idle_timeout: ${SECRET}\n",
			want: errAt("server.idle_timeout", "got the value of ${SECRET}"),
		},
		{
			name: "enum from a secret reference",
			root: "version: 1\nlogging:\n  level: ${SECRET}\n",
			want: errAt("logging.level", "got the value of ${SECRET}"),
		},
		{
			name: "unparseable redis URL with a password",
			root: "version: 1\nstorage:\n  mode: redis\n  redis:\n    cache_url: ${REDIS}\n    state_url: redis://localhost:6380\n",
			want: errAt("storage.redis.cache_url", "must be a redis:// or rediss:// URL with a host"),
		},
		{
			name: "literal unparseable redis URL with a password",
			root: "version: 1\nstorage:\n  mode: redis\n  redis:\n    cache_url: 'redis://:" + plaintext + "@bad host:6379'\n    state_url: redis://localhost:6380\n",
			want: errAt("storage.redis.cache_url", "must be a redis:// or rediss:// URL with a host"),
		},
		{
			name: "base_url with credentials",
			root: "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://u:" + plaintext + "@p.example\n",
			want: errAt("providers[0].base_url", "must not embed credentials"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := mustFail(t, tt.root, env, nil)
			// Literal credentials also get warnings; only the error matters here.
			checkProblems(t, ve.Errors(), tt.want)
			if strings.Contains(ve.Error(), plaintext) {
				t.Errorf("ValidationError leaks the secret:\n%s", ve.Error())
			}
		})
	}
}

func TestSecretValues(t *testing.T) {
	cfg := secretConfig()
	cfg.Providers = append(cfg.Providers, Provider{ID: "dup", APIKey: NewSecret(plaintext)}, Provider{ID: "none"})
	got := cfg.SecretValues()
	want := []string{
		"redis://:" + plaintext + "@c:6379",
		plaintext + "-state",
		"postgres://u:" + plaintext + "@db/x",
		plaintext,
	}
	if !slices.Equal(got, want) {
		t.Errorf("SecretValues() = %q, want %q", got, want)
	}
	var nilCfg *Config
	if v := nilCfg.SecretValues(); v != nil {
		t.Errorf("nil config: %q", v)
	}
}

// TestLiteralCredentialsNeverEchoed covers values and keys that look like
// credentials but sit where the decoder or validator would otherwise quote
// them.
func TestLiteralCredentialsNeverEchoed(t *testing.T) {
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz0123"
	const pw = "Hunter2Pass"
	tests := []struct {
		name  string
		root  string
		wants []want
	}{
		{
			name:  "URL with a password where a mapping is expected",
			root:  "version: 1\nstorage:\n  redis: 'redis://:" + pw + "@cache:6379'\n",
			wants: []want{errAt("storage.redis", "expected a mapping, got a string value")},
		},
		{
			name:  "key where a sequence is expected",
			root:  "version: 1\nproviders:\n" + prov("a", "tags: "+key),
			wants: []want{errAt("providers[0].tags", "expected a sequence, got a string value")},
		},
		{
			name:  "short prefixed key where an integer is expected",
			root:  "version: 1\nproviders:\n" + prov("a", "max_concurrency: sk-live-123456"),
			wants: []want{errAt("providers[0].max_concurrency", "expected an integer, got a string value")},
		},
		{
			name:  "key where a provider is expected",
			root:  "version: 1\nproviders:\n  - '" + key + "'\n",
			wants: []want{errAt("providers[0]", "expected a mapping, got a string value")},
		},
		{
			name:  "key as an unknown field name",
			root:  "version: 1\nproviders:\n" + prov("a", key+": x"),
			wants: []want{errAt("providers[0].<hidden key>", "unknown field (its name looks like a credential, so it is not shown)")},
		},
		{
			name:  "URL with a password as an unknown field name",
			root:  "version: 1\nserver:\n  'redis://:" + pw + "@cache': x\n",
			wants: []want{errAt("server.<hidden key>", "unknown field")},
		},
		{
			name:  "reference error under a credential-like key",
			root:  "version: 1\nserver:\n  " + key + ": ${NOPE}\n",
			wants: []want{errAt("server.<hidden key>", "unknown field"), errAt("server.<hidden key>", "environment variable NOPE is not set")},
		},
		{
			name:  "key in an enum",
			root:  "version: 1\nlogging:\n  level: " + key + "\n",
			wants: []want{errAt("logging.level", "must be one of debug, info, warn, error, got a string value")},
		},
		{
			name:  "bearer token in an enum",
			root:  "version: 1\nlogging:\n  level: 'Bearer abcdefgh12345678'\n",
			wants: []want{errAt("logging.level", "got a string value")},
		},
		{
			name:  "URL password in a listen address",
			root:  "version: 1\nserver:\n  listen: 'redis://:" + pw + "@cache:6379'\n",
			wants: []want{errAt("server.listen", "got a string value")},
		},
		{
			name:  "template with a literal password",
			root:  "version: 1\nserver:\n  idle_timeout: 'redis://:" + pw + "@${HOST}'\n",
			wants: []want{errAt("server.idle_timeout", "got the expansion of a value that references ${HOST}")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := mustFail(t, tt.root, map[string]string{"HOST": "cache"}, nil)
			checkProblems(t, ve.Errors(), tt.wants...)
			for _, s := range []string{key, pw, "sk-live-123456", "abcdefgh12345678"} {
				if strings.Contains(ve.Error(), s) {
					t.Errorf("ValidationError leaks %q:\n%s", s, ve.Error())
				}
			}
		})
	}

	t.Run("ordinary values are still quoted", func(t *testing.T) {
		ve := mustFail(t, "version: 1\nlogging:\n  level: verbose\nserver:\n  listen: ':99999'\n", nil, nil)
		checkProblems(t, ve.Problems,
			errAt("logging.level", `got "verbose"`),
			errAt("server.listen", `got ":99999"`))
	})
}

// TestReferencedValuesNeverEchoed covers validation messages about values
// that came from environment references.
func TestReferencedValuesNeverEchoed(t *testing.T) {
	env := map[string]string{"SECRET": plaintext, "HDR": "Authorization"}
	dep := "    deployments: [{provider: p, upstream_model: x}]\n"
	tests := []struct {
		name  string
		root  string
		wants []want
	}{
		{
			name:  "duplicate provider id",
			root:  "version: 1\nproviders:\n" + prov("${SECRET}") + prov("${SECRET}"),
			wants: []want{errAt("providers[1].id", "duplicate provider id the value of ${SECRET}; first defined at cfg.yaml:3")},
		},
		{
			name:  "duplicate tag",
			root:  "version: 1\nproviders:\n" + prov("p", `tags: ["${SECRET}", "${SECRET}"]`),
			wants: []want{errAt("providers[0].tags[1]", "duplicate tag the value of ${SECRET}")},
		},
		{
			name:  "duplicate forward header",
			root:  "version: 1\nproviders:\n" + prov("p", `forward_headers: ["${SECRET}", "${SECRET}"]`),
			wants: []want{errAt("providers[0].forward_headers[1]", "duplicate header the value of ${SECRET}")},
		},
		{
			name:  "forbidden forward header",
			root:  "version: 1\nproviders:\n" + prov("p", `forward_headers: ["${HDR}"]`),
			wants: []want{errAt("providers[0].forward_headers[0]", "the value of ${HDR} can never be forwarded upstream")},
		},
		{
			name:  "duplicate model id",
			root:  "version: 1\nproviders:\n" + prov("p") + "models:\n  - id: ${SECRET}\n" + dep + "  - id: ${SECRET}\n" + dep,
			wants: []want{errAt("models[1].id", "duplicate model id the value of ${SECRET}")},
		},
		{
			name:  "unknown provider",
			root:  "version: 1\nproviders:\n" + prov("p") + "models:\n  - id: m\n    deployments: [{provider: '${SECRET}', upstream_model: x}]\n",
			wants: []want{errAt("models[0].deployments[0].provider", `unknown provider the value of ${SECRET} (known providers: "p")`)},
		},
		{
			name:  "provider twice in one model",
			root:  "version: 1\nproviders:\n" + prov("${SECRET}") + "models:\n  - id: m\n    deployments: [{provider: '${SECRET}', upstream_model: x}, {provider: '${SECRET}', upstream_model: y}]\n",
			wants: []want{errAt("models[0].deployments[1].provider", "provider the value of ${SECRET} already serves this model")},
		},
		{
			name:  "suggested and known providers named by reference",
			root:  "version: 1\nproviders:\n" + prov("${SECRET}") + "models:\n  - id: m\n    deployments: [{provider: sk-very-secret-value-012345678, upstream_model: x}]\n",
			wants: []want{errAt("models[0].deployments[0].provider", "(did you mean the value of ${SECRET}?)")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := mustFail(t, tt.root, env, nil)
			checkProblems(t, ve.Errors(), tt.wants...)
			if strings.Contains(ve.Error(), plaintext) {
				t.Errorf("ValidationError leaks the value:\n%s", ve.Error())
			}
		})
	}
}
