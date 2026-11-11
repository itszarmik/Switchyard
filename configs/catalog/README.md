# Model catalog

`models.yaml` is the curated Phase 2 catalog that `configs/switchyard.example.yaml` includes. Every model ID, price, context window and output limit in it was checked against the provider's official pages on **2026-11-11**. Each deployment's `pricing.source` and `pricing.checked` record where and when.

Re-check prices at least every 90 days. `switchyard catalog lint` reports older entries. Two Gemini prices change sooner than that (see below).

## Sources

| Provider | Prices | Model IDs, limits, features |
|---|---|---|
| OpenAI | <https://developers.openai.com/api/docs/pricing> | Model pages under <https://developers.openai.com/api/docs/models/>, the GPT-6 guide <https://developers.openai.com/api/docs/guides/latest-model>, the prompt caching guide, the Chat Completions API reference, and <https://developers.openai.com/api/docs/guides/embeddings> |
| Anthropic | <https://platform.claude.com/docs/en/about-claude/pricing> | <https://platform.claude.com/docs/en/about-claude/models/overview> and the structured outputs page. Cross-checked with the `claude-api` skill's model table (cached 2026-11-03), which agrees. |
| Google | <https://aistudio.google.com/docs/pricing> (where `ai.google.dev/gemini-api/docs/pricing` now redirects) | `https://aistudio.google.com/docs/models/<id>`, the OpenAI compatibility page <https://aistudio.google.com/docs/openai> |
| Together | <https://docs.together.ai/docs/serverless/models>, which matches <https://www.together.ai/pricing> | The same table (context length, function calling, structured outputs), plus the chat completions reference, the parameters page and the OpenAI compatibility page |
| Self-hosted | none (see below) | Model card <https://huggingface.co/Qwen/Qwen3.5-9B>, plus vLLM's request schema in `vllm/entrypoints/openai/chat_completion/protocol.py` and `vllm/sampling_params.py` on `main` |

## How the entries were chosen

