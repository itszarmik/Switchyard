// Command configdoc generates docs/configuration.md, the configuration
// reference, from the schema in internal/config/types.go. It reads the file
// with go/parser and walks the struct types from Config: every field with
// a yaml tag becomes a row with its YAML path, its type, its default and
// its doc comment. Defaults are read from the doc comments ("Default 60s.",
// "tenant (default)"), which are the documented contract; a field whose
// comment names no default shows "-".
//
// Usage, from the repository root:
//
//	go run ./tools/configdoc          write docs/configuration.md
//	go run ./tools/configdoc -check   exit 1 if the file is out of date
//
// `make docs-config` runs the first; the package test fails when the
// committed file is stale.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"switchyard/internal/config"
)

// Default paths, relative to the repository root.
const (
	defaultTypes = "internal/config/types.go"
	defaultOut   = "docs/configuration.md"
)

// rootType is the struct the walk starts from.
const rootType = "Config"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command and returns its exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("configdoc", flag.ContinueOnError)
	fset.SetOutput(stderr)
	typesPath := fset.String("types", defaultTypes, "schema `FILE` to read")
	outPath := fset.String("out", defaultOut, "markdown `FILE` to write or check")
	check := fset.Bool("check", false, "fail if -out differs from the generated reference instead of writing it")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	src, err := os.ReadFile(*typesPath)
	if err != nil {
		fmt.Fprintf(stderr, "configdoc: %v\n", err)
		return 1
	}
	doc, err := Generate(*typesPath, src)
	if err != nil {
		fmt.Fprintf(stderr, "configdoc: %v\n", err)
		return 1
	}
	if *check {
		current, err := os.ReadFile(*outPath)
		if err != nil {
			fmt.Fprintf(stderr, "configdoc: %v\n", err)
			return 1
		}
		if !bytes.Equal(current, doc) {
			fmt.Fprintf(stderr, "configdoc: %s is out of date with %s; run `make docs-config`\n", *outPath, *typesPath)
			return 1
		}
		fmt.Fprintf(stdout, "configdoc: %s is up to date\n", *outPath)
		return 0
	}
	if err := os.WriteFile(*outPath, doc, 0o644); err != nil { //nolint:gosec // a documentation file, world-readable on purpose
		fmt.Fprintf(stderr, "configdoc: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "configdoc: wrote %s\n", *outPath)
	return 0
}

// leafTypes are the schema's named value types: each is written as one
// scalar in YAML, whatever its Go representation.
var leafTypes = map[string]string{
	"Duration": "duration",
	"ByteSize": "byte size",
	"Secret":   "secret",
	"Price":    "decimal",
}

// basicTypes maps Go's predeclared types to their YAML description.
var basicTypes = map[string]string{
	"string":  "string",
	"bool":    "boolean",
	"int":     "integer",
	"int64":   "integer",
	"float64": "number",
}

// row is one configuration key.
type row struct {
	path, typ, def, doc string
}

// section is the rows under one top-level key.
type section struct {
	key, doc string
	rows     []row
}

// schema is the parsed types file.
type schema struct {
	types map[string]typeDecl
	// fields maps every Go field name with a yaml tag to that tag, for
	// rewriting Go identifiers in doc comments into YAML keys.
	fields map[string]string
	// keyDocs maps a yaml key and type to the comment of the first field
	// declared with both, for fields that repeat another's key without a
	// comment (PolicyOverride mirrors Policy).
	keyDocs map[string]string
}

// typeDecl is one type declaration and its doc comment.
type typeDecl struct {
	expr ast.Expr
	doc  string
}

// Generate renders the configuration reference for the schema source src,
// read from the file named name.
func Generate(name string, src []byte) ([]byte, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	s := &schema{types: make(map[string]typeDecl), fields: make(map[string]string), keyDocs: make(map[string]string)}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			doc := ts.Doc
			if doc == nil && len(gd.Specs) == 1 {
				doc = gd.Doc
			}
			s.types[ts.Name.Name] = typeDecl{expr: ts.Type, doc: commentText(doc)}
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, fld := range st.Fields.List {
					tag := yamlName(fld)
					if tag == "" {
						continue
					}
					for _, n := range fld.Names {
						s.fields[n.Name] = tag
					}
					k := keyDocKey(tag, fld.Type)
					if doc := commentText(fld.Doc); doc != "" && s.keyDocs[k] == "" {
						s.keyDocs[k] = doc
					}
				}
			}
		}
	}
	root, ok := s.types[rootType]
	if !ok {
		return nil, fmt.Errorf("%s: no type %s", name, rootType)
	}
	rootStruct, ok := root.expr.(*ast.StructType)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a struct", name, rootType)
	}

	top := section{key: "", doc: "Keys at the top level of the file."}
	var sections []section
	for _, fr := range s.fieldRows(rootStruct, "", "") {
		obj, isObject := s.structOf(fr.expr)
		if !isObject {
			top.rows = append(top.rows, fr.row)
			continue
		}
		sec := section{key: fr.path, doc: fr.doc}
		sec.rows = s.walk(obj, fr.path+childSuffix(fr.expr), structDoc(s, fr.expr))
		sections = append(sections, sec)
		top.rows = append(top.rows, fr.row)
	}
	if err := s.checkReachable(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return render(name, s, top, sections), nil
}

