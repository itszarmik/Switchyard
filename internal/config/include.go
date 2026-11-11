package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// includeMatch is one file selected by the root file's include patterns.
type includeMatch struct {
	file string     // the glob match, used as Problem.File
	abs  string     // its absolute path, for de-duplication and Sources
	item *yaml.Node // the first include pattern that matched it
	path string     // that pattern's config path, include[i]
}

// includeFiles resolves the include patterns in inc, parses each matched file
// and appends its providers and models to the root mapping.
func (st *loadState) includeFiles(root, inc *yaml.Node) {
	if inc.Kind != yaml.SequenceNode {
		return // the decoder reports the type error
	}
	var matches []includeMatch
	dir := filepath.Dir(st.root)
	for i, item := range inc.Content {
		path := indexPath("include", i)
		if item.Kind != yaml.ScalarNode || st.failed[path] {
			continue // the decoder or expander reports it
		}
		if item.Value == "" {
			st.nodeError(item, path, "include pattern is empty")
			continue
		}
		pattern := item.Value
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(dir, pattern)
		}
		found, err := st.opts.Glob(pattern)
		if err != nil {
			st.nodeError(item, path, "invalid include pattern: "+globReason(err))
			continue
		}
		if len(found) == 0 {
			st.nodeError(item, path, "include pattern matches no files")
			continue
		}
		for _, f := range found {
			matches = append(matches, includeMatch{file: f, abs: absPath(f), item: item, path: path})
		}
	}

	slices.SortStableFunc(matches, func(a, b includeMatch) int { return strings.Compare(a.abs, b.abs) })
	seen := map[string]bool{absPath(st.root): true}
	for _, m := range matches {
		if seen[m.abs] {
			continue
		}
		seen[m.abs] = true
		st.includeFile(root, m)
	}
}

// globReason describes a Glob error without repeating the pattern.
func globReason(err error) string {
	if errors.Is(err, filepath.ErrBadPattern) {
		return "malformed glob syntax"
	}
	return err.Error()
}

// includeFile loads one included file and merges its sections into root.
func (st *loadState) includeFile(root *yaml.Node, m includeMatch) {
	data, err := st.opts.ReadFile(m.file)
	if err != nil {
		var pe *fs.PathError
		reason := err
		if errors.As(err, &pe) {
			reason = pe.Err
		}
		st.nodeError(m.item, m.path, fmt.Sprintf("cannot read included file %s: %v", m.file, reason))
		return
	}
	st.sources = append(st.sources, m.abs)
	doc := st.parse(m.file, data)
	if doc == nil {
		return
	}
	if doc.Kind != yaml.MappingNode {
		st.addProblem(SeverityError, doc, "", "an included file must be a mapping of providers and models, got "+kindName(doc))
		return
	}
	seen := make(map[string]bool)
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k, v := doc.Content[i], doc.Content[i+1]
		if seen[k.Value] {
			st.addProblem(SeverityError, k, shownKey(k.Value), "duplicate key")
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "include":
			st.addProblem(SeverityError, k, k.Value, "include is only allowed in the root file")
		case "version":
			st.expandTree(v, k.Value)
			if v = resolveAlias(v); v.Kind != yaml.ScalarNode || v.ShortTag() != intTag || v.Value != "1" {
				st.addProblem(SeverityError, v, k.Value, "the version of an included file must be 1")
			}
		case "providers", "models":
			v = resolveAlias(v)
			if v.Kind == yaml.ScalarNode && v.ShortTag() == nullTag {
				continue
			}
			if v.Kind != yaml.SequenceNode {
				st.addProblem(SeverityError, v, k.Value, "expected a sequence, got "+kindName(v))
				continue
			}
			if dst := rootSequence(root, k.Value); dst != nil {
				dst.Content = append(dst.Content, v.Content...)
			}
		default:
			// A key that looks like a credential is not echoed.
			shown := shownKey(k.Value)
			what := shown
			if shown == k.Value {
				what = strconv.Quote(shown)
			}
			st.addProblem(SeverityError, k, shown, fmt.Sprintf(
				"%s is not allowed in included file %s; only version, providers and models are", what, m.file))
		}
	}
}

// rootSequence returns the root mapping's sequence for key, creating it if
// the key is absent or null. It returns nil if the key holds something else,
// which the decoder reports as a type error.
func rootSequence(root *yaml.Node, key string) *yaml.Node {
	v := mappingValue(root, key)
	if v == nil {
		v = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
		return v
	}
	v = resolveAlias(v)
	switch {
	case v.Kind == yaml.SequenceNode:
		return v
	case v.Kind == yaml.ScalarNode && v.ShortTag() == nullTag:
		v.Kind, v.Tag, v.Value = yaml.SequenceNode, "!!seq", ""
		return v
	}
	return nil
}

// resolveAlias follows alias nodes to the node they refer to.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// kindName describes a node's kind for type-error messages.
func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	}
	return "nothing"
}
