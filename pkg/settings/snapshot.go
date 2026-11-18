package settings

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Snapshot is every section's decoded and validated value, as one node runs
// them.
type Snapshot struct{}

// Change is one key whose effective value differs between two snapshots.
type Change struct {
	Section string
	Key     string
	Class   Class
}

// Read decodes every section of table for node from the settings r lists
// across every scope, and returns the snapshot and the sections it refused,
// each with its error; a refused section does not stop the others.
func Read(
	ctx context.Context,
	r entity.SettingsReader,
	table Table,
	node entity.NodeID,
) (*Snapshot, map[string]error, error) {
	return nil, nil, unbuilt("Read")
}

// Get is the section's value for node in snap, with no decode; an error for a
// section snap holds no value of, such as one held at start.
func Get[T any](snap *Snapshot, section string, node entity.NodeID) (T, error) {
	var zero T

	return zero, unbuilt("Get")
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
