// Package eval measures model quality on the operator's own prompts
// (DESIGN §10.7, PLAN P8): it runs a prompt set against candidate models
// and a reference model, has an LLM judge compare each candidate answer
// with the reference's, and fits each model's per-category quality prior Q
// with a bootstrap confidence interval, for `switchyard eval`. It also fits
// the heuristic assessor's weights to labeled prompts.
//
// Results never hold prompt or answer content: only ids, models,
// categories, difficulties, outcomes, costs and error kinds.
package eval