// fieldRow is a row plus the field's type expression.
type fieldRow struct {
	row
	expr ast.Expr
}

// fieldRows returns the rows of st's yaml-tagged fields under prefix. A
// field without a comment of its own is described, in order of
// preference, by the previous field's comment when that names it (as in
// "CertFile is ...; KeyFile its PEM private key"), by the comment of
// another field with the same key and type, by its type's comment, or by
// parentDoc, the comment of st's type.
func (s *schema) fieldRows(st *ast.StructType, prefix, parentDoc string) []fieldRow {
	var out []fieldRow
	prevDoc := ""
	for _, fld := range st.Fields.List {
		tag := yamlName(fld)
		doc := commentText(fld.Doc)
		for _, n := range fld.Names {
			if tag == "" {
				continue
			}
			d := doc
			if d == "" && strings.Contains(prevDoc, n.Name) {
				d = prevDoc
			}
			if d == "" {
				d = s.keyDocs[keyDocKey(tag, fld.Type)]
			}
			if d == "" {
				if td, ok := s.typeDocOf(fld.Type); ok {
					d = td
				}
			}
			if d == "" {
				d = parentDoc
			}
			out = append(out, fieldRow{
				row: row{
					path: joinPath(prefix, tag),
					typ:  s.describe(fld.Type),
					def:  extractDefault(d),
					doc:  d,
				},
				expr: fld.Type,
			})
		}
		if doc != "" {
			prevDoc = doc
		}
	}
	return out
}

// walk returns the rows of st and, depth first, of every struct reachable
// from it, with paths under prefix; doc is the comment of st's type.
func (s *schema) walk(st *ast.StructType, prefix, doc string) []row {
	var out []row
	for _, fr := range s.fieldRows(st, prefix, doc) {
		out = append(out, fr.row)
		if obj, ok := s.structOf(fr.expr); ok {
			out = append(out, s.walk(obj, fr.path+childSuffix(fr.expr), structDoc(s, fr.expr))...)
		}
	}
	return out
}

// structDoc returns the comment of the struct type e refers to, or "".
func structDoc(s *schema, e ast.Expr) string {
	d, _ := s.typeDocOf(e)
	return d
}

// keyDocKey identifies a field by its yaml key and its type.
func keyDocKey(tag string, e ast.Expr) string {
	return tag + " " + types.ExprString(e)
}

// checkReachable reports schema struct types that have yaml-tagged fields
// but cannot be reached from Config, which would leave keys undocumented.
func (s *schema) checkReachable() error {
	seen := make(map[string]bool)
	var visit func(e ast.Expr)
	visit = func(e ast.Expr) {
		switch t := e.(type) {
		case *ast.Ident:
			if seen[t.Name] {
				return
			}
			seen[t.Name] = true
			if td, ok := s.types[t.Name]; ok {
				if st, ok := td.expr.(*ast.StructType); ok {
					for _, f := range st.Fields.List {
						visit(f.Type)
					}
				}
			}
		case *ast.StarExpr:
			visit(t.X)
		case *ast.ArrayType:
			visit(t.Elt)
		case *ast.MapType:
			visit(t.Value)
		}
	}
	visit(ast.NewIdent(rootType))
	var missing []string
	for name, td := range s.types {
		st, ok := td.expr.(*ast.StructType)
		if !ok || seen[name] {
			continue
		}
		for _, f := range st.Fields.List {
			if yamlName(f) != "" {
				missing = append(missing, name)
				break
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("struct types not reachable from %s: %s", rootType, strings.Join(missing, ", "))
	}
	return nil
}

// structOf returns the struct type that e refers to directly, through a
// pointer, or as the element of a list or map, when it has yaml-tagged
// fields of its own (leaf types such as Price do not).
func (s *schema) structOf(e ast.Expr) (*ast.StructType, bool) {
	switch t := e.(type) {
	case *ast.StarExpr:
		return s.structOf(t.X)
	case *ast.ArrayType:
		return s.structOf(t.Elt)
	case *ast.MapType:
		return s.structOf(t.Value)
	case *ast.Ident:
		if _, leaf := leafTypes[t.Name]; leaf {
			return nil, false
		}
		td, ok := s.types[t.Name]
		if !ok {
			return nil, false
		}
		st, ok := td.expr.(*ast.StructType)
		return st, ok
	}
	return nil, false
}

// typeDocOf returns the doc comment of the struct type e refers to,
// directly or as a list or map element, for a field without a comment of
// its own.
func (s *schema) typeDocOf(e ast.Expr) (string, bool) {
	switch t := e.(type) {
	case *ast.StarExpr:
		return s.typeDocOf(t.X)
	case *ast.ArrayType:
		return s.typeDocOf(t.Elt)
	case *ast.MapType:
		return s.typeDocOf(t.Value)
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return "", false
	}
	if _, isStruct := s.structOf(id); !isStruct {
		return "", false
	}
	td := s.types[id.Name]
	return td.doc, td.doc != ""
}

// childSuffix is what follows a field's path before its children's keys:
// [] for a list item and .<name> for a map value.
func childSuffix(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return childSuffix(t.X)
	case *ast.ArrayType:
		return "[]"
	case *ast.MapType:
		return ".<name>"
	}
	return ""
}

// describe names e's YAML type.
func (s *schema) describe(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return s.describe(t.X) + " (optional)"
	case *ast.ArrayType:
		return "list of " + plural(s.describe(t.Elt))
	case *ast.MapType:
		return "map of " + s.describe(t.Key) + " to " + s.describe(t.Value)
	case *ast.Ident:
		if d, ok := basicTypes[t.Name]; ok {
			return d
		}
		if d, ok := leafTypes[t.Name]; ok {
			return d
		}
		if _, ok := s.structOf(t); ok {
			return "object"
		}
		return t.Name
	}
	return fmt.Sprintf("%T", e)
}

