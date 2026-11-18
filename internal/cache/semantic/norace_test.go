//go:build !race

package semantic

// raceEnabled reports whether the tests run under the race detector, which
// slows the vector scans down many times over.
const raceEnabled = false
