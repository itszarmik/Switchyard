package config

// Shadow-sampling defaults (DESIGN §10.7), for the evaluation.shadow fields
// the file does not set.
const (
	// DefaultShadowConcurrency is evaluation.shadow.concurrency.
	DefaultShadowConcurrency = 4
	// DefaultShadowMaxOutputTokens is evaluation.shadow.max_output_tokens.
	DefaultShadowMaxOutputTokens = 1024
	// DefaultVerifiedMinSamples is evaluation.shadow.verified_min_samples.
	DefaultVerifiedMinSamples = 30
	// DefaultVerifiedMinPassRate is evaluation.shadow.verified_min_pass_rate.
	DefaultVerifiedMinPassRate = 0.9
)

// applyEvaluationDefaults fills the zero-valued evaluation.shadow limits.
// None of them may be 0, so a zero value always takes the default.
func applyEvaluationDefaults(e *Evaluation) {
	s := &e.Shadow
	setDefault(&s.Concurrency, DefaultShadowConcurrency)
	setDefault(&s.MaxOutputTokens, DefaultShadowMaxOutputTokens)
	setDefault(&s.VerifiedMinSamples, DefaultVerifiedMinSamples)
	setDefault(&s.VerifiedMinPassRate, DefaultVerifiedMinPassRate)
}
