package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestReferenceExpansion(t *testing.T) {
	absSecret, err := filepath.Abs(filepath.Join("testdata", "abs-secret"))
	if err != nil {
		t.Fatal(err)
	}
	files := fakeFS{
		"key.txt":            "from-file\n",
		"key-crlf.txt":       "from-crlf\r\n",
		"key-raw.txt":        "no-newline",
		"key-two.txt":        "two\n\n",
		"secrets/nested.txt": "nested\n",
		absSecret:            "absolute\n",
		"inc/relative.txt":   "relative-to-include\n",
		"key-max.txt":        strings.Repeat("k", 64<<10),
		"inc/provider.yaml":  "providers:\n  - id: inc\n    type: openai\n    base_url: https://inc.example\n    api_key: ${file:relative.txt}\n",
	}
	env := map[string]string{
		"SET":     "value",
		"EMPTY":   "",
		"PASS":    "p@ss:word",
		"YAMLISH": "a: [b, c]\nd: e",
	}
	// Each case sets providers[0].api_key (a string) to the given scalar.
	tests := []struct {
		name   string
		scalar string
		want   string
	}{
		{"no reference", "plain", "plain"},
		{"required set", "${SET}", "value"},
		{"required set but empty", "${EMPTY}", ""},
		{"default when unset", "${UNSET:-fallback}", "fallback"},
		{"default when empty", "${EMPTY:-fallback}", "fallback"},
		{"default ignored when set", "${SET:-fallback}", "value"},
		{"empty default", "x${UNSET:-}y", "xy"},
		{"default containing colon and dash", "${UNSET:-redis://h:1/0}", "redis://h:1/0"},
		{"escape", "$${SET}", "${SET}"},
		{"double escape then ref", "$$$${SET}", "$${SET}"},
		{"lone dollar is literal", "pa$word $", "pa$word $"},
		{"mixed literal and refs", "redis://:${PASS}@host:6379/${SET}", "redis://:p@ss:word@host:6379/value"},
		{"file relative", "${file:key.txt}", "from-file"},
		{"file nested relative", "${file:secrets/nested.txt}", "nested"},
		{"file absolute", "${file:" + absSecret + "}", "absolute"},
		{"file crlf trimmed", "${file:key-crlf.txt}", "from-crlf"},
		{"file without newline", "${file:key-raw.txt}", "no-newline"},
		{"file only one newline trimmed", "${file:key-two.txt}", "two\n"},
		{"file at the size limit", "${file:key-max.txt}", strings.Repeat("k", 64<<10)},
		{"env value cannot inject YAML", "${YAMLISH}", "a: [b, c]\nd: e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    api_key: '" +
				strings.ReplaceAll(tt.scalar, "'", "''") + "'\n"
			snap := mustLoad(t, root, env, files)
			if got := snap.Config().Providers[0].APIKey.Value(); got != tt.want {
				t.Errorf("api_key = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("file relative to the including file", func(t *testing.T) {
		snap := mustLoad(t, "version: 1\ninclude: [inc/provider.yaml]\n", env, files)
		if got := snap.Config().Providers[0].APIKey.Value(); got != "relative-to-include" {
			t.Errorf("api_key = %q", got)
		}
	})
}

func TestReferenceErrors(t *testing.T) {
	files := fakeFS{"secret.txt": "TOP-SECRET-CONTENTS", "big.txt": strings.Repeat("x", 64<<10+1)}
	tests := []struct {
		name   string
		scalar string
		msg    string
	}{
		{"unset required", "${NOPE}", "environment variable NOPE is not set"},
		{"unset in mixed text", "redis://:${NOPE}@h", "environment variable NOPE is not set"},
		{"unterminated", "${SET", `missing closing "}"`},
		{"empty name", "${}", "empty variable name"},
		{"empty name with default", "${:-x}", "empty variable name"},
		{"invalid name", "${1ABC}", "variable names must match"},
		{"invalid name chars", "${A-B}", "variable names must match"},
		{"bad default syntax", "${A:x}", "use ${VAR}, ${VAR:-default} or ${file:PATH}"},
		{"empty file path", "${file:}", "empty file path"},
		{"missing file", "${file:missing.txt}", "cannot read file missing.txt"},
		{"nested reference in a default", "${A:-${B}}", "malformed reference: nested references are not supported"},
		{"nested reference with text after it", "${A:-x${B}y}z", "malformed reference: nested references are not supported"},
		{"nested reference in a file path", "${file:${B}}", "malformed reference: nested references are not supported"},
		{"unterminated nested reference", "${A:-${B}", "malformed reference: nested references are not supported"},
		{"file too large", "${file:big.txt}", "cannot read file big.txt: larger than the 64 KiB limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := "version: 1\nstorage:\n  ledger:\n    dsn: \"" + tt.scalar + "\"\n"
			ve := mustFail(t, root, map[string]string{"SET": "x"}, files)
			checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 4, path: "storage.ledger.dsn", msg: tt.msg})
		})
	}

	t.Run("several problems in one scalar are all reported", func(t *testing.T) {
		ve := mustFail(t, "version: 1\nlogging:\n  level: ${A}${B}\n", nil, nil)
		checkProblems(t, ve.Problems,
			errAt("logging.level", "environment variable A is not set"),
			errAt("logging.level", "environment variable B is not set"),
		)
	})

	t.Run("file contents never appear in errors", func(t *testing.T) {
		ve := mustFail(t, "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    max_concurrency: ${file:secret.txt}\n", nil, files)
		if strings.Contains(ve.Error(), "TOP-SECRET-CONTENTS") {
			t.Errorf("error leaks file contents:\n%s", ve.Error())
		}
		checkProblems(t, ve.Problems, errAt("providers[0].max_concurrency", "expected an integer, got the value of ${file:secret.txt}"))
	})
}

