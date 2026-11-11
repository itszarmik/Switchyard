package quality

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"switchyard/internal/schema"
)

// Signal names, which are also the keys of the heuristic's weights
// (DESIGN §10.3). Each signal yields a feature fᵢ in [0, 1].
const (
	// SignalLength is the estimated input tokens, log-scaled over about
	// 100 to 32k tokens.
	SignalLength = "length"
	// SignalCode is fenced code, stack traces, a dense ratio of code
	// punctuation and programming vocabulary. Category coding.
	SignalCode = "code"
	// SignalMath is LaTeX, equations, and proof or derivation requests.
	// Category reasoning.
	SignalMath = "math"
	// SignalReasoningCues is wording such as "step by step", "trade-offs",
	// "design" or "analyze".
	SignalReasoningCues = "reasoning_cues"
	// SignalConstraints is the number of explicit instructions and bullet
	// or numbered requirements.
	SignalConstraints = "constraints"
	// SignalConversationDepth is the number of turns before the last user
	// message.
	SignalConversationDepth = "conversation_depth"
	// SignalTools is tool definitions, more of them and a forced tool call
	// counting for more. Category tool_use.
	SignalTools = "tools"
	// SignalStrictSchema is response_format json_schema (or json_object),
	// a larger schema counting for more. Category extraction.
	SignalStrictSchema = "strict_schema"
	// SignalSimpleTask is translate, classify, extract or rewrite requests,
	// requests for a one-word or yes/no answer, and short input. Its
	// default weight is negative.
	SignalSimpleTask = "simple_task"
	// SignalCreative is creative-writing vocabulary (poem, story, toast…).
	// Category creative. Open-ended writing raises difficulty a little.
	SignalCreative = "creative"

	// WeightBias is the weight key of the constant term: a feature that is
	// always 1 and never listed among the fired signals.
	WeightBias = "bias"
)

// signalNames lists the weight keys in their fixed order; feature vectors
// are indexed the same way.
var signalNames = [...]string{
	SignalLength,
	SignalCode,
	SignalMath,
	SignalReasoningCues,
	SignalConstraints,
	SignalConversationDepth,
	SignalTools,
	SignalStrictSchema,
	SignalSimpleTask,
	SignalCreative,
	WeightBias,
}

// Indexes into a feature vector, in signalNames order.
const (
	idxLength = iota
	idxCode
	idxMath
	idxReasoning
	idxConstraints
	idxDepth
	idxTools
	idxSchema
	idxSimple
	idxCreative
	idxBias
	numWeights
)

// defaultWeights are the tuned defaults, in signalNames order. They were
// fitted by hand to testdata/sanity.json (see sanity_test.go).
var defaultWeights = [numWeights]float64{
	idxLength:      0.25,
	idxCode:        0.25,
	idxMath:        0.55,
	idxReasoning:   0.30,
	idxConstraints: 0.25,
	idxDepth:       0.15,
	idxTools:       0.20,
	idxSchema:      0.10,
	idxSimple:      -0.35,
	idxCreative:    0.10,
	idxBias:        0.40,
}

// SignalNames returns the names NewHeuristic accepts as weight keys, in a
// fixed order: every signal, then WeightBias. The caller owns the slice.
func SignalNames() []string {
	return append([]string(nil), signalNames[:]...)
}

// DefaultWeights returns the default weight of every name in SignalNames.
// The caller owns the map.
func DefaultWeights() map[string]float64 {
	m := make(map[string]float64, numWeights)
	for i, name := range signalNames {
		m[name] = defaultWeights[i]
	}
	return m
}

// maxText caps the text the heuristic reads per request, in bytes.
const maxText = 32 << 10

