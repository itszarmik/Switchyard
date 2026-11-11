package config

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeFS is an in-memory file system for LoadOptions.ReadFile and Glob.
// Keys are cleaned paths.
type fakeFS map[string]string

func (f fakeFS) ReadFile(name string) ([]byte, error) {
	if s, ok := f[filepath.Clean(name)]; ok {
		return []byte(s), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f fakeFS) Glob(pattern string) ([]string, error) {
	var out []string
	for name := range f {
		ok, err := filepath.Match(pattern, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// envMap returns a LookupEnv over m.
func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// rootFile is the root path used by in-memory tests.
const rootFile = "cfg.yaml"

// loadYAML loads root text (plus any extra files) from memory.
func loadYAML(t *testing.T, root string, env map[string]string, extra fakeFS) (*Snapshot, error) {
	t.Helper()
	files := fakeFS{rootFile: root}
	for k, v := range extra {
		files[filepath.Clean(k)] = v
	}
	return Load(rootFile, LoadOptions{LookupEnv: envMap(env), ReadFile: files.ReadFile, Glob: files.Glob})
}

// mustLoad loads and fails the test on any error.
func mustLoad(t *testing.T, root string, env map[string]string, extra fakeFS) *Snapshot {
	t.Helper()
	snap, err := loadYAML(t, root, env, extra)
	if err != nil {
		t.Fatalf("Load: unexpected error:\n%v", err)
	}
	return snap
}

// mustFail loads and returns the ValidationError, failing the test if Load
// succeeds or returns another kind of error.
func mustFail(t *testing.T, root string, env map[string]string, extra fakeFS) *ValidationError {
	t.Helper()
	_, err := loadYAML(t, root, env, extra)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Load: want *ValidationError, got %T: %v", err, err)
	}
	return ve
}

// want describes an expected problem. Line 0 means "don't check";
// File "-" means "must be empty".
type want struct {
	sev  Severity
	path string
	msg  string // substring
	file string
	line int
}

func errAt(path, msg string) want  { return want{sev: SeverityError, path: path, msg: msg} }
func warnAt(path, msg string) want { return want{sev: SeverityWarning, path: path, msg: msg} }

// matches reports whether p satisfies w.
func (w want) matches(p Problem) bool {
	if p.Severity != w.sev || p.Path != w.path || !strings.Contains(p.Message, w.msg) {
		return false
	}
	switch w.file {
	case "":
	case "-":
		if p.File != "" {
			return false
		}
	default:
		if p.File != w.file {
			return false
		}
	}
	return w.line == 0 || p.Line == w.line
}

// problemsOf returns the problems of a load: the ValidationError's problems,
// or the snapshot's warnings.
func problemsOf(t *testing.T, root string, env map[string]string, extra fakeFS) []Problem {
	t.Helper()
	snap, err := loadYAML(t, root, env, extra)
	if err != nil {
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("Load: want *ValidationError, got %T: %v", err, err)
		}
		return ve.Problems
	}
	return snap.Warnings()
}

// isNoTenantsWarning reports whether p is the warning every config without
// tenants gets.
func isNoTenantsWarning(p Problem) bool {
	return p.Severity == SeverityWarning && p.Path == "tenants" && p.Message == msgNoTenants
}

// isPricingWarning reports whether p is the warning every deployment without
// prices gets.
func isPricingWarning(p Problem) bool {
	return p.Severity == SeverityWarning && p.Message == MsgPricingUnknown
}

// checkProblems asserts that problems contains exactly the wanted ones, in
// any order. The no-tenants warning comes with every config that defines no
// tenants, and the pricing warning with every deployment that has no prices.
// Most tests are about something else, so the no-tenants warning is ignored
// unless one of wants names the tenants path, and pricing warnings unless
// one of wants is a pricing warning.
func checkProblems(t *testing.T, got []Problem, wants ...want) {
	t.Helper()
	if !slices.ContainsFunc(wants, func(w want) bool { return w.path == "tenants" }) {
		got = slices.DeleteFunc(slices.Clone(got), isNoTenantsWarning)
	}
	if !slices.ContainsFunc(wants, func(w want) bool { return w.msg == MsgPricingUnknown }) {
		got = slices.DeleteFunc(slices.Clone(got), isPricingWarning)
	}
	used := make([]bool, len(got))
	for _, w := range wants {
		found := false
		for i, p := range got {
			if !used[i] && w.matches(p) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			t.Errorf("missing problem %+v", w)
		}
	}
	for i, p := range got {
		if !used[i] {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	if t.Failed() {
		t.Logf("all problems:\n%s", formatProblems(got))
	}
}

func formatProblems(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("  " + p.String() + "\n")
	}
	return b.String()
}

// mustPrice parses a Price or fails the test.
func mustPrice(t *testing.T, s string) Price {
	t.Helper()
	p, err := ParsePrice(s)
	if err != nil {
		t.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}