// plural pluralizes a type description for "list of".
func plural(d string) string {
	switch d {
	case "byte size":
		return "byte sizes"
	}
	if strings.HasSuffix(d, "s") {
		return d
	}
	return d + "s"
}

// yamlName returns the field's yaml key, or "" when it has none.
func yamlName(f *ast.Field) string {
	if f.Tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(reflect.StructTag(raw).Get("yaml"), ",")
	if name == "-" {
		return ""
	}
	return name
}

// joinPath appends key to the dotted path prefix.
func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// commentText returns a comment group's text as one paragraph.
func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.Join(strings.Fields(cg.Text()), " ")
}

var (
	// defaultRE matches "Default 60s.", "Default: x, at least 1." and
	// "Window, default 30s." and stops at the end of the sentence, a
	// semicolon or a colon that ends a clause ("Default false: ..."), not
	// one inside a value (":8080").
	defaultRE = regexp.MustCompile(`(?:\bDefault|, default):? (.+?)(?:\.\s|\.$|;|: |$)`)
	// inlineDefaultRE matches "tenant (default)" and "open (serve; default)".
	inlineDefaultRE = regexp.MustCompile(`([\w.]+) \((?:[^()]*; )?default\)`)
)

// extractDefault returns the default a doc comment documents, or "". With
// several matches the last wins: a field named Default starts its comment
// with "Default applies to ...".
func extractDefault(doc string) string {
	if ms := defaultRE.FindAllStringSubmatch(doc, -1); ms != nil {
		return strings.TrimSpace(ms[len(ms)-1][1])
	}
	if m := inlineDefaultRE.FindStringSubmatch(doc); m != nil {
		return m[1]
	}
	return ""
}

// identRE matches Go identifiers in doc comments, optionally qualified
// (Storage.Mode).
var identRE = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]*(?:\.[A-Z][A-Za-z0-9]*)*\b`)

// proseWords are field names that doc comments also use as ordinary words
// ("Default 60s.", "TCP and TLS"), and so are never rewritten.
var proseWords = map[string]bool{"Default": true, "TLS": true}

// prose rewrites the Go field names in a doc comment into YAML keys in
// code font ("Storage.Mode" becomes `storage.mode`), and escapes the
// characters that would break a table cell or read as HTML. A name inside
// a hyphenated word (X-Switchyard-Tags) is left alone.
func (s *schema) prose(doc string) string {
	doc = cell(doc)
	var b strings.Builder
	last := 0
	for _, loc := range identRE.FindAllStringIndex(doc, -1) {
		id := doc[loc[0]:loc[1]]
		if (loc[0] > 0 && doc[loc[0]-1] == '-') || proseWords[id] {
			continue
		}
		parts := strings.Split(id, ".")
		ok := true
		for i, p := range parts {
			tag, found := s.fields[p]
			if !found {
				ok = false
				break
			}
			parts[i] = tag
		}
		if !ok {
			continue
		}
		b.WriteString(doc[last:loc[0]])
		b.WriteString("`" + strings.Join(parts, ".") + "`")
		last = loc[1]
	}
	b.WriteString(doc[last:])
	return b.String()
}

// cellEscaper escapes text for a markdown table cell: the column separator
// and the angle brackets of placeholders such as <token>, which would
// otherwise read as HTML.
var cellEscaper = strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;")

// cell escapes text for a markdown table cell.
func cell(text string) string {
	return cellEscaper.Replace(text)
}

// defaultCell formats a default: a single token in code font, otherwise
// prose.
func (s *schema) defaultCell(def string) string {
	switch {
	case def == "":
		return "-"
	case !strings.ContainsAny(def, " "):
		return "`" + strings.Trim(def, `"`) + "`"
	}
	return s.prose(def)
}

