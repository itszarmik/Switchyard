// Package conformance checks that the official openai-go SDK behaves the
// same against Switchyard as against an OpenAI-wire upstream.
//
// The suite runs twice: directly against the fake upstream in
// switchyard/test/fakeupstream, which validates both the fake and the suite,
// and through the gateway binary, built and started as a black box by
// switchyard/test/conformance/harness. Assertions that only make sense
// through the gateway (its headers, forwarding rules, model_not_found, auth
// and error mapping) run only then.
//
//	go test ./test/conformance/...          # both; the gateway runs are skipped with -short
//	go test -run Direct ./test/conformance  # against the fake only
package conformance
