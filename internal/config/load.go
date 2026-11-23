package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// LoadOptions replaces process dependencies for callers and tests. Zero
// values mean os.LookupEnv, os.ReadFile and filepath.Glob.
type LoadOptions struct {
	LookupEnv func(key string) (string, bool)
	// ReadFile reads the config files and the files of ${file:…}
	// references. A ${file:…} file larger than 64 KiB is an error. When
	// ReadFile is nil, ${file:…} also only reads regular files, so that a
	// device or FIFO cannot hang the load.
	ReadFile func(name string) ([]byte, error)
	Glob     func(pattern string) ([]string, error)
	// SecretPlaceholders loads the configuration without its secrets, for
	// tools that never call an upstream or a store, such as switchyard
	// route explain. A reference in a secret setting (an API key, a Redis
	// URL, the ledger DSN) reads neither the environment nor a file: a
	// ${VAR:-default} reference takes its default, and any other
	// reference a placeholder that validation accepts (a redis:// or
	// postgres:// URL when the reference is the whole URL). References in
	// other settings resolve as usual.
	SecretPlaceholders bool
	// Now is the time validation resolves dated prices (pricing.changes)
	// at, to tell whether a deployment's prices are known, as the catalog
	// built then would. Nil means time.Now.
	Now func() time.Time
	// Overrides are applied after the files are merged and defaults are set,
	// and before validation, so precedence is defaults < files < Overrides.
	// Supported keys: "server.listen", "server.admin_listen", "logging.level".
	// Any other key is reported as an error problem (Path = the key).
	Overrides map[string]string
}

// Snapshot is an immutable, validated configuration. Callers must not mutate
// the *Config it returns.
type Snapshot struct {
	cfg      *Config
	version  string
	sources  []string
	warnings []Problem
	root     string
	// lines maps each config path decoded from a file to its location.
	lines map[string]fileLine
	// overridden records the config paths set by LoadOptions.Overrides.
	overridden map[string]bool
}

// fileLine is where a config path was written.
type fileLine struct {
	file string
	line int
}

// Locate returns the file and line a config path was written at, such as
// "models[0].deployments[1].pricing", in the form Problem uses. A path that
// is not in any file, such as a field left at its default, is located at its
// nearest ancestor that is, or else at the root file with line 0. A value
// set by an override has no file. Tools that report problems about a loaded
// configuration, such as `switchyard catalog lint`, use it.
func (s *Snapshot) Locate(path string) (file string, line int) {
	if s.overridden[path] {
		return "", 0
	}
	for p := path; p != ""; p = parentPath(p) {
		if fl, ok := s.lines[p]; ok {
			return fl.file, fl.line
		}
	}
	return s.root, 0
}

// Config returns the effective configuration. Callers must not mutate it.
func (s *Snapshot) Config() *Config { return s.cfg }

// Version returns the first 12 hex characters of a SHA-256 over a canonical
// encoding of the effective configuration. Identical input gives an identical
// version; any change to a value, including a secret, changes it.
func (s *Snapshot) Version() string { return s.version }

// Sources returns the absolute paths of every file read: the root first, then
// the included files in load order.
func (s *Snapshot) Sources() []string { return append([]string(nil), s.sources...) }

// Warnings returns the warning-severity problems found while loading.
func (s *Snapshot) Warnings() []Problem { return append([]Problem(nil), s.warnings...) }

// Load reads the root file at path, merges includes, expands references,
// applies defaults and overrides, and validates.
//
// If the root file cannot be read, Load returns a plain wrapped error, and
// errors.Is(err, fs.ErrNotExist) reports a missing file. Every other problem
// (YAML syntax, unknown fields, type errors, include errors, unresolved
// references, validation) is collected and returned together as a
// *ValidationError.
func Load(path string, opts LoadOptions) (*Snapshot, error) {
	st := newLoadState(path, opts)
	data, err := st.opts.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	st.sources = append(st.sources, absPath(path))
	cfg := st.load(data)

	sortProblems(st.problems)
	if !st.hasErrors() {
		lines := make(map[string]fileLine, len(st.pos))
		for p, pos := range st.pos {
			lines[p] = fileLine{file: pos.file, line: pos.line}
		}
		return &Snapshot{
			cfg:        cfg,
			version:    fingerprint(cfg),
			sources:    st.sources,
			warnings:   st.problems,
			root:       st.root,
			lines:      lines,
			overridden: st.overridden,
		}, nil
	}
	return nil, &ValidationError{Problems: st.problems}
}

// position locates the node a config path was decoded from.
type position struct {
	file string
	line int
	node *yaml.Node
}

