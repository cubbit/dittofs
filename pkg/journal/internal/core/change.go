package core

// ChangeKind is what a change Since reports is: a held extent, the marker of
// one of the four removals, or a loss. The zero ChangeKind names none.
type ChangeKind uint8

const (
	// ChangeWrite is a held extent, written or filled.
	ChangeWrite ChangeKind = iota + 1
	// ChangeTruncate is the marker of a Truncate.
	ChangeTruncate
	// ChangeDeallocate is the marker of a Deallocate.
	ChangeDeallocate
	// ChangeDelete is the marker of a Delete.
	ChangeDelete
	// ChangeCloneTarget is the marker of a CloneTarget, which keeps its
	// clone's spec.
	ChangeCloneTarget
	// ChangeLoss is the marker of a synced write the journal lost: the range
	// reads as lost rather than as the file was before the write.
	ChangeLoss
)

// Change is one thing Since reports about a file: a held extent, an unsettled
// removal marker, or a synced loss.
type Change struct {
	Kind    ChangeKind
	Extent  Extent  // the held extent, or the extent a removal marker covers
	Version Version // the content version of the change
	Mtime   int64   // a write's modification time as WriteAt was given it; 0 for every other kind
	// Clone is the clone target's spec; nil for every other kind.
	Clone *CloneSpec
	// Over is, for a write, the runs inside Extent where it superseded
	// another write of the file whose version is above the Since's from, in
	// offset order, disjoint and not adjacent; nil for every other kind and
	// for a write that superseded nothing above from.
	Over []Extent
}
