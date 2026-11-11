package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		root string
		want want
	}{
		{
			name: "top level with suggestion",
			root: "version: 1\nprovider: []\n",
			want: want{sev: SeverityError, file: rootFile, line: 2, path: "provider", msg: `unknown field (did you mean "providers"?)`},
		},
		{
			name: "second level with suggestion",
			root: "version: 1\nserver:\n  listn: ':1'\n",
			want: want{sev: SeverityError, file: rootFile, line: 3, path: "server.listn", msg: `unknown field (did you mean "listen"?)`},
		},
		{
			name: "third level with suggestion",
			root: "version: 1\nstorage:\n  redis:\n    cache_ulr: x\n",
			want: want{sev: SeverityError, file: rootFile, line: 4, path: "storage.redis.cache_ulr", msg: `unknown field (did you mean "cache_url"?)`},
		},
		{
			name: "inside a sequence",
			root: "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    timeouts:\n      conect: 1s\n",
			want: want{sev: SeverityError, file: rootFile, line: 7, path: "providers[0].timeouts.conect", msg: `unknown field (did you mean "connect"?)`},
		},
		{
			name: "deep in models",
			root: "version: 1\nproviders:\n  - {id: p, type: openai, base_url: 'https://p.example'}\nmodels:\n  - id: m\n    deployments:\n      - provider: p\n        upstrem_model: x\n",
			want: want{sev: SeverityError, file: rootFile, line: 8, path: "models[0].deployments[0].upstrem_model", msg: `unknown field (did you mean "upstream_model"?)`},
		},
		{
			name: "no suggestion when nothing is close",
			root: "version: 1\npolicies: {}\n",
			want: want{sev: SeverityError, file: rootFile, line: 2, path: "policies", msg: "unknown field"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.root, nil, nil), tt.want)
		})
	}
}

