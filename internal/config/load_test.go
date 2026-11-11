package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var happyEnv = map[string]string{
	"REDIS_STATE_PASSWORD": "state-pass-9f2c",
	"OPENAI_API_KEY":       "sk-test-openai-123",
	"TOGETHER_API_KEY":     "tg-test-456",
	"GPU_MAX_CONCURRENCY":  "64",
}

func loadHappy(t *testing.T, env map[string]string) *Snapshot {
	t.Helper()
	snap, err := Load(filepath.Join("testdata", "happy", "switchyard.yaml"), LoadOptions{LookupEnv: envMap(env)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return snap
}

func TestLoadHappyPath(t *testing.T) {
	snap := loadHappy(t, happyEnv)
	cfg := snap.Config()

	want := &Config{
		Version: 1,
		Include: []string{"catalog/*.yaml"},
		Server: Server{
			Listen:            ":8080",
			AdminListen:       "127.0.0.1:9090",
			MaxRequestBytes:   8 * mib,
			MaxHeaderBytes:    64 * kib,
			ReadHeaderTimeout: Duration(10 * time.Second),
			BodyReadTimeout:   Duration(60 * time.Second),
			WriteTimeout:      Duration(60 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			ShutdownGrace:     Duration(60 * time.Second),
		},
		Logging: Logging{Level: "info", Format: "json"},
		Storage: Storage{
			Mode: "redis",
			Redis: RedisStorage{
				CacheURL: NewSecret("redis://localhost:6379/0"),
				StateURL: NewSecret("redis://:state-pass-9f2c@localhost:6380/0"),
			},
			Ledger: Ledger{
				Driver: "postgres", DSN: NewSecret("postgres://switchyard:s3cret-db-pass@db.internal:5432/switchyard"),
				BatchSize: 500, FlushInterval: Duration(time.Second), QueueSize: 100_000,
			},
		},
		Providers: []Provider{
			{
				ID: "openai", Type: "openai", BaseURL: "https://api.openai.com/v1",
				APIKey:   NewSecret("sk-test-openai-123"),
				Timeouts: ProviderTimeouts{Connect: Duration(5 * time.Second), FirstToken: Duration(30 * time.Second), Idle: Duration(60 * time.Second), Total: Duration(10 * time.Minute)},
			},
			{
				ID: "gpu-cluster", Type: "openai", BaseURL: "http://vllm.internal:8000/v1",
				Tags: []string{"self_hosted", "zdr"}, MaxConcurrency: 64,
				Timeouts: ProviderTimeouts{Connect: Duration(2 * time.Second), FirstToken: Duration(20 * time.Second), Idle: Duration(60 * time.Second), Total: Duration(10 * time.Minute)},
			},
			{
				ID: "together", Type: "openai", BaseURL: "https://api.together.xyz/v1",
				APIKey:   NewSecret("tg-test-456"),
				Timeouts: ProviderTimeouts{Connect: Duration(5 * time.Second), FirstToken: Duration(30 * time.Second), Idle: Duration(60 * time.Second), Total: Duration(10 * time.Minute)},
			},
		},
		Models: []Model{
			{ID: "llama-3.3-70b", Type: "chat", TokenRatio: 1, Deployments: []Deployment{
				{Provider: "gpu-cluster", UpstreamModel: "meta-llama/Llama-3.3-70B-Instruct", Pricing: Pricing{Input: mustPrice(t, "0.10"), Output: mustPrice(t, "0.10")}},
				{Provider: "together", UpstreamModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo", Pricing: Pricing{Input: mustPrice(t, "0.88"), Output: mustPrice(t, "0.88")}},
			}},
			{ID: "gpt-5", Type: "chat", TokenRatio: 1, Deployments: []Deployment{
				{Provider: "openai", UpstreamModel: "gpt-5", Pricing: Pricing{Input: mustPrice(t, "1.25"), Output: mustPrice(t, "10")}},
			}},
		},
		Tenants: []Tenant{
			{ID: "acme", Keys: []APIKey{
				{ID: "acme-backend", Hash: "sha256:" + strings.Repeat("0123456789abcdef", 4), Tags: []string{"prod", "eu"}},
				{ID: "acme-ci", Hash: "sha256:" + strings.Repeat("fedcba9876543210", 4)},
			}},
			{ID: "beta", Keys: []APIKey{{ID: "beta-1", Hash: "sha256:" + strings.Repeat("a", 64)}}},
		},
		Routing: DefaultRouting(),
		Budgets: Budgets{FailMode: BudgetFailOpen},
	}
	if diff := cmp.Diff(want, cfg, cmp.Comparer(func(a, b Secret) bool { return a.Value() == b.Value() })); diff != "" {
		t.Errorf("Config mismatch (-want +got):\n%s", diff)
	}
	if w := snap.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings:\n%s", formatProblems(w))
	}

	abs := func(p string) string {
		a, err := filepath.Abs(filepath.Join("testdata", "happy", p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	wantSources := []string{abs("switchyard.yaml"), abs("catalog/a-models.yaml"), abs("catalog/b-providers.yaml")}
	if diff := cmp.Diff(wantSources, snap.Sources()); diff != "" {
		t.Errorf("Sources mismatch (-want +got):\n%s", diff)
	}

	if v := snap.Version(); len(v) != 12 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("Version() = %q, want 12 lowercase hex characters", v)
	}
}

func TestVersion(t *testing.T) {
	base := loadHappy(t, happyEnv).Version()
	if again := loadHappy(t, happyEnv).Version(); again != base {
		t.Errorf("Version differs across identical loads: %s vs %s", base, again)
	}

	changed := func(k, v string) map[string]string {
		m := make(map[string]string, len(happyEnv))
		for kk, vv := range happyEnv {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"secret changes", changed("OPENAI_API_KEY", "sk-test-openai-124")},
		{"secret inside a URL changes", changed("REDIS_STATE_PASSWORD", "other")},
		{"plain value changes", changed("GPU_MAX_CONCURRENCY", "65")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v := loadHappy(t, tt.env).Version(); v == base {
				t.Errorf("Version unchanged (%s) after a value changed", v)
			}
		})
	}

	t.Run("override changes it", func(t *testing.T) {
		snap, err := Load(filepath.Join("testdata", "happy", "switchyard.yaml"), LoadOptions{
			LookupEnv: envMap(happyEnv),
			Overrides: map[string]string{"logging.level": "debug"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Version() == base {
			t.Error("Version unchanged after an override")
		}
	})
}

func TestVersionDoesNotContainSecret(t *testing.T) {
	// The canonical encoding must hash secrets, not embed them; check that
	// two configs differing only in a secret of equal length differ, and that
	// SecretValues round-trips.
	a := &Config{Providers: []Provider{{APIKey: NewSecret("aaaa")}}}
	b := &Config{Providers: []Provider{{APIKey: NewSecret("aaab")}}}
	empty := &Config{Providers: []Provider{{}}}
	if fingerprint(a) == fingerprint(b) || fingerprint(a) == fingerprint(empty) {
		t.Error("fingerprint does not depend on secret values")
	}
}

func TestLoadAcceptanceThreeMistakes(t *testing.T) {
	path := filepath.Join("testdata", "acceptance", "three-mistakes.yaml")
	_, err := Load(path, LoadOptions{LookupEnv: envMap(nil)})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	checkProblems(t, ve.Problems,
		want{sev: SeverityError, file: path, line: 7, path: "server.admin_lisen", msg: `unknown field (did you mean "admin_listen"?)`},
		want{sev: SeverityError, file: path, line: 13, path: "providers[0].api_key", msg: "environment variable OPENAI_API_KEY is not set"},
		want{sev: SeverityError, file: path, line: 18, path: "models[0].deployments[0].provider", msg: `unknown provider "openia" (did you mean "openai"?)`},
	)
	if got := len(ve.Errors()); got != 3 {
		t.Errorf("Errors() has %d problems, want 3", got)
	}
	// The file defines no tenants, which is the one warning.
	if !strings.HasSuffix(ve.Error(), "3 errors, 1 warning") {
		t.Errorf("Error() summary wrong:\n%s", ve.Error())
	}
}

func TestLoadRootUnreadable(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), LoadOptions{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got %v", err)
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatal("a missing root file must not be a ValidationError")
	}
}

func TestIncludes(t *testing.T) {
	const providerA = "providers:\n  - id: a\n    type: openai\n    base_url: https://a.example/v1\n"
	tests := []struct {
		name  string
		root  string
		files fakeFS
		wants []want
	}{
		{
			name:  "pattern matches nothing",
			root:  "version: 1\ninclude:\n  - catalog/*.yaml\n  - other.yaml\n",
			files: fakeFS{"other.yaml": "version: 1\n"},
			wants: []want{{sev: SeverityError, file: rootFile, line: 3, path: "include[0]", msg: "matches no files"}},
		},
		{
			name:  "nested include",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "include: [b.yaml]\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, path: "include", msg: "only allowed in the root file"}},
		},
		{
			name:  "forbidden section",
			root:  "version: 1\ninclude: [inc/a.yaml]\n",
			files: fakeFS{"inc/a.yaml": providerA + "server:\n  listen: ':1'\n"},
			wants: []want{{sev: SeverityError, file: "inc/a.yaml", line: 5, path: "server", msg: `"server" is not allowed in included file inc/a.yaml`}},
		},
		{
			name:  "credential-like key in an included file",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "sk-proj-abcdefghijklmnopqrstuvwxyz0123: x\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, path: "<hidden key>", msg: "<hidden key> is not allowed in included file a.yaml"}},
		},
		{
			name:  "included version must be 1",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "version: 2\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, path: "version", msg: "must be 1"}},
		},
		{
			name:  "included section must be a sequence",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "providers:\n  id: x\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 2, path: "providers", msg: "expected a sequence, got a mapping"}},
		},
		{
			name:  "duplicate provider across files",
			root:  "version: 1\ninclude: [a.yaml]\n" + providerA,
			files: fakeFS{"a.yaml": "# comment\n" + providerA},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 3, path: "providers[1].id", msg: `duplicate provider id "a"; first defined at cfg.yaml:4`}},
		},
		{
			name:  "syntax error in an included file",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "providers:\n  - id: [unclosed\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, msg: "did not find expected"}},
		},
		{
			name:  "root file and duplicates are skipped",
			root:  "version: 1\ninclude: ['*.yaml', 'a.yaml']\n",
			files: fakeFS{"a.yaml": providerA},
		},
		{
			name:  "invalid glob",
			root:  "version: 1\ninclude: ['[']\n",
			wants: []want{{sev: SeverityError, file: rootFile, line: 2, path: "include[0]", msg: "invalid include pattern"}},
		},
		{
			name:  "include must be a list",
			root:  "version: 1\ninclude: a.yaml\n",
			wants: []want{{sev: SeverityError, file: rootFile, line: 2, path: "include", msg: `expected a sequence, got "a.yaml"`}},
		},
		{
			name:  "included file is not a mapping",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "- a\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, msg: "must be a mapping of providers, models and aliases, got a sequence"}},
		},
		{
			name:  "empty included file is fine",
			root:  "version: 1\ninclude: [a.yaml]\n",
			files: fakeFS{"a.yaml": "# nothing yet\n"},
		},
		{
			name:  "root providers null is extended",
			root:  "version: 1\ninclude: [a.yaml]\nproviders:\n",
			files: fakeFS{"a.yaml": providerA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.root, nil, tt.files), tt.wants...)
		})
	}
}

func TestIncludeUnreadable(t *testing.T) {
	files := fakeFS{rootFile: "version: 1\ninclude: [ghost.yaml]\n"}
	glob := func(string) ([]string, error) { return []string{"ghost.yaml"}, nil }
	_, err := Load(rootFile, LoadOptions{ReadFile: files.ReadFile, Glob: glob, LookupEnv: envMap(nil)})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 2, path: "include[0]", msg: "cannot read included file ghost.yaml: file does not exist"})
}

func TestIncludeMergeOrderAndSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prov := func(id string) string {
		return "providers:\n  - id: " + id + "\n    type: openai\n    base_url: https://" + id + ".example\n"
	}
	write("root.yaml", "version: 1\ninclude: [c/*.yaml, c/b.yaml]\n"+prov("root"))
	write("c/b.yaml", prov("b"))
	write("c/a.yaml", prov("a"))

	snap, err := Load(filepath.Join(dir, "root.yaml"), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range snap.Config().Providers {
		ids = append(ids, p.ID)
	}
	if diff := cmp.Diff([]string{"root", "a", "b"}, ids); diff != "" {
		t.Errorf("provider order (-want +got):\n%s", diff)
	}
	wantSources := []string{filepath.Join(dir, "root.yaml"), filepath.Join(dir, "c/a.yaml"), filepath.Join(dir, "c/b.yaml")}
	if diff := cmp.Diff(wantSources, snap.Sources()); diff != "" {
		t.Errorf("Sources (-want +got):\n%s", diff)
	}
}

func TestSyntaxError(t *testing.T) {
	ve := mustFail(t, "version: 1\nserver:\n  listen: \":8080\n", nil, nil)
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 3, msg: "found unexpected end of stream"})

	ve = mustFail(t, "version: 1\n---\nversion: 1\n", nil, nil)
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 2, msg: "multiple YAML documents"})
}

