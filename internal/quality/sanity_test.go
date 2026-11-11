package quality

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"switchyard/internal/schema"
)

// Difficulty bands of the sanity set (PLAN P5 acceptance).
const (
	bandEasy   = "easy"   // d < 0.35
	bandMedium = "medium" // 0.35 ≤ d < 0.65
	bandHard   = "hard"   // d ≥ 0.65
)

// sanityEntry is one labeled prompt of testdata/sanity.json. Band was
// assigned by judging the prompt itself, never by running the heuristic;
// Category is the category a reader would assign, recorded for
// information.
type sanityEntry struct {
	ID       string          `json:"id"`
	Band     string          `json:"band"`
	Category string          `json:"category"`
	Request  json.RawMessage `json:"request"`
}

// bandOf returns the band of difficulty d.
func bandOf(d float64) string {
	switch {
	case d < 0.35:
		return bandEasy
	case d < 0.65:
		return bandMedium
	default:
		return bandHard
	}
}

func loadSanity(t testing.TB) []sanityEntry {
	t.Helper()
	data, err := os.ReadFile("testdata/sanity.json")
	if err != nil {
		t.Fatalf("read sanity set: %v", err)
	}
	var entries []sanityEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse sanity set: %v", err)
	}
	return entries
}

// TestSanitySet checks that at least 80 % of the labeled prompts land in
// their difficulty band with the default weights, and lists every
// misclassified prompt for tuning.
func TestSanitySet(t *testing.T) {
	entries := loadSanity(t)
	if len(entries) < 90 {
		t.Fatalf("sanity set has %d prompts, want about 100", len(entries))
	}
	h, err := NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}

	bands := map[string]int{}
	categories := map[string]int{}
	ids := map[string]bool{}
	hits, catHits := 0, 0
	var misses []string
	for _, e := range entries {
		if ids[e.ID] {
			t.Errorf("duplicate id %q", e.ID)
		}
		ids[e.ID] = true
		if e.Band != bandEasy && e.Band != bandMedium && e.Band != bandHard {
			t.Errorf("%s: unknown band %q", e.ID, e.Band)
		}
		if !IsCategory(e.Category) {
			t.Errorf("%s: unknown category %q", e.ID, e.Category)
		}
		bands[e.Band]++
		categories[e.Category]++

		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			t.Fatalf("%s: decode request: %v", e.ID, err)
		}
		a := h.Assess(req, Hints{})
		if a.Category == e.Category {
			catHits++
		}
		if got := bandOf(a.Difficulty); got == e.Band {
			hits++
		} else {
			misses = append(misses, fmt.Sprintf("  %-28s label=%-6s got=%-6s d=%.3f category=%s signals=[%s]",
				e.ID, e.Band, got, a.Difficulty, a.Category, strings.Join(a.Signals, " ")))
		}
	}
	for _, c := range []string{CategoryGeneral, CategoryCoding, CategoryReasoning, CategoryExtraction, CategoryCreative, CategoryToolUse} {
		if categories[c] == 0 {
			t.Errorf("no sanity prompt in category %s", c)
		}
	}

	rate := float64(hits) / float64(len(entries))
	t.Logf("sanity set: %d/%d in band (%.1f%%); bands %v; category agreement %d/%d",
		hits, len(entries), 100*rate, bands, catHits, len(entries))
	if len(misses) > 0 {
		t.Logf("misclassified prompts:\n%s", strings.Join(misses, "\n"))
	}
	if rate < 0.80 {
		t.Errorf("only %.1f%% of the sanity set is in its labeled band, want at least 80%%", 100*rate)
	}
}