// Heuristic is the rule-based Assessor of DESIGN §10.3. It reads only the
// last user message, the system (and developer) messages, the tools,
// tool_choice and response_format, and at most the first 32 KiB of message
// text. It keeps no request content after Assess returns and makes no
// network calls. A Heuristic is safe for concurrent use.
type Heuristic struct {
	weights [numWeights]float64
	vocab   *vocab
	// bufs holds *[]byte text buffers of capacity maxText+utf8.UTFMax, so
	// that Assess does not allocate one per request. A buffer is zeroed
	// before it goes back, so no request text outlives Assess.
	bufs sync.Pool
}

var _ Assessor = (*Heuristic)(nil)

// NewHeuristic returns a Heuristic with the default weights, overridden by
// weights. A nil or empty map means the defaults. A name not in
// SignalNames, or a weight that is not a finite number in [-1, 1], is an
// error.
func NewHeuristic(weights map[string]float64) (*Heuristic, error) {
	h := &Heuristic{weights: defaultWeights, vocab: newVocab()}
	// Sorted, so that the first problem reported does not depend on map
	// order.
	for _, name := range slices.Sorted(maps.Keys(weights)) {
		w := weights[name]
		i := weightIndex(name)
		if i < 0 {
			return nil, fmt.Errorf("quality: unknown heuristic weight %q (known: %s)", name, strings.Join(signalNames[:], ", "))
		}
		if math.IsNaN(w) || w < -1 || w > 1 {
			return nil, fmt.Errorf("quality: heuristic weight %q must be in [-1, 1], got %v", name, w)
		}
		h.weights[i] = w
	}
	h.bufs.New = func() any {
		b := make([]byte, 0, maxText+utf8.UTFMax)
		return &b
	}
	return h, nil
}

// weightIndex returns the index of name in signalNames, or -1.
func weightIndex(name string) int {
	for i, n := range signalNames {
		if n == name {
			return i
		}
	}
	return -1
}

// Thresholds for a category signal to decide the category.
const (
	minCategoryStrength = 0.3
	// extractCueStrength is the category strength of classify/extract
	// wording, which suggests extraction even without a schema.
	extractCueStrength = 0.5
	// numericMathStrength is the reasoning strength of a mathematical
	// term next to numbers ("is 97 a prime", "expected number of rolls"),
	// which is a math question even when it adds little difficulty.
	numericMathStrength = 0.3
)

// Assess rates req. Hints override the heuristic: a known Task sets the
// category and a Difficulty (clamped to [0, 1]) sets the difficulty; either
// sets Hinted. An unknown Task is ignored. Signals lists the signals that
// fired, in SignalNames order, whether or not hints were applied.
func (h *Heuristic) Assess(req *schema.ChatRequest, hints Hints) Assessment {
	var f features
	if req != nil {
		bp := h.bufs.Get().(*[]byte)
		var used []byte
		f, used = h.extract(req, (*bp)[:0])
		clear(used)
		h.bufs.Put(bp)
	}

	d := 0.0
	for i, w := range h.weights {
		d += w * f.v[i]
	}
	a := Assessment{
		Category:   f.category(),
		Difficulty: clamp01(d),
	}
	if n := f.fired(); n > 0 {
		a.Signals = make([]string, 0, n)
		for i := range idxBias {
			if f.v[i] > 0 {
				a.Signals = append(a.Signals, signalNames[i])
			}
		}
	}
	if IsCategory(hints.Task) {
		a.Category = hints.Task
		a.Hinted = true
	}
	if hints.HasDifficulty {
		a.Difficulty = clamp01(hints.Difficulty)
		a.Hinted = true
	}
	return a
}

// IsCategory reports whether s is one of the Category constants.
func IsCategory(s string) bool {
	switch s {
	case CategoryGeneral, CategoryCoding, CategoryReasoning, CategoryExtraction, CategoryCreative, CategoryToolUse:
		return true
	}
	return false
}

// clamp01 clamps v to [0, 1]; NaN becomes 0.
func clamp01(v float64) float64 {
	switch {
	case v > 1:
		return 1
	case v >= 0:
		return v
	default:
		return 0
	}
}

