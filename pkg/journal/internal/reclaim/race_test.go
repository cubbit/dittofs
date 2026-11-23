//go:build race

package reclaim

// raceEnabled is whether the race detector is on, which changes allocation
// counts too much for a check to compare them.
const raceEnabled = true
