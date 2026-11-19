package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test runs in tools/configdoc, two levels below the repository root.
var (
	repoTypes = filepath.Join("..", "..", defaultTypes)
	repoDoc   = filepath.Join("..", "..", defaultOut)
)

// TestCommittedReferenceIsCurrent fails when docs/configuration.md no longer
// matches internal/config/types.go.
func TestCommittedReferenceIsCurrent(t *testing.T) {
	src, err := os.ReadFile(repoTypes)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Generate(defaultTypes, src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(repoDoc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with %s: run `make docs-config` and commit the result", defaultOut, defaultTypes)
	}
}

// TestEveryKeyDocumented checks that every row of the real schema has a
// description, so a new field without a doc comment is noticed.
func TestEveryKeyDocumented(t *testing.T) {
	doc, err := os.ReadFile(repoDoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "| `") && strings.HasSuffix(line, "|  |") {
			t.Errorf("key without a description: %s", line)
		}
	}
}

func TestExtractDefault(t *testing.T) {
	tests := []struct {
		doc, want string
	}{
		{`Listen is the public API address (host:port). Default ":8080".`, `":8080"`},
		{`AdminListen is the address. Default "127.0.0.1:9090". It must never be public.`, `"127.0.0.1:9090"`},
		{`SampleRatio is the fraction, in [0, 1]. Default 0.05.`, `0.05`},
		{`PropagateUpstream sends the traceparent. Default false: provider calls leave the trust boundary.`, `false`},
		{`Burst is extra. Default: RequestsPerMinute / 6 (ten seconds' worth), at least 1.`, `RequestsPerMinute / 6 (ten seconds' worth), at least 1`},
		{`AuthFailuresPerMinute caps failures. Default 30; -1 disables the limit.`, `30`},
		{`ConsecutiveFailures, default 5.`, `5`},
		{`Default applies to a 429 without Retry-After. Default 1s.`, `1s`},
		{`MinVersion is 1.2 (default) or 1.3.`, `1.2`},
		{`FailMode is open (serve, and alert; default) or closed.`, `open`},
		{`Profile is the default profile for "auto" requests.`, ``},
		{`DefaultProfile is the profile used when a policy names none. Default "balanced".`, `"balanced"`},
		{`MaxConcurrency caps in-flight requests; 0 means unlimited.`, ``},
	}
	for _, tt := range tests {
		t.Run(tt.doc, func(t *testing.T) {
			if got := extractDefault(tt.doc); got != tt.want {
				t.Errorf("extractDefault = %q, want %q", got, tt.want)
			}
		})
	}
}

// schemaSrc exercises every kind of field the generator handles.
const schemaSrc = `package config

// Config is the root.
type Config struct {
	// Version is the schema version.
	Version int ` + "`yaml:\"version\"`" + `
	Server  Server ` + "`yaml:\"server\"`" + `
	// Items are things.
	Items []Item ` + "`yaml:\"items\"`" + `
	// Named maps a name to a thing.
	Named map[string]Item ` + "`yaml:\"named\"`" + `
	hidden string
}

// Server configures the listener.
type Server struct {
	// Listen is the address. Default ":8080".
	Listen string ` + "`yaml:\"listen\"`" + `
	// Timeout bounds a request. Default 10s.
	Timeout Duration ` + "`yaml:\"timeout\"`" + `
	// CertFile is a certificate; KeyFile its key.
	CertFile string ` + "`yaml:\"cert_file\"`" + `
	KeyFile  string ` + "`yaml:\"key_file\"`" + `
	// Token is sent as "Bearer <token>" | when Listen is public.
	Token Secret ` + "`yaml:\"token\"`" + `
}

// Item is a thing.
type Item struct {
	// Tags label it.
	Tags []string ` + "`yaml:\"tags\"`" + `
	// Limit, when set, overrides.
	Limit *Limit ` + "`yaml:\"limit\"`" + `
	Price Price ` + "`yaml:\"price\"`" + `
}

// Limit is a cap.
type Limit struct {
	// Rate per minute.
	Rate float64 ` + "`yaml:\"rate\"`" + `
}

// Duration is a duration.
type Duration int64

// Secret is a secret.
type Secret struct{ v *string }

// Price is a decimal.
type Price struct{ text string }

// ByteSize is a size.
type ByteSize int64
`

func TestGenerateRows(t *testing.T) {
	out, err := Generate("types.go", []byte(schemaSrc))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, want := range []string{
		"| `version` | integer | - | `version` is the schema version. |",
		"| [`server`](#server) | object | - | `server` configures the listener. |",
		"| `server.listen` | string | `:8080` | `listen` is the address. Default \":8080\". |",
		"| `server.timeout` | duration | `10s` |",
		"| `server.key_file` | string | - | `cert_file` is a certificate; `key_file` its key. |",
		"| `server.token` | secret | - | `token` is sent as \"Bearer &lt;token&gt;\" \\| when `listen` is public. |",
		"| `items[].tags` | list of strings | - | `tags` label it. |",
		"| `items[].limit` | object (optional) | - | `limit`, when set, overrides. |",
		"| `items[].limit.rate` | number | - | `rate` per minute. |",
		"| `items[].price` | decimal | - | Item is a thing. |",
		"| [`named`](#named) | map of string to object | - |",
		"| `named.<name>.tags` | list of strings | - |",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(doc, "hidden") {
		t.Error("output documents a field without a yaml tag")
	}
}

func TestGenerateErrors(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"syntax error", "package config\ntype", "parse"},
		{"no root", "package config\n// Other.\ntype Other struct{}\n", "no type Config"},
		{"root not a struct", "package config\n// Config.\ntype Config int\n", "not a struct"},
		{
			"unreachable struct",
			"package config\n// Config.\ntype Config struct{}\n// Orphan.\ntype Orphan struct {\n\tA int `yaml:\"a\"`\n}\n",
			"not reachable from Config: Orphan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Generate("types.go", []byte(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Generate error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestRunWriteAndCheck(t *testing.T) {
	dir := t.TempDir()
	types := filepath.Join(dir, "types.go")
	out := filepath.Join(dir, "configuration.md")
	if err := os.WriteFile(types, []byte(schemaSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		args   []string
		before func(t *testing.T)
		want   int
	}{
		{name: "check before writing", args: []string{"-check"}, want: 1},
		{name: "write", want: 0},
		{name: "check current", args: []string{"-check"}, want: 0},
		{
			name: "check stale",
			args: []string{"-check"},
			before: func(t *testing.T) {
				if err := os.WriteFile(out, []byte("stale\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: 1,
		},
		{name: "missing schema", args: []string{"-types", filepath.Join(dir, "missing.go")}, want: 1},
		{name: "bad flag", args: []string{"-bogus"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.before != nil {
				tt.before(t)
			}
			var stdout, stderr bytes.Buffer
			args := append([]string{"-types", types, "-out", out}, tt.args...)
			if got := run(args, &stdout, &stderr); got != tt.want {
				t.Fatalf("run(%q) = %d, want %d; stderr: %s", args, got, tt.want, stderr.String())
			}
		})
	}
}