// features is the feature vector of one request, plus the category
// strengths that are not themselves features.
type features struct {
	v          [numWeights]float64
	extractCue bool
	// codeVocab is the coding strength of programming vocabulary alone:
	// naming a language or tool makes a request a coding one even when
	// it adds little difficulty.
	codeVocab float64
	// numericMath reports a mathematical term together with numbers.
	numericMath bool
}

// fired returns the number of signals with a non-zero feature.
func (f *features) fired() int {
	n := 0
	for i := range idxBias {
		if f.v[i] > 0 {
			n++
		}
	}
	return n
}

// category returns the strongest category signal at or above
// minCategoryStrength, or general. Ties go to the earlier category in the
// order coding, reasoning, tool_use, extraction, creative.
func (f *features) category() string {
	extraction := f.v[idxSchema]
	if f.extractCue {
		extraction = max(extraction, extractCueStrength)
	}
	reasoning := f.v[idxMath]
	if f.numericMath {
		reasoning = max(reasoning, numericMathStrength)
	}
	best, bestStrength := CategoryGeneral, minCategoryStrength-1e-9
	for _, c := range [...]struct {
		name     string
		strength float64
	}{
		{CategoryCoding, max(f.v[idxCode], f.codeVocab)},
		{CategoryReasoning, reasoning},
		{CategoryToolUse, f.v[idxTools]},
		{CategoryExtraction, extraction},
		{CategoryCreative, f.v[idxCreative]},
	} {
		if c.strength > bestStrength {
			best, bestStrength = c.name, c.strength
		}
	}
	return best
}

// extract computes the features of req, using buf as scratch space for
// the lowercased text, and returns the part of buf it used. buf must have
// a capacity of at least maxText+utf8.UTFMax, so that it is never
// reallocated.
func (h *Heuristic) extract(req *schema.ChatRequest, buf []byte) (features, []byte) {
	var f features
	msgs := req.Messages()

	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = i
			break
		}
	}

	// The last user message first, so that it survives the cap; then the
	// system and developer messages. Their sizes count only their text, in
	// full: neither the JSON around it nor inline images, which would make
	// a short question about a picture look long.
	userBytes, sysBytes := 0, 0
	if lastUser >= 0 {
		buf, userBytes = appendContent(buf, msgs[lastUser].Content, maxText)
	}
	userEnd := len(buf)
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if len(buf) < maxText {
				buf = append(buf, '\n')
			}
			var n int
			buf, n = appendContent(buf, m.Content, maxText)
			sysBytes += n
		}
	}

	// Conversation depth: the messages other than system ones before the
	// last user message.
	turns := 0
	for i, m := range msgs {
		if i < lastUser && m.Role != "system" && m.Role != "developer" {
			turns++
		}
	}
	f.v[idxDepth] = clamp01(float64(turns) / 8)

	// Length: estimated input tokens of what was read, log-scaled.
	tokens := float64(userBytes+sysBytes) / 4
	if tokens > 100 {
		f.v[idxLength] = clamp01(math.Log(tokens/100) / math.Log(320))
	}

	t := scanText(buf, userEnd, h.vocab)
	f.v[idxCode] = t.code()
	f.v[idxMath] = t.math()
	// The square root makes the first cue count most: one open-ended
	// "explain" or "compare" already separates a request from a lookup.
	f.v[idxReasoning] = math.Sqrt(clamp01(float64(t.reasoning) / 4))
	f.v[idxConstraints] = clamp01(float64(t.bullets+t.constraintWords) / 8)
	f.v[idxCreative] = clamp01(float64(t.creative) / 2)
	f.extractCue = t.extractCue
	if t.codeWords > 0 {
		f.codeVocab = min(0.5, 0.15+0.15*float64(t.codeWords))
	}
	f.numericMath = t.mathWords > 0 && t.digits > 0

	// Simple task: a cue at the start of the user message, which counts
	// less the longer the message is (a long "extract …" over pages of
	// text is not simple), or a short user message.
	userTokens := float64(userBytes) / 4
	simple := clamp01((shortTokens - userTokens) / shortRamp)
	if t.simpleCue {
		simple = max(simple, clamp01(1-(userTokens-cueFullTokens)/cueFadeTokens))
	}
	f.v[idxSimple] = simple

	f.v[idxTools] = toolsFeature(req)
	f.v[idxSchema] = schemaFeature(req)
	f.v[idxBias] = 1
	return f, buf
}