func TestTypeErrors(t *testing.T) {
	tests := []struct {
		name  string
		root  string
		env   map[string]string
		wants []want
	}{
		{
			name:  "mapping where a sequence is expected",
			root:  "version: 1\nproviders:\n  id: p\n",
			wants: []want{{sev: SeverityError, line: 3, path: "providers", msg: "expected a sequence, got a mapping"}},
		},
		{
			name:  "sequence where a mapping is expected",
			root:  "version: 1\nserver: [a]\n",
			wants: []want{{sev: SeverityError, line: 2, path: "server", msg: "expected a mapping, got a sequence"}},
		},
		{
			name:  "string where an integer is expected",
			root:  "version: one\n",
			wants: []want{{sev: SeverityError, line: 1, path: "version", msg: `expected an integer, got "one"`}},
		},
		{
			name:  "float where an integer is expected",
			root:  "version: 1.5\n",
			wants: []want{{sev: SeverityError, line: 1, path: "version", msg: `expected an integer, got "1.5"`}},
		},
		{
			name:  "mapping where a string is expected",
			root:  "version: 1\nlogging:\n  level: {a: b}\n",
			wants: []want{{sev: SeverityError, line: 3, path: "logging.level", msg: "expected a string, got a mapping"}},
		},
		{
			name:  "secret given a mapping",
			root:  "version: 1\nstorage:\n  ledger:\n    dsn: {a: b}\n",
			wants: []want{{sev: SeverityError, line: 4, path: "storage.ledger.dsn", msg: "expected a string, got a mapping"}},
		},
		{
			name: "decoding continues after a type error",
			root: "version: 1\nserver:\n  max_request_bytes: lots\n  idle_timeout: 60\nlogging:\n  level: loud\n",
			wants: []want{
				{sev: SeverityError, line: 3, path: "server.max_request_bytes", msg: `expected a byte size such as "10MiB" or a whole number of bytes, got "lots"`},
				{sev: SeverityError, line: 4, path: "server.idle_timeout", msg: `expected a duration such as "60s", got "60"`},
				{sev: SeverityError, line: 6, path: "logging.level", msg: "must be one of"},
			},
		},
		{
			name:  "expanded value is named by its reference",
			root:  "version: 1\nserver:\n  idle_timeout: ${IDLE}\n",
			env:   map[string]string{"IDLE": "forever"},
			wants: []want{{sev: SeverityError, line: 3, path: "server.idle_timeout", msg: `expected a duration such as "60s", got the value of ${IDLE}`}},
		},
		{
			name:  "mixed expansion is named by its template",
			root:  "version: 1\nserver:\n  idle_timeout: ${IDLE}x\n",
			env:   map[string]string{"IDLE": "60"},
			wants: []want{{sev: SeverityError, line: 3, path: "server.idle_timeout", msg: `got the expansion of "${IDLE}x"`}},
		},
		{
			name:  "duplicate key",
			root:  "version: 1\nlogging:\n  level: info\n  level: debug\n",
			wants: []want{{sev: SeverityError, line: 4, path: "logging.level", msg: "duplicate key (first set at line 3)"}},
		},
		{
			name:  "integer out of range",
			root:  "version: 99999999999999999999\n",
			wants: []want{{sev: SeverityError, line: 1, path: "version", msg: "expected an integer"}},
		},
		{
			name:  "top level is not a mapping",
			root:  "- a\n- b\n",
			wants: []want{{sev: SeverityError, line: 1, path: "", msg: "expected a mapping, got a sequence"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := mustFail(t, tt.root, tt.env, nil)
			for i := range tt.wants {
				tt.wants[i].file = rootFile
			}
			checkProblems(t, ve.Problems, tt.wants...)
		})
	}
}

func TestAliases(t *testing.T) {
	root := `version: 1
x-timeouts: &t {connect: 1s}
providers:
  - id: a
    type: openai
    base_url: https://a.example
    timeouts: *t
`
	// x-timeouts is an unknown field; aliases still resolve elsewhere.
	ve := mustFail(t, root, nil, nil)
	checkProblems(t, ve.Problems, errAt("x-timeouts", "unknown field"))

	snap := mustLoad(t, "version: 1\nproviders:\n  - id: &pid a\n    type: openai\n    base_url: https://a.example\nmodels:\n  - id: m\n    deployments: [{provider: *pid, upstream_model: x}]\n", nil, nil)
	if got := snap.Config().Models[0].Deployments[0].Provider; got != "a" {
		t.Errorf("aliased provider = %q, want a", got)
	}
}

func TestSuggest(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		want       string
	}{
		{"listn", []string{"listen", "admin_listen"}, "listen"},
		{"openia", []string{"anthropic", "openai"}, "openai"},
		{"xyz", []string{"listen"}, ""},
		{"id", []string{"ip"}, "ip"},
		{"ab", []string{"xy"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := suggest(tt.name, tt.candidates); got != tt.want {
				t.Errorf("suggest(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestMergeKeys(t *testing.T) {
	root := `version: 1
server:
  <<: {listen: "127.0.0.1:8081", admin_listen: "127.0.0.1:9091"}
  listen: "127.0.0.1:8082"
providers:
  - &base
    id: a
    type: openai
    base_url: https://a.example
    max_concurrency: 4
    timeouts: {connect: 3s}
  - <<: *base
    id: b
    max_concurrency: 8
  - <<: [{max_concurrency: 1, tags: [zdr]}, *base]
    id: c
`
	snap := mustLoad(t, root, nil, nil)
	cfg := snap.Config()
	if cfg.Server.Listen != "127.0.0.1:8082" || cfg.Server.AdminListen != "127.0.0.1:9091" {
		t.Errorf("server = %q, %q; want the explicit listen and the merged admin_listen",
			cfg.Server.Listen, cfg.Server.AdminListen)
	}
	type prov struct {
		id, typ, url string
		maxc         int
		tags         int
		connect      string
	}
	var got []prov
	for _, p := range cfg.Providers {
		got = append(got, prov{p.ID, p.Type, p.BaseURL, p.MaxConcurrency, len(p.Tags), p.Timeouts.Connect.String()})
	}
	wantProvs := []prov{
		{"a", "openai", "https://a.example", 4, 0, "3s"},
		{"b", "openai", "https://a.example", 8, 0, "3s"},
		// The earlier mapping in a merge sequence wins.
		{"c", "openai", "https://a.example", 1, 1, "3s"},
	}
	if len(got) != len(wantProvs) {
		t.Fatalf("got %d providers, want %d", len(got), len(wantProvs))
	}
	for i := range wantProvs {
		if got[i] != wantProvs[i] {
			t.Errorf("providers[%d] = %+v, want %+v", i, got[i], wantProvs[i])
		}
	}

	tests := []struct {
		name  string
		root  string
		env   map[string]string
		wants []want
	}{
		{
			name:  "unknown merged key",
			root:  "version: 1\nserver:\n  <<: {listn: ':1'}\n",
			wants: []want{{sev: SeverityError, line: 3, path: "server.listn", msg: `unknown field (did you mean "listen"?)`}},
		},
		{
			name: "merged value with a type error",
			root: "version: 1\nproviders:\n  - &p {id: a, type: openai, base_url: 'https://a.example', max_concurrency: x}\n  - <<: *p\n    id: b\n",
			wants: []want{
				{sev: SeverityError, line: 3, path: "providers[0].max_concurrency", msg: "expected an integer"},
				{sev: SeverityError, line: 4, path: "providers[1].max_concurrency", msg: "expected an integer"},
			},
		},
		{
			name:  "merge value is not a mapping",
			root:  "version: 1\nserver:\n  <<: 5\n",
			wants: []want{{sev: SeverityError, line: 3, path: "server", msg: "a merge key (<<) needs a mapping or a sequence of mappings"}},
		},
		{
			name:  "merge sequence holds a scalar",
			root:  "version: 1\nserver:\n  <<: [{listen: ':1'}, x]\n",
			wants: []want{{sev: SeverityError, line: 3, path: "server", msg: "a merge key (<<) needs a mapping or a sequence of mappings"}},
		},
		{
			name:  "a mapping that merges itself",
			root:  "version: 1\nserver: &s\n  listen: ':1'\n  <<: *s\n",
			wants: []want{{sev: SeverityError, line: 4, path: "server", msg: "must not merge a mapping into itself"}},
		},
		{
			name:  "references in an inline merge are reported at the merged path",
			root:  "version: 1\nserver:\n  <<: {listen: '${NOPE}'}\n",
			wants: []want{{sev: SeverityError, line: 3, path: "server.listen", msg: "environment variable NOPE is not set"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := mustFail(t, tt.root, tt.env, nil)
			for i := range tt.wants {
				tt.wants[i].file = rootFile
			}
			checkProblems(t, ve.Problems, tt.wants...)
		})
	}
}

// TestMergeKeysDoNotExplode loads a config whose merge keys double the
// number of merged mappings at every level, which would take 2^40 steps if
// each merge were resolved anew.
func TestMergeKeysDoNotExplode(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nproviders:\n  - &m0 {id: p0, type: openai, base_url: 'https://p.example'}\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "  - <<: &m%d {<<: [*m%d, *m%d]}\n    id: p%d\n", i, i-1, i-1, i)
	}
	snap := mustLoad(t, b.String(), nil, nil)
	if n := len(snap.Config().Providers); n != 41 {
		t.Fatalf("got %d providers, want 41", n)
	}
	if p := snap.Config().Providers[40]; p.ID != "p40" || p.Type != "openai" {
		t.Errorf("providers[40] = %q, %q; want p40, openai", p.ID, p.Type)
	}
}

func TestAliasProblemLocations(t *testing.T) {
	t.Run("a problem on an aliased value is reported at the alias", func(t *testing.T) {
		root := `version: 1
providers:
  - id: a
    type: &t gemini
    base_url: https://a.example
  - id: b
    type: *t
    base_url: https://b.example
`
		ve := mustFail(t, root, nil, nil)
		checkProblems(t, ve.Problems,
			want{sev: SeverityError, file: rootFile, line: 4, path: "providers[0].type", msg: "must be one of"},
			want{sev: SeverityError, file: rootFile, line: 7, path: "providers[1].type", msg: "must be one of"},
		)
	})
	t.Run("a failed expansion is reported once", func(t *testing.T) {
		root := `version: 1
providers:
  - id: a
    type: openai
    base_url: https://a.example
    max_concurrency: &m ${NOPE}
  - id: b
    type: openai
    base_url: https://b.example
    max_concurrency: *m
`
		ve := mustFail(t, root, nil, nil)
		checkProblems(t, ve.Problems,
			want{sev: SeverityError, file: rootFile, line: 6, path: "providers[0].max_concurrency", msg: "environment variable NOPE is not set"})
	})
	t.Run("a type error on an alias is reported at the alias", func(t *testing.T) {
		root := "version: 1\nx: &v [a]\nserver:\n  listen: *v\n"
		ve := mustFail(t, root, nil, nil)
		checkProblems(t, ve.Problems,
			want{sev: SeverityError, file: rootFile, line: 2, path: "x", msg: "unknown field"},
			want{sev: SeverityError, file: rootFile, line: 4, path: "server.listen", msg: "expected a string, got a sequence"})
	})
}
