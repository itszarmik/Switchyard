package eval

import (
	"fmt"
	"strings"
	"testing"

	"switchyard/internal/usage"
)

func TestReadPrompts(t *testing.T) {
	const ok = `{"id":"a","request":{"model":"m","messages":[{"role":"user","content":"secret-text"}]}}`
	tests := []struct {
		name, text, err string
		n               int
	}{
		{name: "one", text: ok + "\n\n", n: 1},
		{name: "category", text: `{"id":"a","category":"coding","request":{"model":"m","messages":[{"role":"user","content":"x"}]}}`, n: 1},
		{name: "empty", text: "\n", err: "empty"},
		{name: "not json", text: "secret-text\n", err: "line 1: not a JSON prompt object"},
		{name: "no id", text: `{"request":{"model":"m","messages":[]}}`, err: "line 1: missing id"},
		{name: "duplicate", text: ok + "\n" + ok, err: `line 2: duplicate id "a" (first on line 1)`},
		{name: "no request", text: `{"id":"a"}`, err: "missing request"},
		{name: "bad category", text: `{"id":"a","category":"poetry","request":{"model":"m","messages":[]}}`, err: `unknown category "poetry"`},
		{name: "bad request", text: `{"id":"a","request":{"messages":[{"content":"secret-text"}]}}`, err: "line 1: invalid request: Missing required parameter: 'model'."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps, err := ReadPrompts(strings.NewReader(tt.text))
			switch {
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("error %v, want %q", err, tt.err)
			case tt.err == "" && (err != nil || len(ps) != tt.n):
				t.Errorf("%d prompts, %v", len(ps), err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-text") {
				t.Errorf("error echoes content: %v", err)
			}
		})
	}
}

func TestReadResults(t *testing.T) {
	const a1 = `{"id":"a","prompt_set":"s","model":"m","category":"general","difficulty":0.5,"error":"judge: x"}`
	const a2 = `{"id":"a","prompt_set":"s","model":"m","category":"general","difficulty":0.5,"outcome":"equivalent","acceptable":true}`
	const b = `{"id":"b","prompt_set":"s","model":"m","category":"general","difficulty":0.5,"outcome":"reference_better"}`
	res, torn, err := ReadResults(strings.NewReader(a1 + "\n" + b + "\n" + a2 + "\n" + `{"id":"c","mod`))
	if err != nil {
		t.Fatalf("a torn last line must be ignored: %v", err)
	}
	if len(res) != 3 || torn != int64(len(`{"id":"c","mod`)) {
		t.Fatalf("%d results, torn %d", len(res), torn)
	}
	if _, torn, err := ReadResults(strings.NewReader(a1 + "\n" + b)); err != nil || torn != 0 {
		t.Errorf("a complete last line without a newline: torn %d, %v", torn, err)
	}
	if !Spent([]Result{{CandidateCostUSD: usage.FromNanoUSD(1000), ReferenceCostUSD: usage.FromNanoUSD(20), JudgeCostUSD: usage.FromNanoUSD(3)}, {JudgeCostUSD: usage.FromNanoUSD(4)}}).Equal(usage.FromNanoUSD(1027)) {
		t.Error("Spent does not add up every cost")
	}
	latest := Latest(res)
	if len(latest) != 2 || latest[0].ID != "a" || !latest[0].Judged() || latest[1].ID != "b" {
		t.Errorf("Latest = %+v", latest)
	}
	done := Completed(res)
	if !done[Key{"s", "a", "m"}] || !done[Key{"s", "b", "m"}] || len(done) != 2 {
		t.Errorf("Completed = %v", done)
	}
	if _, _, err := ReadResults(strings.NewReader("garbage\n" + b + "\n")); err == nil {
		t.Error("a bad complete line must be an error")
	}
}

func TestFitRows(t *testing.T) {
	var res []Result
	for i := range 40 {
		d := float64(i%4) / 3 // 0, 1/3, 2/3, 1
		res = append(res, Result{ID: fmt.Sprintf("p%d", i), PromptSet: "s", Model: "m", Category: "coding", Judge: "j@p", Reference: "r@p",
			Difficulty: d, Outcome: "equivalent", Acceptable: i%5 != 0})
	}
	res = append(res,
		Result{ID: "x", PromptSet: "s", Model: "m", Category: "general", Difficulty: 0.5, Outcome: "equivalent", Acceptable: true, Judge: "j@p", Reference: "r@p"},
		Result{ID: "y", PromptSet: "s", Model: "m", Category: "general", Difficulty: 0.5, Error: "judge: x"},
	)
	prior := func(model, category string) (float64, bool, bool) {
		if category == "coding" {
			return 0.6, false, true
		}
		return 0.7, true, true
	}
	rows := Fit(res, 10, prior)
	if len(rows) != 2 || rows[0].Category != "coding" || rows[1].Category != "general" {
		t.Fatalf("rows %+v", rows)
	}
	c := rows[0]
	if c.Judged != 40 || c.Estimate.N != 40 || c.Estimate.Informative != 30 || !c.Written || c.Prior != 0.6 {
		t.Errorf("coding row %+v", c)
	}
	g := rows[1]
	if g.Judged != 1 || g.Failed != 1 || g.Written || !g.Estimate.Insufficient || !g.PriorFallback {
		t.Errorf("general row %+v", g)
	}
	ov := Overrides(rows, "2026-11-12")
	if len(ov) != 1 || ov[0].Samples != 30 || ov[0].Evaluated != "2026-11-12" || ov[0].CILow > ov[0].Value || ov[0].Value > ov[0].CIHigh ||
		ov[0].Source != "switchyard eval: prompts=s judge=j@p reference=r@p" {
		t.Errorf("overrides %+v", ov)
	}
	var sb strings.Builder
	if err := WriteReport(&sb, rows, 10); err != nil {
		t.Fatal(err)
	}
	// general's one acceptable sample gives the interval [1, 1].
	for _, want := range []string{"leaves out the 10 with difficulty 0", "1 failed", "insufficient", "0.700 (general)",
		"zero-width interval: every sample agrees, so N_INFORMATIVE (1) is the guide"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, sb.String())
		}
	}
	if strings.Count(sb.String(), "zero-width") != 1 {
		t.Errorf("only general's interval has zero width:\n%s", sb.String())
	}
	if again := Fit(res, 10, prior); again[0].Estimate != c.Estimate {
		t.Error("Fit is not deterministic")
	}
}
