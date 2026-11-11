//go:build race

package unread

// raceEnabled is whether the race build is on, which changes allocation
// counts too much for a check to compare them.
const raceEnabled = true
