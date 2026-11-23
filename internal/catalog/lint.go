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
//   - every warning the config loader reported about models or aliases;
//   - a deployment without the prices routing needs, among the prices in
//     force at now;
//   - a deployment without pricing.source or pricing.checked;
//   - pricing.checked older than StaleAfter;
//   - notices: a dated price change already in force at now, which
//     belongs in the base prices; each deployment's next price change,
//     with its time and the prices it sets; a chat deployment whose output
//     price is below its input price, in its base prices or from a change
//     on; and a chat model or deployment without streaming.
//
// Findings are sorted by file, line and path. Values are never echoed,
// except validated prices and times, so findings are safe to print.
func Lint(snap *config.Snapshot, now time.Time) []Finding {
	var out []Finding
	add := func(level Level, path, msg string) {
		file, line := snap.Locate(path)
		out = append(out, Finding{Level: level, File: file, Line: line, Path: path, Message: msg})
	}
	for _, w := range snap.Warnings() {
		// Unknown prices are checked below, with the prices in force at
		// now rather than at load time.
		if isCatalogPath(w.Path) && w.Message != config.MsgPricingUnknown {
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
			if !config.PricingKnown(m.Type, d.Pricing.At(now)) {
				add(LevelWarning, pp, config.MsgPricingUnknown)
			}
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
			for k, c := range d.Pricing.Changes {
				from, err := config.ParseChangeFrom(c.From)
				if err != nil {
					continue // the config loader rejects it
				}
				if !from.After(now) {
					// A notice, so that lint does not start failing on
					// the day a change takes effect.
					add(LevelNotice, pp+".changes["+strconv.Itoa(k)+"].from",
						"in force since "+formatChangeTime(from)+"; fold it into the base prices and remove it when convenient")
				}
			}
			if k, from, ok := d.Pricing.NextChange(now); ok {
				add(LevelNotice, pp+".changes["+strconv.Itoa(k)+"]",
					"next price change at "+formatChangeTime(from)+": "+changePrices(d.Pricing.Changes[k]))
			}
			if chat && outputCheaper(d.Pricing) {
				add(LevelNotice, pp+".output", "unusual: output cheaper than input")
			}
			for k, c := range d.Pricing.Changes {
				from, err := config.ParseChangeFrom(c.From)
				if err != nil || !chat || (!c.Input.IsSet() && !c.Output.IsSet()) {
					continue
				}
				if outputCheaper(d.Pricing.At(from)) {
					add(LevelNotice, pp+".changes["+strconv.Itoa(k)+"]",
						"unusual: output cheaper than input from "+formatChangeTime(from))
				}
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

// outputCheaper reports whether p sets input and output prices and output
// is the cheaper.
func outputCheaper(p config.Pricing) bool {
	return p.Input.IsSet() && p.Output.IsSet() && p.Output.Float64() < p.Input.Float64()
}

// formatChangeTime formats a price change's time in UTC, such as
// "2026-12-01 00:00 UTC", with seconds only when they are not zero.
func formatChangeTime(t time.Time) string {
	t = t.UTC()
	if t.Second() != 0 || t.Nanosecond() != 0 {
		return t.Format("2006-01-02 15:04:05.999999999 UTC")
	}
	return t.Format("2006-01-02 15:04 UTC")
}

// changePrices lists the prices a change sets, such as "input 4.00,
// output 20.00"; the others stay as they were.
func changePrices(c config.PriceChange) string {
	var parts []string
	for _, f := range []struct {
		name string
		p    config.Price
	}{{"input", c.Input}, {"output", c.Output}, {"cached_input", c.CachedInput}, {"cache_write", c.CacheWrite}} {
		if f.p.IsSet() {
			parts = append(parts, f.name+" "+f.p.String())
		}
	}
	return strings.Join(parts, ", ")
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
