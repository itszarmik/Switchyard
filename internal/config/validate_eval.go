package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// validateEvaluation checks quality_overrides and evaluation.judge (Phase 8).
// models maps each model id to its first definition.
//
// Each override must name a defined model and a known quality category,
// once per (model, category) pair; value, ci_low and ci_high must lie in
// [0, 1] with ci_low ≤ value ≤ ci_high; samples must not be negative; and
// evaluated must be a date as YYYY-MM-DD. The judge model, when set, must
// be a chat model id or a model@provider deployment of the catalog.
func (st *loadState) validateEvaluation(c *Config, models map[string]*Model) {
	first := make(map[[2]string]string)
	for i := range c.QualityOverrides {
		o := &c.QualityOverrides[i]
		base := indexPath("quality_overrides", i)
		st.checkOverrideModel(base+".model", o.Model, models)
		st.checkOverrideCategory(base+".category", o.Category)

		vOK := st.checkOverrideUnit(base+".value", o.Value)
		lowOK := st.checkOverrideUnit(base+".ci_low", o.CILow)
		highOK := st.checkOverrideUnit(base+".ci_high", o.CIHigh)
		if vOK && lowOK && o.CILow > o.Value {
			st.errorAt(base+".ci_low", "must not be greater than value ("+overrideFloat(o.Value)+")")
		}
		if vOK && highOK && o.CIHigh < o.Value {
			st.errorAt(base+".ci_high", "must not be less than value ("+overrideFloat(o.Value)+")")
		}
		if o.Samples < 0 {
			st.errorAt(base+".samples", "must not be negative")
		}
		ep := base + ".evaluated"
		switch _, err := time.Parse(DateLayout, o.Evaluated); {
		case o.Evaluated == "":
			st.errorAt(ep, "required; set the date of the evaluation as YYYY-MM-DD")
		case err != nil:
			st.errorAt(ep, "must be a date as YYYY-MM-DD, got "+st.describe(ep, o.Evaluated))
		}

		if o.Model == "" || o.Category == "" {
			continue
		}
		key := [2]string{o.Model, o.Category}
		if prev, dup := first[key]; dup {
			st.errorAt(base, fmt.Sprintf("duplicate quality override for model %s, category %s; first defined at %s",
				st.describe(base+".model", o.Model), st.describe(base+".category", o.Category), st.where(prev)))
			continue
		}
		first[key] = base
	}

	st.checkJudgeModel("evaluation.judge.model", c.Evaluation.Judge.Model, models)
}

// checkOverrideModel reports an override model that is missing or not in
// the catalog.
func (st *loadState) checkOverrideModel(path, id string, models map[string]*Model) {
	switch {
	case id == "":
		st.errorAt(path, msgRequired)
	case models[id] == nil:
		msg := "unknown model " + st.describe(path, id)
		if s := suggest(id, overrideModelIDs(models)); s != "" && mayQuote(id) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		st.errorAt(path, msg)
	}
}

// checkOverrideCategory reports an override category that is missing or
// unknown.
func (st *loadState) checkOverrideCategory(path, category string) {
	switch {
	case category == "":
		st.errorAt(path, msgRequired+"; use one of "+strings.Join(qualityCategories, ", "))
	case !slices.Contains(qualityCategories, category):
		msg := "unknown quality category " + st.describe(path, category)
		if s := suggest(category, qualityCategories); s != "" && mayQuote(category) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		st.errorAt(path, msg+"; use one of "+strings.Join(qualityCategories, ", "))
	}
}

// checkOverrideUnit reports a value outside [0, 1] and returns whether v is in it.
func (st *loadState) checkOverrideUnit(path string, v float64) bool {
	if math.IsNaN(v) || v < 0 || v > 1 {
		st.errorAt(path, "must be between 0 and 1, got "+overrideFloat(v))
		return false
	}
	return true
}

// checkJudgeModel reports a judge model that is set but is neither a chat
// model id nor a model@provider deployment in the catalog. An empty id is
// valid: shadow sampling, which needs a judge, checks that it is set.
func (st *loadState) checkJudgeModel(path, id string, models map[string]*Model) {
	if id == "" {
		return
	}
	modelID, provider, pinned := strings.Cut(id, "@")
	m := models[modelID]
	switch {
	case m == nil:
		msg := "unknown model " + st.describe(path, id) + "; use a chat model id or model@provider"
		if s := suggest(modelID, overrideModelIDs(models)); s != "" && mayQuote(id) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		st.errorAt(path, msg)
	case m.Type != "" && m.Type != ModelTypeChat:
		st.errorAt(path, "model "+st.describe(path, id)+" has type "+m.Type+"; the judge must be a chat model")
	case pinned && !slices.ContainsFunc(m.Deployments, func(d Deployment) bool { return d.Provider == provider }):
		st.errorAt(path, "deployment "+st.describe(path, id)+" names a provider that does not serve that model")
	}
}

// overrideModelIDs returns the keys of models, sorted.
func overrideModelIDs(models map[string]*Model) []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// overrideFloat formats v in the shortest form that round-trips.
func overrideFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