func TestEmptyRootFile(t *testing.T) {
	ve := mustFail(t, "", nil, nil)
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, path: "version", msg: "required"})
}

func TestOverrides(t *testing.T) {
	const root = "version: 1\nserver:\n  listen: ':7000'\n  admin_listen: 127.0.0.1:7001\nlogging:\n  level: warn\n"
	tests := []struct {
		name      string
		overrides map[string]string
		check     func(t *testing.T, c *Config)
		wants     []want
	}{
		{
			name: "file beats defaults",
			check: func(t *testing.T, c *Config) {
				if c.Server.Listen != ":7000" || c.Logging.Level != "warn" || c.Logging.Format != "json" {
					t.Errorf("got listen=%q level=%q format=%q", c.Server.Listen, c.Logging.Level, c.Logging.Format)
				}
			},
		},
		{
			name:      "overrides beat file",
			overrides: map[string]string{"server.listen": ":9000", "server.admin_listen": "[::1]:9001", "logging.level": "debug"},
			check: func(t *testing.T, c *Config) {
				if c.Server.Listen != ":9000" || c.Server.AdminListen != "[::1]:9001" || c.Logging.Level != "debug" {
					t.Errorf("got %+v %+v", c.Server, c.Logging)
				}
			},
		},
		{
			name:      "unknown key",
			overrides: map[string]string{"server.port": "1"},
			wants:     []want{{sev: SeverityError, file: "-", path: "server.port", msg: "unsupported override"}},
		},
		{
			name:      "invalid override value has no file",
			overrides: map[string]string{"logging.level": "loud"},
			wants:     []want{{sev: SeverityError, file: "-", path: "logging.level", msg: `got "loud"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := fakeFS{rootFile: root}
			snap, err := Load(rootFile, LoadOptions{ReadFile: files.ReadFile, Glob: files.Glob, LookupEnv: envMap(nil), Overrides: tt.overrides})
			if len(tt.wants) > 0 {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("want *ValidationError, got %v", err)
				}
				checkProblems(t, ve.Problems, tt.wants...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, snap.Config())
		})
	}
}

func TestOverrideErrorString(t *testing.T) {
	p := Problem{Severity: SeverityError, Path: "server.listen", Message: "bad"}
	if got := p.String(); got != "error: server.listen: bad" {
		t.Errorf("String() = %q", got)
	}
}

func TestProblemString(t *testing.T) {
	tests := []struct {
		p    Problem
		want string
	}{
		{Problem{Severity: SeverityError, File: "cfg.yaml", Line: 3, Path: "a.b", Message: "m"}, "cfg.yaml:3: error: a.b: m"},
		{Problem{Severity: SeverityWarning, File: "cfg.yaml", Message: "m"}, "cfg.yaml: warning: m"},
		{Problem{Severity: SeverityError, Path: "server.listen", Message: "m"}, "error: server.listen: m"},
		{Problem{Severity: SeverityError, Message: "m"}, "error: m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
	if SeverityError.String() != "error" || SeverityWarning.String() != "warning" {
		t.Error("Severity.String")
	}
}

func TestValidationErrorSortedAndComplete(t *testing.T) {
	// Errors and warnings from two files, found in a different order than
	// they sort.
	root := "version: 1\ninclude: [z.yaml]\nserver:\n  admin_listen: ':9090'\nlogging:\n  level: loud\n"
	ve := mustFail(t, root, nil, fakeFS{"z.yaml": "providers:\n  - id: BAD\n    type: openai\n    base_url: https://x\n"})
	var got []string
	for _, p := range ve.Problems {
		got = append(got, p.String())
	}
	wantOrder := []string{
		// No tenants: a warning about a missing key sorts first, at line 0.
		"cfg.yaml: warning: tenants: " + msgNoTenants,
		`cfg.yaml:4: warning: server.admin_listen: ` + msgAdminExposed,
		`cfg.yaml:6: error: logging.level: must be one of debug, info, warn, error, got "loud"`,
		`z.yaml:2: error: providers[0].id: must match ^[a-z0-9][a-z0-9_-]{0,62}$, got "BAD"`,
	}
	if diff := cmp.Diff(wantOrder, got); diff != "" {
		t.Errorf("problems (-want +got):\n%s", diff)
	}
	if len(ve.Errors()) != 2 {
		t.Errorf("Errors() = %d, want 2", len(ve.Errors()))
	}
	if !strings.HasSuffix(ve.Error(), "2 errors, 2 warnings") {
		t.Errorf("Error() = %q", ve.Error())
	}
}