// toolsFeature scores the tools signal: 0 without tools; otherwise 0.4 for
// the first tool, 0.075 per further tool and 0.2 for a forced tool call.
func toolsFeature(req *schema.ChatRequest) float64 {
	n := 0
	if raw, ok := req.Field("tools"); ok {
		n += countElements(raw)
	}
	if raw, ok := req.Field("functions"); ok {
		n += countElements(raw)
	}
	if n == 0 {
		return 0
	}
	v := 0.4 + 0.075*float64(n-1)
	if forcesTool(req.ToolChoice()) {
		v += 0.2
	}
	return clamp01(v)
}

// forcesTool reports whether tool_choice forces a tool call: "required",
// or an object naming a function.
func forcesTool(raw []byte) bool {
	i := skipWS(raw, 0)
	if i >= len(raw) {
		return false
	}
	if raw[i] == '{' {
		return true
	}
	s, next := rawStringBytes(raw, i)
	return next >= 0 && string(s) == "required"
}

// schemaFeature scores the strict_schema signal: json_schema is 0.5 plus
// the schema's size (1 at 3 KB), json_object is 0.3, anything else 0.
func schemaFeature(req *schema.ChatRequest) float64 {
	raw, ok := req.Field("response_format")
	if !ok {
		return 0
	}
	typ, ok := stringMember(raw, "type")
	if !ok {
		return 0
	}
	switch string(typ) {
	case "json_schema":
		return clamp01(0.5 + float64(len(raw))/3000)
	case "json_object":
		return 0.3
	}
	return 0
}

// Shape of the simple_task feature.
const (
	// A user message of at most shortTokens−shortRamp estimated tokens
	// is fully "short"; one of shortTokens or more is not short at all.
	shortTokens = 28
	shortRamp   = 16
	// A simple-task cue counts fully up to cueFullTokens and fades to
	// nothing over the next cueFadeTokens.
	cueFullTokens = 40
	cueFadeTokens = 120
	// cueLead is how far into the user message, in bytes, a simple-task
	// cue word may start: the instruction verb leads ("Translate …",
	// "Classify …"), whereas a later "spell" or "rewrite" is usually part
	// of a larger task.
	cueLead = 64
)

// textStats are the counts one pass over the request text produces.
type textStats struct {
	length int

	fences      int    // ``` markers
	fencedLines int    // lines inside fenced code blocks
	stack       uint16 // stack* bits: the kinds of stack-trace evidence seen
	codeSyms    int    // code punctuation bytes
	codeWords   int    // distinct programming terms
	latexCmds   int    // backslash commands such as \frac
	dollars     int    // $ signs (LaTeX math delimiters)
	carets      int    // ^, as in x^2
	equals      int    // =
	digits      int
	mathWords   int // distinct mathematical terms
	proofCue    bool
	reasoning   int // distinct reasoning cues
	creative    int // distinct creative-writing terms
	bullets     int // bulleted or numbered lines
	simpleCue   bool
	extractCue  bool

	constraintWords int // occurrences of instruction words and phrases
}

// Kinds of stack-trace evidence, as textStats.stack bits.
const (
	stackFrameAt    uint16 = 1 << iota // an indented "at …" frame line (Java, JavaScript)
	stackFileLine                      // file.go:123, File.java:87, app.py:3, x.js:10
	stackPyLine                        // Python's `File "x.py", line 3`
	stackErrorColon                    // panic:, error:, FooError:, BarException:
	stackRustError                     // error[E0502]
	stackTraceback                     // "traceback"
	stackRunning                       // goroutine 1 [running]
	stackSegv                          // sigsegv
	stackSegfault                      // segmentation fault
)

