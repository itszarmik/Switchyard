package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// decode decodes the merged root node into cfg, reporting unknown fields and
// type errors with their paths and continuing past each one.
func (st *loadState) decode(root *yaml.Node, cfg *Config) {
	st.decodeValue(root, reflect.ValueOf(cfg).Elem(), "")
}

// decodeValue decodes n into v, which must be addressable. It walks structs
// by their yaml tags itself rather than relying on yaml.v3, so that it can
// report every unknown key and every type error, each with a full path, and
// never echo a value that came from a reference.
func (st *loadState) decodeValue(n *yaml.Node, v reflect.Value, path string) {
	st.decodeValueAt(n, n, v, path)
}

// decodeValueAt is decodeValue with the position of path, and problems about
// n itself, located at node at. decodeValue passes n unresolved, so an
// aliased value is located at the alias rather than at its anchor; a merged
// value is located at the merge key it came through.
func (st *loadState) decodeValueAt(at, n *yaml.Node, v reflect.Value, path string) {
	n = resolveAlias(n)
	if at.Line > 0 && path != "" {
		st.pos[path] = position{file: st.fileOf[at], line: at.Line, node: n}
	}
	if st.failed[path] {
		return // the expander already reported a problem here
	}
	if st.failedNodes[n] {
		// The expander reported a problem where the aliased node is defined.
		st.failed[path] = true
		return
	}

	if reflect.PointerTo(v.Type()).Implements(unmarshalerType) {
		u := v.Addr().Interface().(yaml.Unmarshaler)
		if err := u.UnmarshalYAML(n); err != nil {
			msg := err.Error()
			if v.Type() == priceType && strings.HasSuffix(path, ".limit_usd") {
				msg = usdAmountMessage(err)
			}
			st.typeError(at, n, v.Type(), path, msg)
		}
		return
	}

	isNull := n.Kind == yaml.ScalarNode && n.ShortTag() == nullTag
	switch v.Kind() {
	case reflect.Struct:
		if isNull {
			return
		}
		if n.Kind != yaml.MappingNode {
			st.typeError(at, n, v.Type(), path, "expected a mapping")
			return
		}
		st.decodeStruct(n, v, path)

	case reflect.Slice:
		if isNull {
			v.SetZero()
			return
		}
		if n.Kind != yaml.SequenceNode {
			st.typeError(at, n, v.Type(), path, "expected a sequence")
			return
		}
		s := reflect.MakeSlice(v.Type(), len(n.Content), len(n.Content))
		for i, c := range n.Content {
			st.decodeValue(c, s.Index(i), indexPath(path, i))
		}
		v.Set(s)

	case reflect.String:
		if n.Kind != yaml.ScalarNode {
			st.typeError(at, n, v.Type(), path, "expected a string")
			return
		}
		if !isNull {
			v.SetString(n.Value)
		}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if isNull {
			return
		}
		var i int64
		if n.Kind != yaml.ScalarNode || n.ShortTag() != intTag || n.Decode(&i) != nil {
			st.typeError(at, n, v.Type(), path, "expected an integer")
			return
		}
		if v.OverflowInt(i) {
			st.typeError(at, n, v.Type(), path, "integer is out of range")
			return
		}
		v.SetInt(i)

	case reflect.Map:
		if isNull {
			v.SetZero()
			return
		}
		if n.Kind != yaml.MappingNode {
			st.typeError(at, n, v.Type(), path, "expected a mapping")
			return
		}
		st.decodeMap(n, v, path)

	case reflect.Float32, reflect.Float64:
		if isNull {
			return
		}
		var f float64
		tag := n.ShortTag()
		if n.Kind != yaml.ScalarNode || (tag != intTag && tag != floatTag) || n.Decode(&f) != nil {
			st.typeError(at, n, v.Type(), path, "expected a number")
			return
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			st.typeError(at, n, v.Type(), path, "expected a finite number")
			return
		}
		v.SetFloat(f)

	case reflect.Bool:
		if isNull {
			return
		}
		var b bool
		if n.Kind != yaml.ScalarNode || n.ShortTag() != boolTag || n.Decode(&b) != nil {
			st.typeError(at, n, v.Type(), path, "expected a boolean")
			return
		}
		v.SetBool(b)

	case reflect.Pointer:
		// An optional block, such as a rule's set.cache: null or absent
		// leaves it nil, anything else is decoded into a new value.
		if isNull {
			v.SetZero()
			return
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		st.decodeValueAt(at, n, v.Elem(), path)

	default:
		// Only reachable if the schema gains a field of a kind this decoder
		// does not handle yet.
		st.nodeError(at, path, "internal error: unsupported config field type "+v.Type().String())
	}
}

// mergeTag is the tag yaml.v3 gives an unquoted merge key, <<.
const mergeTag = "!!merge"

// isMergeKey reports whether mapping key k is the YAML merge key <<.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.ShortTag() == mergeTag
}

