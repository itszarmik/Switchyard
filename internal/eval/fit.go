package eval

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"

	"switchyard/internal/config"
)

// DefaultMinSamples is the default number of informative samples an
// estimate needs before it is written as an override.
const DefaultMinSamples = MinInformative

// Row is the fit of one (model, category) pair.
type Row struct {
	Model, Category string
	// Judged is the number of verdicts; Failed the number of lines whose
	// pair failed (an answer or the judge), which are left out of the fit.
	Judged, Failed int
	// Acceptable is the number of acceptable verdicts.
	Acceptable int
	// Estimate is the fit over the verdicts; Estimate.N is Judged.
	Estimate Estimate
	// Prior is the model's current prior for the category, as routing uses
	// it (an existing override included), when HasPrior; PriorFallback
	// reports that it is the general prior, for want of the category's.
	Prior         float64
	HasPrior      bool
	PriorFallback bool
	// Source describes the evaluation: the prompt sets, the judges and the
	// references behind the verdicts.
	Source string
	// Written reports whether the row becomes an override: its estimate
	// is defined and has at least the minimum of informative samples.
	Written bool
}

// PriorFunc returns a model's current quality prior for a category, and
// whether it is the general prior used as a fallback. ok is false when the
// model has neither.
type PriorFunc func(model, category string) (q float64, fallback, ok bool)

// Fit estimates Q for every (model, category) pair of results, keeping the
// latest line of each (prompt, model) pair (see Latest). An estimate with
// at least minSamples informative samples is marked Written; minSamples
// below 1 counts as 1. prior, when set, fills the current priors. Rows are
// sorted by model, then category.
func Fit(results []Result, minSamples int, prior PriorFunc) []Row {
	minSamples = max(minSamples, 1)
	type acc struct {
		row     Row
		samples []Sample
		sources map[string]bool
	}
	byPair := make(map[[2]string]*acc)
	for _, r := range Latest(results) {
		k := [2]string{r.Model, r.Category}
		a := byPair[k]
		if a == nil {
			a = &acc{row: Row{Model: r.Model, Category: r.Category}, sources: map[string]bool{}}
			byPair[k] = a
		}
		if !r.Judged() {
			a.row.Failed++
			continue
		}
		a.row.Judged++
		if r.Acceptable {
			a.row.Acceptable++
		}
		a.samples = append(a.samples, Sample{Difficulty: r.Difficulty, Acceptable: r.Acceptable})
		a.sources[fmt.Sprintf("prompts=%s judge=%s reference=%s", r.PromptSet, r.Judge, r.Reference)] = true
	}
	rows := make([]Row, 0, len(byPair))
	for _, a := range byPair {
		row := a.row
		row.Estimate = EstimateQ(a.samples, Seed(row.Model, row.Category))
		row.Written = row.Estimate.OK && row.Estimate.Informative >= minSamples
		srcs := make([]string, 0, len(a.sources))
		for s := range a.sources {
			srcs = append(srcs, s)
		}
		slices.Sort(srcs)
		row.Source = "switchyard eval: " + strings.Join(srcs, "; ")
		if prior != nil {
			row.Prior, row.PriorFallback, row.HasPrior = prior(row.Model, row.Category)
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b Row) int {
		return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.Category, b.Category))
	})
	return rows
}

// round4 rounds v to 4 decimals, the precision overrides are written at.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// Overrides returns the written rows as quality overrides evaluated on
// date (YYYY-MM-DD), with values rounded to 4 decimals. Samples is the
// number of informative samples.
func Overrides(rows []Row, date string) []config.QualityOverride {
	var out []config.QualityOverride
	for _, r := range rows {
		if !r.Written {
			continue
		}
		e := r.Estimate
		out = append(out, config.QualityOverride{
			Model: r.Model, Category: r.Category,
			Value: round4(e.Q), CILow: round4(e.CILow), CIHigh: round4(e.CIHigh),
			Samples: e.Informative, Evaluated: date, Source: r.Source,
		})
	}
	return out
}

// WriteOverrides writes overrides as a config file that the main config
// can include: version 1 and a quality_overrides sequence, after a header
// comment naming origin.
func WriteOverrides(w io.Writer, overrides []config.QualityOverride, origin string) error {
	doc := struct {
		Version          int                      `yaml:"version"`
		QualityOverrides []config.QualityOverride `yaml:"quality_overrides"`
	}{Version: 1, QualityOverrides: overrides}
	if doc.QualityOverrides == nil {
		doc.QualityOverrides = []config.QualityOverride{}
	}
	header := "# Measured quality priors, written by `switchyard eval fit` from " + origin + ".\n" +
		"# Include this file from the main config (include: [<this file>]); each entry\n" +
		"# replaces the catalog prior of its model and category (DESIGN §10.7).\n"
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("write the overrides: %w", err)
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("write the overrides: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("write the overrides: %w", err)
	}
	return nil
}

// WriteReport prints rows as a table: model, category, verdicts, informative
// samples, Q, its 90% interval, the current prior and the change, and a
// note for insufficient and unwritten estimates, followed by notes on
// what the columns mean.
func WriteReport(w io.Writer, rows []Row, minSamples int) error {
	minSamples = max(minSamples, 1)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCATEGORY\tN\tN_INFORMATIVE\tQ\tCI (90%)\tPRIOR\tCHANGE\tNOTE")
	zero, failed := 0, 0
	for _, r := range rows {
		e := r.Estimate
		zero += e.N - e.Informative
		failed += r.Failed
		q, ci, change := "-", "-", "-"
		if e.OK {
			q = fmt.Sprintf("%.3f", e.Q)
			ci = fmt.Sprintf("[%.3f, %.3f]", e.CILow, e.CIHigh)
		}
		prior := "-"
		if r.HasPrior {
			prior = fmt.Sprintf("%.3f", r.Prior)
			if r.PriorFallback {
				prior += " (general)"
			}
			if e.OK {
				change = fmt.Sprintf("%+.3f", e.Q-r.Prior)
			}
		}
		var notes []string
		switch {
		case !e.OK:
			notes = append(notes, "no informative samples")
		case e.Insufficient:
			notes = append(notes, "insufficient")
		}
		if e.OK && e.CILow == e.CIHigh {
			notes = append(notes, fmt.Sprintf("zero-width interval: every sample agrees, so N_INFORMATIVE (%d) is the guide", e.Informative))
		}
		if !r.Written {
			notes = append(notes, fmt.Sprintf("not written (needs %d informative)", minSamples))
		}
		if r.Failed > 0 {
			notes = append(notes, fmt.Sprintf("%d failed", r.Failed))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			r.Model, r.Category, e.N, e.Informative, q, ci, prior, change, strings.Join(notes, "; "))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	_, err := fmt.Fprintf(w, "\nN counts verdicts; N_INFORMATIVE leaves out the %d with difficulty 0, which carry no information about Q.\n"+
		"Q is the maximum-likelihood estimate of P(acceptable | d) = 1 − d·(1 − Q), with a 90%% percentile-bootstrap interval (%d resamples).\n"+
		"Estimates with fewer than %d informative samples are insufficient. %d failed pairs (answer or judge errors) are not in the fit.\n",
		zero, Resamples, MinInformative, failed)
	if err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}
