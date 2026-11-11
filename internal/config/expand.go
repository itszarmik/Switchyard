package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxRefFileBytes caps the size of a file read by a ${file:PATH} reference.
const maxRefFileBytes = 64 << 10

// segment is one part of an expanded scalar: literal text from the file, or
// the value of one reference.
type segment struct {
	text string
	ref  string // the reference as written, such as "${PASS}"; "" for literal text
	// fromDefault is set when text is the default of a ${VAR:-default}
	// reference, which is written literally in the file.
	fromDefault bool
}

// literal reports whether the segment's text was written in the file, either
// as plain text or as the default of a reference that was used.
func (sg segment) literal() bool { return sg.ref == "" || sg.fromDefault }

// expansion records how a scalar containing references was expanded.
type expansion struct {
	raw  string    // the scalar as written in the file
	refs []string  // the references, as written, in order
	segs []segment // the expanded value, in parts
}

// describe names where an expanded value came from without revealing it,
// for example "the value of ${MAXC}".
func (e *expansion) describe() string {
	if len(e.segs) == 1 && len(e.refs) == 1 {
		return "the value of " + e.refs[0]
	}
	if mayQuote(e.raw) {
		return fmt.Sprintf("the expansion of %q", e.raw)
	}
	// The literal text around the references may be a credential.
	return "the expansion of a value that references " + strings.Join(e.refs, ", ")
}

// envNameRE matches a valid environment variable name.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// expandTree expands references in every scalar value at or below n. Mapping
// keys are never expanded, and alias nodes are skipped because the node they
// refer to is expanded where it is defined. The values of a merge key (<<)
// are expanded at the path of the mapping they are merged into.
func (st *loadState) expandTree(n *yaml.Node, path string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			st.expandTree(c, path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if !isMergeKey(k) {
				st.expandTree(v, joinPath(path, shownKey(k.Value)))
				continue
			}
			if v.Kind == yaml.SequenceNode {
				for _, c := range v.Content {
					st.expandTree(c, path)
				}
			} else {
				st.expandTree(v, path)
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			st.expandTree(c, indexPath(path, i))
		}
	case yaml.ScalarNode:
		if !st.visited[n] {
			st.visited[n] = true
			st.expandScalar(n, path)
		}
	}
}

// expandScalar expands the references in one scalar node in place. On any
// error the node is left unchanged, and both path and the node are marked as
// failed, so the decoder does not report a second problem for it here or
// where it is used through an alias.
func (st *loadState) expandScalar(n *yaml.Node, path string) {
	s := n.Value
	if !strings.Contains(s, "$") {
		return
	}
	var (
		segs    []segment
		refs    []string
		lit     strings.Builder
		changed bool
		ok      = true
	)
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, segment{text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 == len(s) || (s[i+1] != '$' && s[i+1] != '{') {
			lit.WriteByte(s[i])
			i++
			continue
		}
		if s[i+1] == '$' {
			lit.WriteByte('$')
			changed = true
			i += 2
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			st.nodeError(n, path, `malformed reference: missing closing "}"`)
			ok = false
			break
		}
		body, ref := s[i+2:i+2+end], s[i:i+3+end]
		if strings.Contains(body, "${") {
			st.nodeError(n, path, "malformed reference: nested references are not supported")
			ok = false
			i = skipReference(s, i)
			continue
		}
		i += 3 + end
		placeholder := ""
		if st.opts.SecretPlaceholders {
			placeholder = secretPlaceholder(path, ref == s)
		}
		val, fromDefault, err := st.resolveRef(n, body, placeholder)
		if err != nil {
			st.nodeError(n, path, err.Error())
			ok = false
			continue
		}
		flush()
		segs = append(segs, segment{text: val, ref: ref, fromDefault: fromDefault})
		refs = append(refs, ref)
		changed = true
	}
	if !ok {
		st.failedNodes[n] = true
		return
	}
	if !changed {
		return
	}
	flush()
	var b strings.Builder
	for _, sg := range segs {
		b.WriteString(sg.text)
	}
	n.Value = b.String()
	if n.Style&(yaml.TaggedStyle|yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
		// A plain scalar: let YAML resolve the expanded text's type, so that
		// max_concurrency: ${MAXC} decodes as an integer.
		n.Tag = ""
	}
	if len(refs) > 0 {
		st.expanded[n] = &expansion{raw: s, refs: refs, segs: segs}
	}
}

