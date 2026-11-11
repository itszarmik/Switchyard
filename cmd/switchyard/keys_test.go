package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/auth"
	"switchyard/internal/config"
)

var (
	generatedKeyRE = regexp.MustCompile(`sy_[A-Za-z0-9_-]{43}`)
	snippetHashRE  = regexp.MustCompile(`(?m)^        hash: (sha256:[0-9a-f]{64})$`)
)

// generated is the parsed output of keys generate.
type generated struct {
	stdout  string
	key     string
	hash    string
	snippet string // from "tenants:" to the end
}

// generateKey runs keys generate through run, requires success and parses
// its output.
func generateKey(t *testing.T, args ...string) generated {
	t.Helper()
	code, stdout, stderr := runCLI(t, env{}, append([]string{"keys", "generate"}, args...)...)
	if code != exitOK || stderr != "" {
		t.Fatalf("keys generate %v: exit %d, stderr %q", args, code, stderr)
	}
	keys := generatedKeyRE.FindAllString(stdout, -1)
	if len(keys) != 1 {
		t.Fatalf("stdout has %d keys, want exactly 1:\n%s", len(keys), stdout)
	}
	m := snippetHashRE.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout has no hash line:\n%s", stdout)
	}
	i := strings.Index(stdout, "\ntenants:\n")
	if i < 0 {
		t.Fatalf("stdout has no tenants snippet:\n%s", stdout)
	}
	return generated{stdout: stdout, key: keys[0], hash: m[1], snippet: stdout[i+1:]}
}

func TestKeysGenerate(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantTenant string
		wantKeyID  string // "" means the default, derived from the hash
	}{
		{name: "defaults", wantTenant: "default"},
		{name: "tenant", args: []string{"--tenant", "acme"}, wantTenant: "acme"},
		{name: "tenant and key id", args: []string{"--tenant", "acme", "--key-id", "acme-backend"}, wantTenant: "acme", wantKeyID: "acme-backend"},
		{name: "single-dash flags", args: []string{"-tenant=acme", "-key-id=ci_1"}, wantTenant: "acme", wantKeyID: "ci_1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := generateKey(t, tt.args...)
			if g.hash != auth.HashKey(g.key) {
				t.Errorf("snippet hash %s is not the hash of the printed key", g.hash)
			}
			wantKeyID := tt.wantKeyID
			if wantKeyID == "" {
				wantKeyID = "key-" + strings.TrimPrefix(g.hash, "sha256:")[:6]
			}
			wantSnippet := "tenants:\n  - id: " + tt.wantTenant + "\n    keys:\n      - id: " + wantKeyID + "\n        hash: " + g.hash + "\n"
			if g.snippet != wantSnippet {
				t.Errorf("snippet =\n%s\nwant\n%s", g.snippet, wantSnippet)
			}
			if !strings.Contains(g.stdout, "Store it now; it cannot be shown again.") {
				t.Errorf("stdout has no store-it-now notice:\n%s", g.stdout)
			}
			if !strings.Contains(g.stdout, "\n    "+g.key+"\n") {
				t.Errorf("the key is not on a line of its own:\n%s", g.stdout)
			}

			// The snippet pastes into a config that loads without problems,
			// and the store built from it authenticates the printed key.
			path := filepath.Join(t.TempDir(), "switchyard.yaml")
			if err := os.WriteFile(path, []byte("version: 1\n"+g.snippet), 0o600); err != nil {
				t.Fatal(err)
			}
			snap, err := config.Load(path, config.LoadOptions{LookupEnv: env{}.lookup})
			if err != nil {
				t.Fatalf("config.Load of the snippet: %v", err)
			}
			if w := snap.Warnings(); len(w) != 0 {
				t.Errorf("the snippet has warnings: %v", w)
			}
			p, ok := auth.NewStore(snap.Config().Tenants).Authenticate(g.key)
			want := auth.Principal{TenantID: tt.wantTenant, KeyID: wantKeyID}
			if !ok || !cmp.Equal(p, want) {
				t.Errorf("Authenticate = %+v, %v; want %+v, true", p, ok, want)
			}
		})
	}
}

func TestKeysGenerateIsRandom(t *testing.T) {
	a, b := generateKey(t), generateKey(t)
	if a.key == b.key || a.hash == b.hash {
		t.Errorf("two runs printed the same key or hash")
	}
}

func TestKeysErrors(t *testing.T) {
	const pasted = "sy_Zq8pL2xW9vN4mK7jR3tYabcdefghijklmnopqrstu"
	tests := []struct {
		name      string
		args      []string
		wantCode  int
		wantErrRE string
	}{
		{"no subcommand", []string{"keys"}, exitUsage, `(?s)^error: keys: missing subcommand\nUsage: switchyard keys generate`},
		{"unknown subcommand", []string{"keys", "list"}, exitUsage, `(?s)^error: keys: unknown subcommand "list"\nUsage: switchyard keys generate`},
		{"help", []string{"keys", "generate", "--help"}, exitOK, `(?s)^Usage: switchyard keys generate`},
		{"unknown flag", []string{"keys", "generate", "--bogus"}, exitUsage, `flag provided but not defined: -bogus`},
		{"missing flag value", []string{"keys", "generate", "--tenant"}, exitUsage, `flag needs an argument: -tenant`},
		{"extra argument", []string{"keys", "generate", pasted}, exitUsage, `(?s)^error: keys generate: takes no arguments, only flags\nUsage:`},
		{"invalid tenant", []string{"keys", "generate", "--tenant", "Acme"}, exitUsage, `^error: keys generate: --tenant is invalid: use 1 to 63 characters`},
		{"empty tenant", []string{"keys", "generate", "--tenant", ""}, exitUsage, `^error: keys generate: --tenant is invalid`},
		{"invalid key id", []string{"keys", "generate", "--key-id", pasted}, exitUsage, `^error: keys generate: --key-id is invalid`},
		{"key id too long", []string{"keys", "generate", "--key-id", strings.Repeat("k", 64)}, exitUsage, `^error: keys generate: --key-id is invalid`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, env{}, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if !regexp.MustCompile(tt.wantErrRE).MatchString(stderr) {
				t.Errorf("stderr = %q, want a match for %q", stderr, tt.wantErrRE)
			}
			if strings.Contains(stderr, pasted) || generatedKeyRE.MatchString(stderr) {
				t.Errorf("stderr contains a key: %q", stderr)
			}
		})
	}
}

func TestUsageMentionsKeys(t *testing.T) {
	if !strings.Contains(usage, "switchyard keys generate [flags]") {
		t.Errorf("usage does not list keys generate:\n%s", usage)
	}
}