- **OpenAI.** Small: `gpt-6-luna`. Mid: `gpt-6.1-sol`. Frontier: `gpt-6-astra`. These are the three models OpenAI's model catalog recommends as starting points. The embedding model is `text-embedding-3-small`.
- **Anthropic.** The current lineup from the models overview: `claude-fable-5-2`, `claude-opus-5-5`, `claude-sonnet-5-5` and `claude-haiku-5-5`. Claude Mythos 5.2 is left out because only verified organizations can use it.
- **Google.** `gemini-3.8-flash` (the model in Google's compatibility examples) and `gemini-4-carbon` (the most capable model). Both are stable.
- **Open weight.** `Qwen/Qwen3.5-9B`, an Apache-2.0 model served by Together and on the self-hosted vLLM cluster. Together's serverless catalog currently has no Llama model.

## Price conventions

- Prices are USD per 1M tokens, written as strings so the decimal text stays exact.
- `cached_input` is the cache-hit price. `cache_write` is the per-token cache-write price:
  - Anthropic: the 5-minute write.
  - OpenAI GPT-6: the default 30-minute write (1.25x input).
  - Gemini: not set, because Gemini charges storage per token-hour, not per token written.
- Only one price tier fits the schema, so each entry uses the default tier. These tiers are left out:
  - OpenAI GPT-6 prompts over 272K input tokens cost 2x for input and cache, and 1.5x for output.
  - Claude Haiku 5.5 prompts over 100,000 tokens cost 0.50 input and 2.50 output.
  - Batch, flex and fast/priority tiers, regional uplifts, and Anthropic's 1-hour cache writes.

## Prices to update soon

- `gemini-4-carbon`: Google lists 2.00 / 10.00 / 0.10 "through November 30, 2026", then 4.00 / 20.00 / 0.20 from December 1, 2026.
- `gemini-3.8-flash`: Google lists 0.75 / 3.75 / 0.075 through December 31, 2026, then 1.50 / 7.50 / 0.15 from January 1, 2027.

## Self-hosted price: deliberately unset

`qwen3.5-9b@gpu-cluster` has no price. An amortized price depends on your GPU cost, utilization and measured throughput, and no provider page can verify those. The deployment is therefore commented out in `models.yaml`, so the shipped catalog passes `catalog lint`. Uncomment it once you have a price; an unpriced deployment ranks last and fails `catalog lint`. To compute one:

```
USD per 1M tokens = (GPU $/hour × GPUs) / (sustained tokens/sec × 3600 × utilization) × 1,000,000
```

For example, with made-up numbers that are **not a price**: one GPU at $2.00/hour, 1,500 tokens/s and 50% utilization gives 2 / (1500 × 3600 × 0.5) × 1e6 ≈ 0.74. Use the same figure for input and output unless you measure prefill and decode separately.

## Capabilities

A capability is listed only when the provider documents it for that model, on the endpoint Switchyard calls. The comments in `models.yaml` say why each capability is listed or left out. The main gaps:

- **GPT-6 has no `tools` capability.** OpenAI says Astra and Sol support tool calling only through the Responses API. Luna supports function calling in Chat Completions only with `reasoning_effort: "none"`, and its default is `medium`.
- **json_schema** means native structured outputs everywhere; no adapter emulates it. On Claude, the adapter reduces the client's schema to the subset Anthropic supports, as Anthropic's SDKs do: unsupported keywords (`minimum`, `maxLength`, `maxItems` and so on) move into the field's description and every object gets `additionalProperties: false`.
- **json_mode on Claude.** Listed for all four models. The anthropic adapter emulates `json_object` with a system-prompt instruction, never with a forced tool, so it works on the models that reject forced tools. It is best effort: the output is not constrained to valid JSON. Use `json_schema` (native structured outputs) when validity matters.
- **forced_tools on Claude.** Fable 5.2, Opus 5.5 and Sonnet 5.5 return 400 for a forced `tool_choice` (`any` or `tool`), so only Haiku 5.5 lists `forced_tools`. The gateway never sends `tool_choice: "required"` or a named function to the others.
- **sampling.** Not listed for any Claude model (Opus 5.5 and Fable reject `temperature` and `top_p`; Sonnet 5.5 and Haiku 5.5 reject non-default values), for GPT-6 (the GPT-6 guide says to remove them when reasoning is on, which is the default), or for Gemini (Google's guides say Gemini 3.6+ and Gemini 4 ignore them). The gateway always removes `temperature` and `top_p` for a deployment without `sampling`. Listed for Qwen on Together and vLLM.
- **Gemini's OpenAI endpoint.**
  - `stream_usage` is listed, because Google's own streaming example sends `stream_options.include_usage`.
  - Everything the compatibility page doesn't document is left out: parallel tool calls, json_object, n, logprobs, seed, penalties and cached-token reporting.
  - `forced_tools` is left out: the AI Studio compatibility page shows only `tool_choice: "auto"`. Google's Vertex OpenAI endpoint documents `required`, but not a named function, and it is a different endpoint.
- **Together vs vLLM.** The vLLM deployment is commented out until you set its price (see above). It gets a deployment-level override, because it accepts more than Together documents: `stream_usage`, `n`, `logprobs`, `temperature_2` (it validates temperature in [0, 2]) and `completions`. Tool calling on vLLM needs `--enable-auto-tool-choice --tool-call-parser qwen3_coder`. `prompt_caching` needs `--enable-prompt-tokens-details`.

## Quality priors

These are **starting estimates** (DESIGN §10.2). Only `general` is set, and the other categories fall back to it.

Each value is the model's [Artificial Analysis Intelligence Index](https://artificialanalysis.ai/leaderboards/models) (read 2026-11-11), divided by 70 and rounded to two places. The index is taken at the model's default reasoning effort, or at `high` where no default is documented. I found no public per-category source covering all of these models.

| Model | Effort used | Index | Prior |
|---|---|---|---|
| claude-fable-5-2 | high (default) | 62 | 0.89 |
| gemini-4-carbon | high | 60 | 0.86 |
| gpt-6-astra | high | 55 | 0.79 |
| claude-opus-5-5 | medium (default) | 48 | 0.69 |
| gpt-6.1-sol | medium (default) | 46 | 0.66 |
| claude-sonnet-5-5 | high (default) | 42 | 0.60 |
| gemini-3.8-flash | high | 31 | 0.44 |
| claude-haiku-5-5 | medium (default) | 25 | 0.36 |
| gpt-6-luna | medium (default) | 18 | 0.26 |
| qwen3.5-9b | — | not listed | unset |

Replace these with measurements from your own traffic in Phase 8.