// skipReference returns the index just past the reference that starts at
// s[i] ("${"), counting nested "${" and "}" pairs, or len(s) if it is not
// closed.
func skipReference(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch {
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case s[j] == '}':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// resolveRef returns the value of one reference, given the text between
// "${" and "}", and whether that value is the reference's default. Errors
// never contain the value of a variable or the contents of a file. When
// placeholder is set, nothing is read: a well-formed reference resolves to
// its default, if it has one, and otherwise to placeholder.
func (st *loadState) resolveRef(n *yaml.Node, body, placeholder string) (val string, fromDefault bool, err error) {
	if p, isFile := strings.CutPrefix(body, "file:"); isFile {
		if p == "" {
			return "", false, errors.New("malformed reference: empty file path in ${file:PATH}")
		}
		if placeholder != "" {
			return placeholder, false, nil
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(st.fileOf[n]), p)
		}
		data, err := st.readRefFile(p)
		if err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return "", false, fmt.Errorf("cannot read file %s: %w", p, err)
		}
		v := string(data)
		if t, cut := strings.CutSuffix(v, "\r\n"); cut {
			v = t
		} else {
			v = strings.TrimSuffix(v, "\n")
		}
		return v, false, nil
	}

	name, def, hasDef := body, "", false
	if i := strings.IndexByte(body, ':'); i >= 0 {
		rest := body[i+1:]
		if !strings.HasPrefix(rest, "-") {
			return "", false, errors.New("malformed reference: use ${VAR}, ${VAR:-default} or ${file:PATH}")
		}
		name, def, hasDef = body[:i], rest[1:], true
	}
	if name == "" {
		return "", false, errors.New("malformed reference: empty variable name")
	}
	if !envNameRE.MatchString(name) {
		return "", false, errors.New("malformed reference: variable names must match [A-Za-z_][A-Za-z0-9_]*")
	}
	if placeholder != "" {
		if hasDef {
			return def, true, nil
		}
		return placeholder, false, nil
	}
	v, set := st.opts.LookupEnv(name)
	switch {
	case hasDef && v == "":
		return def, true, nil
	case !set:
		return "", false, fmt.Errorf("environment variable %s is not set", name)
	}
	return v, false, nil
}

// Placeholders of LoadOptions.SecretPlaceholders.
const (
	// placeholderSecret replaces a reference that is part of a secret, or
	// a whole secret that is not a URL.
	placeholderSecret = "switchyard-placeholder"
	// placeholderRedisCache, placeholderRedisState and placeholderDSN
	// replace a whole Redis URL or ledger DSN; the two Redis URLs differ,
	// as the two instances should.
	placeholderRedisCache = "redis://placeholder.invalid:6379/0"
	placeholderRedisState = "redis://placeholder.invalid:6380/0"
	placeholderDSN        = "postgres://placeholder.invalid/switchyard"
)

// secretPaths holds the config path of every Secret field of Config, each
// sequence index written as "[]" (see indexFree), such as
// "providers[].api_key". It is computed once from the types and never
// changes.
var secretPaths = secretFieldPaths(reflect.TypeFor[Config](), "", map[string]bool{})

// secretFieldPaths adds to out the path of every Secret field of t, which
// is at path, and returns out. Maps are not walked: no map holds a secret.
func secretFieldPaths(t reflect.Type, path string, out map[string]bool) map[string]bool {
	switch {
	case t == secretType:
		out[path] = true
	case t.Kind() == reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				secretFieldPaths(f.Type, joinPath(path, yamlName(f)), out)
			}
		}
	case t.Kind() == reflect.Slice, t.Kind() == reflect.Array:
		secretFieldPaths(t.Elem(), path+"[]", out)
	case t.Kind() == reflect.Pointer:
		secretFieldPaths(t.Elem(), path, out)
	}
	return out
}

// indexFree returns path with every sequence index, such as "[3]", written
// as "[]".
func indexFree(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		b.WriteByte(path[i])
		if path[i] != '[' {
			continue
		}
		j := i + 1
		for j < len(path) && path[j] >= '0' && path[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(path) && path[j] == ']' {
			i = j - 1
		}
	}
	return b.String()
}

// secretPlaceholder returns what a reference at path expands to under
// LoadOptions.SecretPlaceholders, or "" when path is not a secret and the
// reference resolves as usual. whole reports that the reference is the
// whole value.
func secretPlaceholder(path string, whole bool) string {
	if !secretPaths[indexFree(path)] {
		return ""
	}
	if whole {
		switch path {
		case "storage.redis.cache_url":
			return placeholderRedisCache
		case "storage.redis.state_url":
			return placeholderRedisState
		case "storage.ledger.dsn":
			return placeholderDSN
		}
	}
	return placeholderSecret
}

// errRefFileTooLarge is returned for a ${file:PATH} file over the size cap.
var errRefFileTooLarge = fmt.Errorf("larger than the %d KiB limit for ${file:...} references", maxRefFileBytes>>10)

// limitRefFile wraps a LoadOptions.ReadFile so that it rejects files over
// the ${file:PATH} size cap.
func limitRefFile(readFile func(string) ([]byte, error)) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		data, err := readFile(name)
		if err != nil {
			return nil, err
		}
		if len(data) > maxRefFileBytes {
			return nil, errRefFileTooLarge
		}
		return data, nil
	}
}

// readRefFile reads a ${file:PATH} file from disk. It reads only regular
// files, so a device such as /dev/zero or a FIFO cannot hang or exhaust
// memory, and at most maxRefFileBytes of them.
func readRefFile(name string) ([]byte, error) {
	// Stat before opening: opening a FIFO blocks until a writer appears.
	if fi, err := os.Stat(name); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Check again in case the path was replaced after the first check.
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRefFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRefFileBytes {
		return nil, errRefFileTooLarge
	}
	return data, nil
}
