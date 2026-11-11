package config

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Severity classifies a Problem.
type Severity int

const (
	// SeverityError makes the configuration invalid.
	SeverityError Severity = iota
	// SeverityWarning flags a risky but valid setting.
	SeverityWarning
)

// String returns "error" or "warning".
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	default:
		return "Severity(" + strconv.Itoa(int(s)) + ")"
	}
}

// Problem is one error or warning found while loading a configuration.
type Problem struct {
	Severity Severity
	// File is the root path as passed to Load, or for an included file the
	// glob match joined to the root file's directory, so a relative root path
	// stays relative. It is "" when the problem has no file, such as an
	// invalid override.
	File string
	// Line is 1-based, or 0 when unknown.
	Line int
	// Path is the config path, such as "providers[1].api_key" or
	// "server.listen", or "" when the problem is not about one setting.
	Path    string
	Message string
}

// String formats the problem as "FILE:LINE: SEVERITY: PATH: MESSAGE",
// omitting empty or zero parts.
func (p Problem) String() string {
	parts := make([]string, 0, 4)
	loc := p.File
	if p.Line > 0 {
		if loc != "" {
			loc += ":" + strconv.Itoa(p.Line)
		} else {
			loc = "line " + strconv.Itoa(p.Line)
		}
	}
	if loc != "" {
		parts = append(parts, loc)
	}
	parts = append(parts, p.Severity.String())
	if p.Path != "" {
		parts = append(parts, p.Path)
	}
	if p.Message != "" {
		parts = append(parts, p.Message)
	}
	return strings.Join(parts, ": ")
}

// ValidationError holds every problem found by a failed Load, errors and
// warnings, sorted by File, then Line, then Path. It always contains at least
// one error.
type ValidationError struct {
	Problems []Problem
}

// Error lists one problem per line, followed by a summary line with the
// number of errors and warnings.
func (e *ValidationError) Error() string {
	var b strings.Builder
	var errs, warns int
	for _, p := range e.Problems {
		b.WriteString(p.String())
		b.WriteByte('\n')
		if p.Severity == SeverityError {
			errs++
		} else {
			warns++
		}
	}
	fmt.Fprintf(&b, "%s, %s", plural(errs, "error"), plural(warns, "warning"))
	return b.String()
}

// Errors returns only the error-severity problems.
func (e *ValidationError) Errors() []Problem {
	var out []Problem
	for _, p := range e.Problems {
		if p.Severity == SeverityError {
			out = append(out, p)
		}
	}
	return out
}

// plural formats a count with a noun, such as "1 error" or "2 errors".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// sortProblems orders problems by File, then Line, then Path, keeping the
// order in which they were found for ties.
func sortProblems(ps []Problem) {
	slices.SortStableFunc(ps, func(a, b Problem) int {
		return cmp.Or(
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Path, b.Path),
		)
	})
}