// mappingPair is one key and value of a mapping, after merge keys are
// resolved. via is the merge key the pair came through, or nil for a key
// written in the mapping itself.
type mappingPair struct {
	key, val, via *yaml.Node
}

// mappingPairs returns the pairs of mapping n with its merge keys (<<)
// resolved as in YAML 1.1: keys written in n come first and win, then the
// keys of each merged mapping that are not already present, an earlier
// mapping in a merge sequence winning over a later one. Merged mappings may
// themselves contain merge keys. A malformed merge value, or a mapping that
// merges itself, is reported at path.
//
// The result for each node is computed once, so aliases merged many times
// over cannot make the work grow exponentially.
func (st *loadState) mappingPairs(n *yaml.Node, path string) []mappingPair {
	if pairs, done := st.mergedPairs[n]; done {
		return pairs
	}
	st.merging[n] = true
	defer delete(st.merging, n)

	var pairs []mappingPair
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			merges = append(merges, k, v)
			continue
		}
		pairs = append(pairs, mappingPair{key: k, val: v})
	}

	present := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		present[p.key.Value] = true
	}
	for i := 0; i < len(merges); i += 2 {
		k, v := merges[i], resolveAlias(merges[i+1])
		sources := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			sources = sources[:0]
			for _, c := range v.Content {
				sources = append(sources, resolveAlias(c))
			}
		}
		for _, src := range sources {
			if src.Kind != yaml.MappingNode {
				st.addProblem(SeverityError, k, path, "a merge key (<<) needs a mapping or a sequence of mappings, got "+kindName(src))
				break
			}
			if st.merging[src] {
				st.addProblem(SeverityError, k, path, "a merge key (<<) must not merge a mapping into itself")
				break
			}
			for _, p := range st.mappingPairs(src, path) {
				if p.key.Kind == yaml.ScalarNode && present[p.key.Value] {
					continue
				}
				present[p.key.Value] = true
				if p.via == nil {
					p.via = k
				}
				pairs = append(pairs, p)
			}
		}
	}
	st.mergedPairs[n] = pairs
	return pairs
}

// decodeStruct decodes mapping n into struct v field by field. Keys that
// look like credentials are never echoed: their paths use hiddenKey.
func (st *loadState) decodeStruct(n *yaml.Node, v reflect.Value, path string) {
	fields := structFields(v.Type())
	seen := make(map[string]*yaml.Node)
	var suggested []string
	for _, p := range st.mappingPairs(n, path) {
		k, val := p.key, p.val
		// A problem with a merged key is reported at the merge key.
		at := k
		if p.via != nil {
			at = p.via
		}
		key := k.Value
		childPath := joinPath(path, shownKey(key))
		if k.Kind != yaml.ScalarNode {
			st.addProblem(SeverityError, at, path, "mapping keys must be strings")
			continue
		}
		if first, dup := seen[key]; dup {
			st.nodeError(at, childPath, "duplicate key (first set at line "+strconv.Itoa(first.Line)+")")
			continue
		}
		seen[key] = k
		idx, ok := fields.index[key]
		if !ok {
			msg := "unknown field"
			if shownKey(key) != key {
				msg += " (its name looks like a credential, so it is not shown)"
			} else if s := suggest(key, fields.names); s != "" {
				msg += " (did you mean " + strconv.Quote(s) + "?)"
				suggested = append(suggested, s)
			}
			st.nodeError(at, childPath, msg)
			continue
		}
		if p.via != nil {
			st.decodeValueAt(p.via, val, v.Field(idx), childPath)
		} else {
			st.decodeValue(val, v.Field(idx), childPath)
		}
	}
	// A misspelled key usually means the field it resembles is missing; don't
	// also report that field as required.
	for _, s := range suggested {
		if _, set := seen[s]; !set {
			st.failed[joinPath(path, s)] = true
		}
	}
}