// code scores the code signal.
func (t *textStats) code() float64 {
	v := 0.0
	if t.fences > 0 {
		v += 0.25 + 0.25*clamp01(float64(t.fencedLines)/30)
	}
	v += 0.6 * clamp01(float64(bits.OnesCount16(t.stack))/2)
	if t.length >= 40 {
		v += 0.25 * clamp01((float64(t.codeSyms)/float64(t.length)-0.015)/0.05)
	}
	v += 0.2 * clamp01(float64(t.codeWords)/3)
	return clamp01(v)
}

// math scores the math signal.
func (t *textStats) math() float64 {
	v := 0.0
	if t.proofCue {
		v += 0.7
	}
	latex := t.latexCmds
	if t.dollars >= 2 {
		latex++
	}
	v += 0.4 * clamp01(float64(latex)/3)
	v += 0.3 * clamp01(float64(t.mathWords)/3)
	if t.digits > 0 && (t.carets > 0 || t.equals > 0) && t.fences == 0 {
		v += 0.2
	}
	return clamp01(v)
}

// scanText makes one pass over the lowercased text b, of which b[:userEnd]
// is the last user message, and returns its counts. Words are looked up
// once each; phrases are matched from their first word.
func scanText(b []byte, userEnd int, v *vocab) textStats {
	t := textStats{length: len(b)}
	var seen [cueBits / 64]uint64

	lineStart, inFence := true, false
	lead := min(userEnd, cueLead)
	for i := 0; i < len(b); {
		c := b[i]
		if lineStart {
			lineStart = false
			if isBullet(b, i) {
				t.bullets++
			}
			if isFrameLine(b, i) {
				t.stack |= stackFrameAt
			}
		}
		switch {
		case c == '\n':
			lineStart = true
			if inFence {
				t.fencedLines++
			}
		case c == '`' && i+2 < len(b) && b[i+1] == '`' && b[i+2] == '`':
			t.fences++
			inFence = !inFence
			i += 3
			continue
		case isLower(c):
			j := i + 1
			for j < len(b) && isWordByte(b[j]) {
				j++
			}
			end := j
			for end-i > 1 && (b[end-1] == '-' || b[end-1] == '\'') {
				end--
			}
			w := b[i:end]
			if k, ok := v.words[string(w)]; ok {
				if k.class != 0 {
					t.count(k.class, k.id, &seen, i < lead, i < userEnd)
				}
				for _, p := range k.phrases {
					if bytes.HasPrefix(b[end:], p.rest) {
						t.count(p.class, p.id, &seen, i < lead, i < userEnd)
					}
				}
			}
			t.stack |= stackWord(b, i, end)
			i = j
			continue
		case c >= '0' && c <= '9':
			t.digits++
		case c == ':':
			if i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '9' && hasSourceExt(b[:i]) {
				t.stack |= stackFileLine
			}
		case c == '\\':
			// LaTeX commands have two or more letters (\frac, \pi);
			// \d or \n are regex or escape sequences.
			if i+2 < len(b) && isLower(b[i+1]) && isLower(b[i+2]) {
				t.latexCmds++
			}
		case c == '$':
			t.dollars++
		case c == '^':
			t.carets++
		case c == '=':
			t.equals++
			t.codeSyms++
		case c == '[':
			t.codeSyms++
			if i+9 <= len(b) && string(b[i:i+9]) == "[running]" {
				t.stack |= stackRunning
			}
		case c == '{' || c == '}' || c == ';' || c == '(' || c == ')' || c == '<' || c == '>' || c == ']':
			t.codeSyms++
		}
		i++
	}
	return t
}

