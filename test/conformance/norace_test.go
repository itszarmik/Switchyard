//go:build !race

package conformance

// raceEnabled reports whether the tests run with the race detector, in
// which case the gateway under test is built with it too.
const raceEnabled = false