func TestTypedExpansion(t *testing.T) {
	env := map[string]string{"MAXC": "64", "FLAG": "true", "NAME": "64"}

	t.Run("plain scalar expands to an int", func(t *testing.T) {
		root := "version: ${VER}\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    max_concurrency: ${MAXC}\n"
		snap := mustLoad(t, root, map[string]string{"VER": "1", "MAXC": "64"}, nil)
		if got := snap.Config().Providers[0].MaxConcurrency; got != 64 {
			t.Errorf("max_concurrency = %d, want 64", got)
		}
	})

	t.Run("quoted scalar stays a string", func(t *testing.T) {
		root := "version: 1\nproviders:\n  - id: p\n    type: openai\n    base_url: https://p.example\n    max_concurrency: \"${MAXC}\"\n"
		ve := mustFail(t, root, env, nil)
		checkProblems(t, ve.Problems, errAt("providers[0].max_concurrency", "expected an integer, got the value of ${MAXC}"))
	})

	t.Run("int-looking value into a string field", func(t *testing.T) {
		root := "version: 1\nmodels:\n  - id: m\n    deployments:\n      - provider: ${NAME}\n        upstream_model: x\n"
		ve := mustFail(t, root, env, nil)
		checkProblems(t, ve.Problems, errAt("models[0].deployments[0].provider", "unknown provider the value of ${NAME}"))
	})

	// Config has no bool fields yet, so decode into a test struct.
	type flags struct {
		Enabled bool `yaml:"enabled"`
		Quoted  bool `yaml:"quoted"`
		Count   int  `yaml:"count"`
	}
	t.Run("plain scalar expands to a bool", func(t *testing.T) {
		st := newLoadState(rootFile, LoadOptions{LookupEnv: envMap(env)})
		root := st.parse(rootFile, []byte("enabled: ${FLAG}\nquoted: '${FLAG}'\ncount: ${MAXC}\n"))
		st.expandTree(root, "")
		var got flags
		st.decodeValue(root, reflect.ValueOf(&got).Elem(), "")
		if !got.Enabled || got.Count != 64 {
			t.Errorf("got %+v, want enabled and count 64", got)
		}
		checkProblems(t, st.problems, errAt("quoted", "expected a boolean, got the value of ${FLAG}"))
	})
}

func TestExpansionIsNodeLevel(t *testing.T) {
	// A value containing YAML syntax must stay a single scalar.
	st := newLoadState(rootFile, LoadOptions{LookupEnv: envMap(map[string]string{"V": "x\nserver: {listen: ':1'}"})})
	root := st.parse(rootFile, []byte("logging:\n  level: ${V}\n"))
	st.expandTree(root, "")
	var got Config
	st.decodeValue(root, reflect.ValueOf(&got).Elem(), "")
	if len(st.problems) != 0 {
		t.Fatalf("problems: %v", st.problems)
	}
	if got.Server.Listen != "" || got.Logging.Level != "x\nserver: {listen: ':1'}" {
		t.Errorf("expansion changed structure: %+v", got)
	}
	if lvl := mappingValue(mappingValue(root, "logging"), "level"); lvl.Kind != yaml.ScalarNode {
		t.Errorf("node kind changed to %v", lvl.Kind)
	}
}

func TestMappingKeysAreNotExpanded(t *testing.T) {
	ve := mustFail(t, "version: 1\nserver:\n  ${KEY}: x\n", map[string]string{"KEY": "listen"}, nil)
	checkProblems(t, ve.Problems, errAt("server.${KEY}", "unknown field"))
}

// TestFileReferencesOnDisk covers the checks made when ${file:...} reads the
// real file system: only regular files of at most 64 KiB are read.
func TestFileReferencesOnDisk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.txt", "from-disk\n")
	write("max.txt", strings.Repeat("m", 64<<10))
	write("big.txt", strings.Repeat("b", 64<<10+1))
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, ref, want, msg string
	}{
		{name: "regular file", ref: "ok.txt", want: "from-disk"},
		{name: "at the limit", ref: "max.txt", want: strings.Repeat("m", 64<<10)},
		{name: "too large", ref: "big.txt", msg: "larger than the 64 KiB limit"},
		{name: "directory", ref: "subdir", msg: "not a regular file"},
		{name: "device", ref: os.DevNull, msg: "not a regular file"},
		{name: "missing", ref: "missing.txt", msg: "no such file or directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(dir, "switchyard.yaml")
			write("switchyard.yaml", "version: 1\nstorage:\n  ledger:\n    dsn: ${file:"+tt.ref+"}\n")
			snap, err := Load(root, LoadOptions{LookupEnv: envMap(nil)})
			if tt.msg == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if got := snap.Config().Storage.Ledger.DSN.Value(); got != tt.want {
					t.Errorf("dsn has %d bytes, want %d", len(got), len(tt.want))
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Load = %v, want a *ValidationError", err)
			}
			checkProblems(t, ve.Problems, errAt("storage.ledger.dsn", tt.msg))
		})
	}
}
