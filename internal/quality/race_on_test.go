//go:build race

package quality

// raceEnabled reports whether the race detector is on; it makes sync.Pool
// drop items at random, so allocation counts are only checked without it.
const raceEnabled = true
