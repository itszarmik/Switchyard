// Package smoke holds the opt-in live smoke test: it runs the gateway
// against a real OpenAI-compatible provider. The test is behind the smoke
// build tag and skips when OPENAI_API_KEY is unset, so CI never calls a
// provider.
//
//	OPENAI_API_KEY=sk-... make smoke
//	OPENAI_API_KEY=sk-... SMOKE_MODEL=gpt-5-nano go test -tags smoke -count=1 -v ./test/smoke
//
// SMOKE_MODEL picks the model (default gpt-5-mini). SMOKE_BASE_URL points
// the gateway at another OpenAI-compatible provider (default
// https://api.openai.com/v1).
package smoke