// loadState carries everything one Load call accumulates.
type loadState struct {
	opts LoadOptions
	// now is LoadOptions.Now, read once.
	now time.Time
	// readRefFile reads the file of a ${file:…} reference.
	readRefFile func(name string) ([]byte, error)
	root        string // the root path as passed to Load
	sources     []string
	problems    []Problem

	// fileOf records the file every parsed node came from.
	fileOf map[*yaml.Node]string
	// expanded records the scalars that contained at least one reference.
	expanded map[*yaml.Node]*expansion
	// visited records the scalars already expanded, so aliases and nodes
	// expanded early are not expanded twice.
	visited map[*yaml.Node]bool
	// pos records where each decoded config path came from.
	pos map[string]position
	// failed records the config paths that already have a decode or
	// expansion error, so validation does not report follow-on errors.
	failed map[string]bool
	// failedNodes records the scalars whose expansion failed, so a path that
	// uses one through an alias is not reported again.
	failedNodes map[*yaml.Node]bool
	// mergedPairs caches the pairs of each mapping with its merge keys
	// resolved, and merging holds the mappings being resolved right now.
	mergedPairs map[*yaml.Node][]mappingPair
	merging     map[*yaml.Node]bool
	// overridden records the config paths set by LoadOptions.Overrides.
	overridden map[string]bool
}

// newLoadState returns the state for loading root, with nil options
// replaced by their defaults.
func newLoadState(root string, opts LoadOptions) *loadState {
	refFile := readRefFile
	if opts.ReadFile != nil {
		refFile = limitRefFile(opts.ReadFile)
	} else {
		opts.ReadFile = os.ReadFile
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.Glob == nil {
		opts.Glob = filepath.Glob
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &loadState{
		opts:        opts,
		now:         opts.Now(),
		readRefFile: refFile,
		root:        root,
		fileOf:      make(map[*yaml.Node]string),
		expanded:    make(map[*yaml.Node]*expansion),
		visited:     make(map[*yaml.Node]bool),
		pos:         make(map[string]position),
		failed:      make(map[string]bool),
		failedNodes: make(map[*yaml.Node]bool),
		mergedPairs: make(map[*yaml.Node][]mappingPair),
		merging:     make(map[*yaml.Node]bool),
		overridden:  make(map[string]bool),
	}
}

// load runs every stage after the root file has been read and returns the
// decoded configuration. Problems accumulate in st.problems.
func (st *loadState) load(data []byte) *Config {
	cfg := &Config{}
	before := len(st.problems)
	root := st.parse(st.root, data)
	if root == nil && len(st.problems) > before {
		return cfg // a syntax error; validating an empty config would only add noise
	}
	if root != nil {
		if root.Kind == yaml.MappingNode {
			if inc := mappingValue(root, "include"); inc != nil {
				st.expandTree(inc, "include")
				st.includeFiles(root, inc)
			}
		}
		st.expandTree(root, "")
		st.decode(root, cfg)
	}
	applyDefaults(cfg, st.explicit)
	st.applyOverrides(cfg)
	st.validate(cfg)
	return cfg
}

// explicit reports whether the files set the config path to a value other
// than null.
func (st *loadState) explicit(path string) bool {
	p, ok := st.pos[path]
	if !ok || p.node == nil {
		return false
	}
	return p.node.Kind != yaml.ScalarNode || p.node.ShortTag() != nullTag
}

func (st *loadState) hasErrors() bool {
	for _, p := range st.problems {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// addProblem records a problem located at node n.
func (st *loadState) addProblem(sev Severity, n *yaml.Node, path, msg string) {
	p := Problem{Severity: sev, Path: path, Message: msg}
	if n != nil {
		p.File = st.fileOf[n]
		p.Line = n.Line
	}
	st.problems = append(st.problems, p)
}

// nodeError records an error at node n and marks path as failed.
func (st *loadState) nodeError(n *yaml.Node, path, msg string) {
	st.addProblem(SeverityError, n, path, msg)
	st.failed[path] = true
}

// yamlLineRE extracts the line number from a yaml.v3 syntax error.
var yamlLineRE = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

// parse parses one file into its top-level node, recording the file of every
// node. It returns nil for an empty file or a syntax error, which it records.
func (st *loadState) parse(file string, data []byte) *yaml.Node {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if !errors.Is(err, io.EOF) {
			st.problems = append(st.problems, syntaxProblem(file, err))
		}
		return nil
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		st.problems = append(st.problems, Problem{
			Severity: SeverityError, File: file, Line: extra.Line,
			Message: "multiple YAML documents are not supported",
		})
	case !errors.Is(err, io.EOF):
		st.problems = append(st.problems, syntaxProblem(file, err))
	}
	st.markFile(&doc, file)
	if len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

// syntaxProblem turns a yaml.v3 parse error into a Problem, moving the line
// number out of the message.
func syntaxProblem(file string, err error) Problem {
	p := Problem{Severity: SeverityError, File: file, Message: strings.TrimPrefix(err.Error(), "yaml: ")}
	if m := yamlLineRE.FindStringSubmatch(err.Error()); m != nil {
		p.Line, _ = strconv.Atoi(m[1])
		p.Message = m[2]
	}
	return p
}

// markFile records file as the origin of n and every node below it.
func (st *loadState) markFile(n *yaml.Node, file string) {
	if n == nil {
		return
	}
	if _, ok := st.fileOf[n]; ok {
		return
	}
	st.fileOf[n] = file
	for _, c := range n.Content {
		st.markFile(c, file)
	}
}

// mappingValue returns the value node for key in mapping m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// absPath returns the absolute form of p, or the cleaned p if the working
// directory is unknown.
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}
