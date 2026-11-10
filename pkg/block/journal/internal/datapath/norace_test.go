//go:build !race

package datapath

// raceEnabled is whether the race detector is on, which changes allocation
// counts and makes sync.Pool drop items at random.
const raceEnabled = false