// render writes the markdown document.
func render(name string, s *schema, top section, sections []section) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Configuration reference\n\n")
	fmt.Fprintf(&b, "<!-- Generated by tools/configdoc from %s. Do not edit; run `make docs-config`. -->\n\n", name)
	b.WriteString(intro)
	fmt.Fprintf(&b, reloadText, restartOnlyList())
	b.WriteString("## Value types\n\n| Type | Written as |\n|---|---|\n")
	leaves := make([]string, 0, len(leafTypes))
	for n := range leafTypes {
		leaves = append(leaves, n)
	}
	sort.Strings(leaves)
	for _, n := range leaves {
		fmt.Fprintf(&b, "| %s | %s |\n", leafTypes[n], cell(s.types[n].doc))
	}
	b.WriteString("\n## Top level\n\n")
	b.WriteString(top.doc + " Sections that hold further keys link to their own table.\n\n")
	b.WriteString("| Key | Type | Default | Description |\n|---|---|---|---|\n")
	keyed := make(map[string]bool, len(sections))
	for _, sec := range sections {
		keyed[sec.key] = true
	}
	for _, r := range top.rows {
		key := "`" + r.path + "`"
		if keyed[r.path] {
			key = "[`" + r.path + "`](#" + anchor(r.path) + ")"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", key, r.typ, s.defaultCell(r.def), s.prose(r.doc))
	}
	for _, sec := range sections {
		fmt.Fprintf(&b, "\n## %s\n\n", sec.key)
		if sec.doc != "" {
			b.WriteString(s.prose(sec.doc) + "\n\n")
		}
		b.WriteString("| Key | Type | Default | Description |\n|---|---|---|---|\n")
		for _, r := range sec.rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.path, r.typ, s.defaultCell(r.def), s.prose(r.doc))
		}
	}
	return b.Bytes()
}

// anchor is the GitHub heading anchor of a section named key.
func anchor(key string) string {
	return strings.ReplaceAll(key, ".", "")
}

// intro explains how to read the tables.
const intro = `Every key Switchyard's config file accepts, with its type, its default and
what it does. Unknown keys are rejected at load time, and
` + "`switchyard config validate`" + ` reports every problem at once. For a
commented example of a complete file, see
[configs/switchyard.example.yaml](../configs/switchyard.example.yaml); for
the compose deployment's file, [deploy/config/switchyard.yaml](../deploy/config/switchyard.yaml).

**Paths.** Keys are written as dotted paths from the top of the file.
` + "`[]`" + ` stands for any item of a list (` + "`providers[].id`" + ` is the
` + "`id`" + ` of each provider) and ` + "`<name>`" + ` for any key of a map
(` + "`routing.profiles.<name>.min_quality`" + `).

**Defaults.** The default column is the value used when a key is left
out; "-" means none is documented, so the zero value applies (empty, 0 or
false). A default that depends on other keys is described in words.

**References.** Any value can be written as ` + "`${VAR}`" + ` (a required
environment variable), ` + "`${VAR:-default}`" + ` or
` + "`${file:/path}`" + `; secrets must be. Settings resolve from the
defaults, then the file and its includes, then the environment overrides
` + "`SWITCHYARD_LISTEN`" + `, ` + "`SWITCHYARD_ADMIN_LISTEN`" + ` and
` + "`SWITCHYARD_LOG_LEVEL`" + `, then command-line flags.

`

// reloadText is the reload paragraph; restartOnlyList fills %s with the
// restart-only paths of config.RestartOnly.
const reloadText = `**Reload.** SIGHUP or ` + "`POST /admin/v1/reload`" + ` applies a changed file
without a restart, except for these keys and everything under them, which
are reported as ` + "`restart_required`" + ` and need one: %s. So do the
features a reload turns on that are built only at startup: the semantic
cache (` + "`policies[].cache.semantic`" + `), shadow sampling
(` + "`policies[].shadow.sample_rate`" + `) and, with ` + "`storage.mode: redis`" + `, a
response cache when none was on at startup (` + "`policies[].cache`" + `).
Everything else applies at once, ` + "`server.admin_token`" + ` and the rate limits
included.

`

// restartOnlyList formats config.RestartOnly for reloadText.
func restartOnlyList() string {
	paths := config.RestartOnly()
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = "`" + p + "`"
	}
	return strings.Join(out, ", ")
}
