package core

// ShareTag is a share's tag in this installation. The tag of a handle is
// written into every record appended through it, and a file belongs to the
// share whose tag its records carry. A tag names one share once: it is never
// given to another share, so a record under a forgotten tag can never be read
// as a later share's.
type ShareTag uint32

// ShareID is a share's identity across the store. It names the source share of
// a clone, which may differ from the destination's, and the share a snapshot
// hold belongs to.
type ShareID [16]byte

// SnapshotCut is a share's cut number: the counter a snapshot raises. A
// version's cut is the cut of the commit that made it exist.
type SnapshotCut uint64

// JournalID is a journal's identity: a random 128-bit value chosen when the
// journal is created and never changed. It seeds the checksum of every
// structure in the journal's segments, so a segment of another journal never
// verifies here.
type JournalID [16]byte

// InstallationID is the identity of the installation a journal belongs to,
// written into the journal when it is created and rewritten only by a restamp.
type InstallationID [16]byte

// SegmentID names a segment. Ids start at 1 and ascend, across every append
// stream, in the order segments are created; 0 names no segment.
type SegmentID uint64

// Seq is a record's sequence number: the order this journal appended records
// in. It comes from one counter per journal, never per file or per segment,
// and no sequence number is issued twice. Where two records carry the same
// content version, the higher sequence number wins.
type Seq uint64

// SyncMark is a per-stream sync mark a caller recorded durably and hands Open,
// which recovery checks the scanned tails against. Stream is the append
// stream's number, as its segments' headers carry it; Segment the segment id
// the stream's last recorded sync reached; Through the stream's synced-through
// offset there, never past a slot whose write failed or had not completed;
// Offset the end of the last record that sync covered, which may lie past
// Through and never names a seal marker, footer or trailer; Sequence that
// record's sequence number.
type SyncMark struct {
	Stream   uint32
	Segment  uint64
	Through  int64
	Offset   int64
	Sequence uint64
}

// SegmentRange is a byte range of one segment: Len bytes starting at file
// offset Off of segment Segment.
type SegmentRange struct {
	Segment SegmentID
	Off     int64
	Len     int64
}
