package quality

import "strings"

// cueClass says what a vocabulary word is evidence of.
type cueClass uint8

const (
	cueReasoning    cueClass = iota + 1 // raises difficulty
	cueMath                             // mathematical term
	cueProof                            // asks for a proof or derivation
	cueCode                             // programming term
	cueCreative                         // creative-writing term
	cueSimple                           // a simple transformation (translate, rewrite…)
	cueExtract                          // a simple classification or extraction
	cueConstraint                       // an explicit instruction word
	cueSimplePhrase                     // asks for a short answer ("one word")
)

// cueBits bounds the number of distinct vocabulary entries tracked per
// request.
const cueBits = 256

// vocabulary lists the lowercase words the heuristic recognizes, by class.
// Words are matched whole; inflections are listed explicitly.
var vocabulary = [...]struct {
	class cueClass
	words []string
}{
	{cueReasoning, []string{
		"design", "architect", "architecture", "analyze", "analysis",
		"analyse", //nolint:misspell // British spelling, as users write it
		"tradeoff", "tradeoffs", "trade-off", "trade-offs", "justify", "justifying", "compare",
		"evaluate", "optimize", "optimal", "scalable", "scalability", "consistency",
		"consistent", "correctness", "correct", "argue", "critique", "implications",
		"strategy", "rigorous", "rigorously", "invariant", "invariants", "linearizability",
		"concurrency", "concurrent", "race", "deadlock", "distributed", "explain",
		"why", "reasoning", "isolate", "diagnose", "investigate", "migration",
		"rollback", "failover", "step-by-step", "recommend", "rank", "conflict",
	}},
	{cueMath, []string{
		"integral", "derivative", "eigenvalue", "eigenvalues", "matrix", "probability",
		"expected", "equation", "equations", "polynomial", "inequality", "duality",
		"converges", "convergent", "limit", "limits", "infinitely", "prime", "primes",
		"modulo", "solve", "sequence", "linear", "optimum", "expectation", "variance",
		"combinatorics", "permutation", "permutations", "algebra", "calculus",
		"formula", "sample", "power",
	}},
	{cueProof, []string{
		"prove", "proof", "proofs", "derive", "derivation", "theorem", "lemma",
		"induction", "qed", "impossibility",
	}},
	{cueCode, []string{
		"function", "python", "javascript", "typescript", "golang", "rust", "java",
		"c++", "php", "sql", "query", "regex", "bash", "script", "compile",
		"compiler", "bug", "debug", "refactor", "unit", "api", "kubernetes", "docker",
		"variable", "method", "async", "await", "git", "github", "code", "endpoint",
		"node.js", "struct", "goroutine", "mutex", "callback", "docstring", "test",
		"tests", "implement", "crawler", "heap", "stack", "pod", "runtime",
		"atomic", "queue", "lock-free", "unsafe", "generic", "generics",
	}},
	{cueCreative, []string{
		"poem", "haiku", "limerick", "sonnet", "story", "novel", "chapter", "lyrics",
		"song", "toast", "slogan", "tagline", "fiction", "verse", "rhyme",
		"screenplay", "narrator", "plot", "catchy", "pentameter",
	}},
	{cueSimple, []string{
		"translate", "rewrite", "rephrase", "capitalize", "define",
		"synonym", "antonym", "plural", "spell", "grammar", "proofread", "rename",
	}},
	{cueExtract, []string{
		"classify", "extract", "categorize", "label", "sentiment", "spam",
	}},
	{cueConstraint, []string{
		"must", "should", "never", "always", "exactly", "ensure", "without",
		"avoid", "only", "include", "requirements", "constraints", "require",
		"required", "preserve", "strict",
	}},
}

// phrases are multi-word cues, matched when their first word is followed
// by the rest verbatim.
var phrases = [...]struct {
	class cueClass
	text  string
}{
	{cueReasoning, "step by step"},
	{cueReasoning, "trade off"},
	{cueReasoning, "pros and cons"},
	{cueReasoning, "root cause"},
	{cueReasoning, "edge case"},
	{cueReasoning, "explain why"},
	{cueReasoning, "show your work"},
	{cueProof, "show that"},
	{cueProof, "prove that"},
	{cueSimplePhrase, "one word"},
	{cueSimplePhrase, "yes or no"},
	{cueSimplePhrase, "one sentence"},
	{cueSimplePhrase, "one-line"},
	{cueSimplePhrase, "keep it short"},
	{cueSimplePhrase, "in short"},
	{cueConstraint, "do not"},
	{cueConstraint, "don't"},
	{cueConstraint, "at least"},
	{cueConstraint, "at most"},
	{cueConstraint, "no more than"},
	{cueConstraint, "no other"},
}

// phrase is the lookup form of one entry of phrases: the bytes after its
// first word.
type phrase struct {
	rest  []byte
	class cueClass
	id    uint16
}

// vocab is the lookup form of vocabulary and phrases, built once per
// Heuristic. Each word maps to its own cue (class 0 if it only starts
// phrases) and the phrases it starts, so the scanner looks each word up
// once.
type vocab struct {
	words map[string]wordCue
}

// wordCue is a vocab entry.
type wordCue struct {
	class   cueClass
	id      uint16
	phrases []phrase
}

// newVocab builds a vocab. A word listed twice keeps its first class.
func newVocab() *vocab {
	v := &vocab{words: make(map[string]wordCue)}
	var id uint16
	for _, group := range vocabulary {
		for _, w := range group.words {
			if _, dup := v.words[w]; dup {
				continue
			}
			v.words[w] = wordCue{class: group.class, id: id % cueBits}
			id++
		}
	}
	for _, p := range phrases {
		first, rest := p.text, ""
		if i := strings.IndexByte(p.text, ' '); i >= 0 {
			first, rest = p.text[:i], p.text[i:]
		}
		e := v.words[first]
		e.phrases = append(e.phrases, phrase{rest: []byte(rest), class: p.class, id: id % cueBits})
		v.words[first] = e
		id++
	}
	return v
}
