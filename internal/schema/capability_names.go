package schema

// The rest of the capability vocabulary that config, the catalog and the
// gateway share. A request never derives these from its body alone (or not
// yet), but deployments declare them in the catalog.
const (
	// CapStreamUsage: the deployment accepts stream_options.include_usage.
	// Without it the gateway does not inject the option, and usage may be
	// missing from streams.
	CapStreamUsage Capability = "stream_usage"
	// CapParallelTools: the deployment honors parallel_tool_calls: true.
	CapParallelTools Capability = "parallel_tools"
	// CapTemperature2: the deployment accepts a temperature above 1, up to 2.
	CapTemperature2 Capability = "temperature_2"
	// CapPenalties: the deployment accepts frequency_penalty and
	// presence_penalty.
	CapPenalties Capability = "penalties"
	// CapCompletions: the deployment serves the legacy /v1/completions
	// natively.
	CapCompletions Capability = "completions"
	// CapPromptCaching: the deployment reports cached-token usage.
	CapPromptCaching Capability = "prompt_caching"
	// CapSampling: the deployment accepts temperature and top_p. Some
	// models document fixed sampling and reject these parameters, so a
	// deployment without it always has them removed (see AlwaysDropped).
	CapSampling Capability = "sampling"
	// CapForcedTools: the deployment honors tool_choice "required" or a
	// specific function. It is a hard requirement.
	CapForcedTools Capability = "forced_tools"
)

// vocabulary is every defined capability, in documentation order.
var vocabulary = [...]Capability{
	CapStreaming, CapStreamUsage, CapTools, CapParallelTools, CapForcedTools,
	CapVision, CapJSONMode, CapJSONSchema, CapN, CapLogprobs, CapSeed,
	CapSampling, CapTemperature2, CapPenalties, CapCompletions,
	CapPromptCaching,
}

// Vocabulary returns every defined capability, in documentation order. The
// result is a fresh slice the caller may modify.
func Vocabulary() []Capability {
	return append([]Capability(nil), vocabulary[:]...)
}

// Known reports whether c is in the vocabulary.
func (c Capability) Known() bool {
	for _, v := range vocabulary {
		if v == c {
			return true
		}
	}
	return false
}

// Droppable reports whether the gateway may remove the request parameters
// that need c, instead of excluding a deployment that lacks c: n, logprobs,
// seed, penalties, temperature_2 and parallel_tools when
// routing.drop_unsupported_params is set, and sampling always (see
// AlwaysDropped). Every other capability is a hard requirement.
func (c Capability) Droppable() bool {
	switch c {
	case CapN, CapLogprobs, CapSeed, CapPenalties, CapTemperature2, CapParallelTools, CapSampling:
		return true
	}
	return false
}

// AlwaysDropped reports whether the parameters that need c are removed for
// a deployment that lacks c regardless of routing.drop_unsupported_params.
// Only sampling is: models without it document fixed sampling, so removing
// temperature and top_p keeps the request's intent better than excluding
// the model.
func (c Capability) AlwaysDropped() bool { return c == CapSampling }
