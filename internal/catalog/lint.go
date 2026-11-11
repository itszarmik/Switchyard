package catalog

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// StaleAfter is how old pricing.checked may be before Lint reports it.
const StaleAfter = 90 * 24 * time.Hour

// Level classifies a lint Finding.
type Level int

const (
	// LevelWarning is a catalog problem; `switchyard catalog lint` fails on
	// it.
	LevelWarning Level = iota
	// LevelNotice is something unusual that may be intended; it is
	// reported but does not fail lint.
	LevelNotice
)

// String returns "warning" or "notice".
func (l Level) String() string {
	switch l {
	case LevelWarning:
		return "warning"
	case LevelNotice:
		return "notice"
	default:
		return "Level(" + strconv.Itoa(int(l)) + ")"
	}
}

// Finding is one result of Lint, located like a config.Problem.
type Finding struct {
	Level Level
	// File and Line locate Path, as config.Snapshot.Locate does.
	File string
	Line int
	// Path is the config path, such as "models[0].deployments[1].pricing".
	Path    string
	Message string
}

// String formats the finding as "FILE:LINE: LEVEL: PATH: MESSAGE", the
// format of config.Problem, omitting empty or zero parts.
func (f Finding) String() string {
	parts := make([]string, 0, 4)
	loc := f.File
	if f.Line > 0 {
		if loc != "" {
			loc += ":" + strconv.Itoa(f.Line)
		} else {
			loc = "line " + strconv.Itoa(f.Line)
		}
	}
	if loc != "" {
		parts = append(parts, loc)
	}
	parts = append(parts, f.Level.String())
	if f.Path != "" {
		parts = append(parts, f.Path)
	}
	if f.Message != "" {
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, ": ")
}

// Lint reports catalog findings for a loaded configuration, as of now:
//   - every warning the config loader reported about models or aliases,
//     such as missing prices;
//   - a deployment without pricing.source or pricing.checked;
//   - pricing.checked older than StaleAfter;
//   - notices: a chat deployment whose output price is below its input
//     price, and a chat model or deployment without streaming.
//
// Findings are sorted by file, line and path. Values are never echoed, so
// findings are safe to print.
func Lint(snap *config.Snapshot, now time.Time) []Finding {
	var out []Finding
	add := func(level Level, path, msg string) {
		file, line := snap.Locate(path)
		out = append(out, Finding{Level: level, File: file, Line: line, Path: path, Message: msg})
	}
	for _, w := range snap.Warnings() {
		if isCatalogPath(w.Path) {
			out = append(out, Finding{Level: LevelWarning, File: w.File, Line: w.Line, Path: w.Path, Message: w.Message})
		}
	}

	today := now.UTC().Truncate(24 * time.Hour)
	cfg := snap.Config()
	for i := range cfg.Models {
		m := &cfg.Models[i]
		base := "models[" + strconv.Itoa(i) + "]"
		chat := m.Type == config.ModelTypeChat
		modelStreams := slices.Contains(m.Capabilities, string(schema.CapStreaming))
		reportedModel := false
		for j := range m.Deployments {
			d := &m.Deployments[j]
			dp := base + ".deployments[" + strconv.Itoa(j) + "]"
			pp := dp + ".pricing"
			if d.Pricing.Source == "" {
				add(LevelWarning, pp+".source", "missing; record the URL the prices were checked against")
			}
			switch checked, err := time.Parse(config.DateLayout, d.Pricing.Checked); {
			case d.Pricing.Checked == "":
				add(LevelWarning, pp+".checked", "missing; record the date the prices were checked, as YYYY-MM-DD")
			case err != nil:
				// The config loader rejects it; nothing to add.
			case today.Sub(checked) > StaleAfter:
				days := int(today.Sub(checked) / (24 * time.Hour))
				add(LevelWarning, pp+".checked", fmt.Sprintf("prices were checked %d days ago, more than %d; check them again",
					days, int(StaleAfter/(24*time.Hour))))
			}
			if chat && d.Pricing.Input.IsSet() && d.Pricing.Output.IsSet() &&
				d.Pricing.Output.Float64() < d.Pricing.Input.Float64() {
				add(LevelNotice, pp+".output", "unusual: output cheaper than input")
			}
			if !chat {
				continue
			}
			switch {
			case d.Capabilities != nil && !slices.Contains(d.Capabilities, string(schema.CapStreaming)):
				add(LevelNotice, dp+".capabilities", "a chat deployment without capability streaming; streamed requests never go to it")
			case d.Capabilities == nil && !modelStreams && !reportedModel:
				reportedModel = true
				add(LevelNotice, base+".capabilities", "a chat model without capability streaming; streamed requests never go to it")
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Path, b.Path))
	})
	return out
}

// isCatalogPath reports whether a config path is about the catalog.
func isCatalogPath(path string) bool {
	for _, root := range []string{"models", "aliases"} {
		if path == root || strings.HasPrefix(path, root+"[") || strings.HasPrefix(path, root+".") {
			return true
		}
	}
	return false
}
