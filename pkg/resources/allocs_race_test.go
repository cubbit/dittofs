//go:build race

package resources

// underRace reports the race detector's presence, which keeps sync.Pool's
// private per-P caches from reusing, so the alloc gate skips.
const underRace = true