// stackWord returns the stack-trace evidence of the word b[i:end].
func stackWord(b []byte, i, end int) uint16 {
	w := b[i:end]
	next := byte(0)
	if end < len(b) {
		next = b[end]
	}
	switch string(w) {
	case "traceback":
		return stackTraceback
	case "sigsegv":
		return stackSegv
	case "segmentation":
		if end+6 <= len(b) && string(b[end:end+6]) == " fault" {
			return stackSegfault
		}
	case "line":
		// File "x.py", line 3
		if i >= 3 && string(b[i-3:i]) == "\", " {
			return stackPyLine
		}
	case "error":
		if end+2 <= len(b) && string(b[end:end+2]) == "[e" {
			return stackRustError
		}
	}
	if next == ':' && (string(w) == "panic" || hasSuffix(w, "error") || hasSuffix(w, "exception")) {
		return stackErrorColon
	}
	return 0
}

// hasSuffix reports whether w ends in suffix, without allocating.
func hasSuffix(w []byte, suffix string) bool {
	return len(w) >= len(suffix) && string(w[len(w)-len(suffix):]) == suffix
}

// hasSourceExt reports whether b ends in a source file name with one of
// the extensions that appear in stack traces: name.go, name.py, name.js or
// name.java.
func hasSourceExt(b []byte) bool {
	for _, ext := range [...]string{".go", ".py", ".js", ".java"} {
		n := len(b) - len(ext)
		if n > 0 && string(b[n:]) == ext && isWordByte(b[n-1]) {
			return true
		}
	}
	return false
}

// isFrameLine reports whether the line starting at b[i] is an indented
// stack frame such as "\tat com.acme.Foo.bar(Foo.java:12)".
func isFrameLine(b []byte, i int) bool {
	j := i
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	return j > i && j+3 <= len(b) && string(b[j:j+3]) == "at "
}

// isLower reports whether c is a lowercase ASCII letter.
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }

// isWordByte reports whether c continues a word: a lowercase letter, a
// digit, an apostrophe, a hyphen or an underscore.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '\'' || c == '-' || c == '_' || c == '+'
}

// isBullet reports whether the line starting at b[i] is a bulleted or
// numbered list item: "- ", "* ", "• " or "1. " / "1) ", after optional
// indentation.
func isBullet(b []byte, i int) bool {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	if i+1 >= len(b) {
		return false
	}
	switch {
	case (b[i] == '-' || b[i] == '*') && b[i+1] == ' ':
		return true
	case bytes.HasPrefix(b[i:], []byte("• ")):
		return true
	}
	j := i
	for j < len(b) && j-i < 3 && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	return j > i && j+1 < len(b) && (b[j] == '.' || b[j] == ')') && b[j+1] == ' '
}

// count records one vocabulary hit of the given class and id. leading
// reports that the hit starts within cueLead bytes of the user message,
// inUser that it is in the user message at all.
func (t *textStats) count(class cueClass, id uint16, seen *[cueBits / 64]uint64, leading, inUser bool) {
	if class == cueConstraint {
		t.constraintWords++
		return
	}
	bit := uint64(1) << (id % 64)
	if seen[id/64]&bit != 0 {
		return
	}
	switch class {
	case cueSimple, cueExtract:
		// Only the lead of the user's own request says whether the task
		// is simple; system prompts commonly mention classifying or
		// extracting.
		if !leading {
			return
		}
		t.simpleCue = true
		if class == cueExtract {
			t.extractCue = true
		}
	case cueSimplePhrase:
		// "one word", "yes or no": anywhere in the user's request.
		if !inUser {
			return
		}
		t.simpleCue = true
	case cueReasoning:
		t.reasoning++
	case cueMath:
		t.mathWords++
	case cueProof:
		t.proofCue = true
		t.mathWords++
	case cueCode:
		t.codeWords++
	case cueCreative:
		t.creative++
	}
	seen[id/64] |= bit
}
