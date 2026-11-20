package settings

// Change is one key whose effective value differs between two snapshots.
type Change struct {
	Section string
	Key     string
	Class   Class
}

// Diff lists the keys whose effective value differs from prev to cur; a nil
// prev makes every key a change.
func Diff(prev, cur *Snapshot) []Change {
	panic(unbuilt("Diff"))
}

// Keep is the receiver with section's values taken from prev, so a refused
// section keeps what it last ran.
func (s *Snapshot) Keep(section string, prev *Snapshot) *Snapshot {
	panic(unbuilt("Keep"))
}