// decodeMap decodes mapping n into map v, whose keys are strings. Entry
// paths are the map's path and the key joined with a dot, as for struct
// fields (for example models[0].quality.coding or aliases.fast); a key that
// looks like a credential is shown as hiddenKey. Duplicate keys are errors,
// including keys that come from different files, such as aliases defined in
// two included files.
func (st *loadState) decodeMap(n *yaml.Node, v reflect.Value, path string) {
	if v.Type().Key().Kind() != reflect.String {
		st.nodeError(n, path, "internal error: unsupported config map key type "+v.Type().Key().String())
		return
	}
	m := reflect.MakeMap(v.Type())
	seen := make(map[string]*yaml.Node)
	for _, p := range st.mappingPairs(n, path) {
		k, val := p.key, p.val
		at := k
		if p.via != nil {
			at = p.via
		}
		if k.Kind != yaml.ScalarNode {
			st.addProblem(SeverityError, at, path, "mapping keys must be strings")
			continue
		}
		key := k.Value
		childPath := joinPath(path, shownKey(key))
		if first, dup := seen[key]; dup {
			st.nodeError(at, childPath, "duplicate key (first set at "+st.nodeWhere(first, at)+")")
			continue
		}
		seen[key] = k
		elem := reflect.New(v.Type().Elem()).Elem()
		if p.via != nil {
			st.decodeValueAt(p.via, val, elem, childPath)
		} else {
			st.decodeValue(val, elem, childPath)
		}
		m.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), elem)
	}
	v.Set(m)
}

// nodeWhere formats the location of node n for a message about node at:
// "line N" when both are in the same file, else "FILE:N".
func (st *loadState) nodeWhere(n, at *yaml.Node) string {
	if f := st.fileOf[n]; f != st.fileOf[at] {
		return f + ":" + strconv.Itoa(n.Line)
	}
	return "line " + strconv.Itoa(n.Line)
}

// fieldSet maps the yaml names of a struct's fields to their indexes.
type fieldSet struct {
	index map[string]int
	names []string // in declaration order, for suggestions
}

// structFields returns the yaml-tagged fields of struct type t.
func structFields(t reflect.Type) fieldSet {
	fs := fieldSet{index: make(map[string]int)}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := yamlName(f)
		if name == "-" {
			continue
		}
		fs.index[name] = i
		fs.names = append(fs.names, name)
	}
	return fs
}

// yamlName returns the key a struct field is written as in YAML.
func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "" {
		return strings.ToLower(f.Name)
	}
	return name
}

// typeError reports, at node at, that node n cannot be decoded into type t.
// The message describes what was found without revealing an expanded value,
// a value that looks like a credential, or anything at all for a Secret.
func (st *loadState) typeError(at, n *yaml.Node, t reflect.Type, path, msg string) {
	st.nodeError(at, path, msg+", got "+st.describeNode(n, t == secretType))
}

// describeNode describes what node n holds, for error messages. Expanded
// scalars are named by their references, and secret scalars, long ones and
// ones that look like credentials by their kind.
func (st *loadState) describeNode(n *yaml.Node, secret bool) string {
	if n.Kind != yaml.ScalarNode {
		return kindName(n)
	}
	if e := st.expanded[n]; e != nil {
		return e.describe()
	}
	if secret || !mayQuote(n.Value) {
		return scalarKind(n)
	}
	return strconv.Quote(n.Value)
}

// scalarKind names the type of scalar n, such as "a string value".
func scalarKind(n *yaml.Node) string {
	switch tag := n.ShortTag(); tag {
	case "!!str":
		return "a string value"
	case intTag:
		return "an integer value"
	case floatTag:
		return "a float value"
	case boolTag:
		return "a boolean value"
	case nullTag:
		return "a null value"
	default:
		return "a " + strings.TrimPrefix(tag, "!!") + " value"
	}
}

// suggest returns the candidate closest to name by edit distance, or "" if
// none is close enough to be a likely typo.
func suggest(name string, candidates []string) string {
	limit := 2
	if len(name) <= 4 {
		limit = 1
	}
	best, bestDist := "", limit+1
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance returns the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
