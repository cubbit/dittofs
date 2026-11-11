package journal

import (
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
)

// Stats returns the journal's statistics: journal-wide, with a per-share
// breakdown. It is served from counters the journal maintains: it neither
// walks the placement index or the segment set nor blocks a write, and it is
// safe to call concurrently with every other operation. After Close it
// returns the zero Stats, since it has no error to return.
//
// decision: fields whose source belongs to a later step (the offload, release
// and repack counters, PinnedSegments, Recovery, RejectedSegments, TornTails,
// ScanResyncs) read zero, and so does UnreclaimedBytes, journal-wide and per
// share: the index has no per-record backing hook to keep it without a walk.
// A zero reads as nothing offered, nothing recovered, nothing unreclaimed; no
// caller may rely on those until the sources land.
func (j *Journal) Stats() Stats {
	// closed is read first: a Close racing this call leaves the core's
	// statistics readable, or this returns the zero Stats, and never both
	// halves.
	if j.closed.Load() || j.core == nil {
		return Stats{}
	}
	return statsFrom(j.core.Stats())
}

// statsFrom converts the core's statistics to the public shape. Maps and
// slices are copied, so the result is the caller's.
func statsFrom(c datapath.Stats) Stats {
	s := Stats{
		MaxBytes: c.MaxBytes, UsedBytes: c.UsedBytes, ReservedBytes: c.ReservedBytes,
		HeldBytes: c.HeldBytes, DirtyBytes: c.DirtyBytes, OldestDirty: c.OldestDirty,
		Files: c.Files, ExtentCount: c.ExtentCount, DropRanges: c.DropRanges,
		Segments: c.Segments, Sealed: c.Sealed, Spares: c.Spares, UnreclaimedBytes: c.UnreclaimedBytes,
		HeadroomBytes: c.HeadroomBytes, RepackReserveBytes: c.RepackReserveBytes,
		PinnedSegments: c.PinnedSegments, OpenDescriptors: c.OpenDescriptors, DescriptorLimit: c.DescriptorLimit,
		DamagedSegments: c.DamagedSegments,
		SyncDuration: Histogram{
			Bounds: slices.Clone(c.SyncDuration.Bounds), Counts: slices.Clone(c.SyncDuration.Counts), Sum: c.SyncDuration.Sum,
		},
		RemovalMarkers: c.RemovalMarkers, VersionLimit: c.VersionLimit, VersionsLeft: c.VersionsLeft,
		NextVersion: c.NextVersion, ExtentLimit: c.ExtentLimit,
		Counters:       Counters(c.Counters),
		Losses:         slices.Clone(c.Losses),
		LossGeneration: c.LossGeneration,
		ReadOnly:       c.ReadOnly,
	}
	if c.LastSyncError != nil {
		s.LastSyncError = &SyncFailure{Err: c.LastSyncError.Err, At: c.LastSyncError.At}
	}
	for _, t := range c.TagConflicts {
		s.TagConflicts = append(s.TagConflicts, TagConflict{File: t.File, Tags: t.Tags})
	}
	s.Shares = make(map[ShareTag]ShareStats, len(c.Shares))
	for t, ss := range c.Shares {
		s.Shares[t] = ShareStats(ss)
	}
	return s
}

// Stats is the journal's statistics. It is not a consistent snapshot of the
// journal. It guarantees only these inequalities, each kept by the order in
// which the counters are updated:
//
//   - DirtyBytes <= HeldBytes;
//   - UsedBytes >= the storage the segments allocate;
//   - ReservedBytes >= 0.
//
// Any other relation between two fields may be momentarily off by one
// in-flight operation.
//
// Each field's comment names its kind: a gauge is a level that rises and
// falls, a counter only rises and restarts at zero on open, a histogram is a
// distribution, and a value is reported as it is rather than charted.
//
// Every map and slice in a Stats belongs to the caller: Stats returns copies
// and never keeps them.
//
// decision: Stats carries no SuspectSegments, SyncMarks or HeadroomSparesShort,
// and Counters no descriptor_overruns or descriptor_open_failures, though RFC 1
// sections 3.7 and 3.9 list them: nothing maintains a source for any of them
// yet. A zero would claim nothing is suspect or short, so each is left out
// until its source lands, which overturns this for that field.
type Stats struct {
	// MaxBytes is the configured capacity. Kind: gauge.
	MaxBytes int64

	// UsedBytes is the local storage allocated: the segments, each at its
	// accounted size. Clean held bytes, dirty bytes and unreclaimed bytes all
	// count here. Kind: gauge.
	UsedBytes int64

	// ReservedBytes is the record bytes reserved by in-flight writes inside
	// allocated segments, not yet written. Kind: gauge.
	ReservedBytes int64

	// HeldBytes is the sum of held extent lengths. Kind: gauge.
	HeldBytes int64

	// DirtyBytes is the held bytes whose offloaded bit is unset. Kind: gauge.
	DirtyBytes int64

	// OldestDirty is when the oldest held extent whose offloaded bit is unset
	// was written: the age of the upload backlog. Zero when nothing is dirty.
	// Kind: value.
	OldestDirty time.Time

	// Files is the number of files with at least one held extent. Kind: gauge.
	Files int

	// ExtentCount is the number of placement index entries: held extents,
	// retained extents and removal markers. Kind: gauge.
	ExtentCount int64

	// DropRanges is the number of ranges the removal-drop test keeps per file
	// and segment. Kind: gauge.
	DropRanges int

	// Segments is the number of segments in total, and Sealed the number of
	// them sealed. Both are gauges.
	Segments, Sealed int

	// Spares is the number of content spare segments the pool holds, made or
	// recycled and taken by no stream yet, each counted in UsedBytes at the
	// segment cap. The headroom, repack reserve and reconcile spares are not
	// counted here. Kind: gauge.
	Spares int

	// UnreclaimedBytes is the storage in segments not yet unlinked that backs
	// no held or retained extent and no live record: what repack can recover.
	// Kind: gauge.
	UnreclaimedBytes int64

	// UnreclaimedEndedBytes is the part of UnreclaimedBytes in segments that
	// take no further append: sealed ones, and ones a failed sync or append
	// ended, whether or not their window has resolved; a segment a running
	// repack has selected counts until it is retired. Kind: gauge.
	//
	// ponytail: always 0 until Repack seals and the journal keeps this
	// figure (RFC 1 §10 questions 5 and 6); the engine reads it as "nothing
	// left to free". Upgrade to the maintained counter when Repack lands.
	UnreclaimedEndedBytes int64

	// HeadroomBytes is the reserved headroom for records without bytes, and
	// RepackReserveBytes the repack reserve, each as allocated now. Both are
	// gauges.
	HeadroomBytes, RepackReserveBytes int64

	// PinnedSegments is the number of segments held by running offers. Kind:
	// gauge.
	PinnedSegments int

	// OpenDescriptors is the number of open segment descriptors, and
	// DescriptorLimit the bound they are held under. Both are gauges.
	OpenDescriptors, DescriptorLimit int

	// DamagedSegments is the number of segments holding records that do not
	// verify. Kind: gauge.
	DamagedSegments int

	// LastSyncError is the most recent sync failure; nil when no sync has
	// failed since open. Kind: value.
	LastSyncError *SyncFailure

	// SyncDuration is the time per sync. Kind: histogram.
	SyncDuration Histogram

	// RemovalMarkers is the number of removal markers not yet settled. Kind:
	// gauge.
	RemovalMarkers int

	// VersionLimit is the version limit, VersionsLeft how many versions remain
	// below it, and NextVersion the next version the counter issues. Kinds:
	// value, gauge, value.
	VersionLimit Version
	VersionsLeft uint64
	NextVersion  Version

	// ExtentLimit is the configured index entry bound. Kind: gauge.
	ExtentLimit int64

	// Counters are the cumulative counts since open. Kind: counters.
	Counters Counters

	// RejectedSegments is the number of segments not attached at open. Kind:
	// gauge.
	RejectedSegments int

	// SuspectSegments is the number of segments holding a record no read could
	// read. Kind: gauge.
	SuspectSegments int

	// Recovery is how the last open rebuilt the journal. Kind: value.
	Recovery RecoveryStats

	// Losses are the most recent loss events, oldest first. The journal keeps
	// a bounded number of them; a poller that sees a gap in their sequence
	// numbers knows how many it missed. Kind: value.
	Losses []LossEvent

	// LossGeneration is raised on every loss of content not yet offloaded and
	// every failed sync window. It is never lower than any value this process
	// has exposed for this journal directory. Kind: value.
	LossGeneration uint64

	// TagConflicts are the files recovery rebuilt under two tags. Every call on
	// such a file is refused until one of the tags is forgotten. Kind: value.
	TagConflicts []TagConflict

	// Shares holds, for every tag that holds a record, attached or not, that
	// share's statistics. Kind: per share.
	Shares map[ShareTag]ShareStats

	// ReadOnly is whether the journal refuses writes, and why. Kind: value.
	ReadOnly ReadOnlyState
}

// ShareStats is one share's statistics. Every field is a gauge.
type ShareStats struct {
	// Limit is the share's limit.
	Limit int64

	// UsedBytes is the record bytes of the share's records still on disk.
	UsedBytes int64

	ReservedBytes int64
	HeldBytes     int64
	DirtyBytes    int64

	// UnreclaimedBytes is the unreclaimed storage counted to the share: that
	// of its records.
	UnreclaimedBytes int64

	// UnreclaimedEndedBytes is the part of UnreclaimedBytes in segments that
	// take no further append, counted to the share as UnreclaimedBytes is. It
	// is 0 until Repack seals (see Stats.UnreclaimedEndedBytes).
	UnreclaimedEndedBytes int64
}

// Counters are the journal's cumulative counts since open. Every field is a
// counter: it only rises, and restarts at zero on open. Each answers something
// only the journal knows.
//
// The descriptor counters of RFC 1 section 3.9 are left out for the reason
// Stats gives.
type Counters struct {
	// WrittenBytes is the client bytes accepted by WriteAt.
	WrittenBytes uint64

	// FilledBytes is the remote bytes placed by Fill.
	FilledBytes uint64

	// FillRefused is the number of fills refused as older than the file.
	FillRefused uint64

	// OfferedBytes is the bytes offered at offload, and MarkedBytes the bytes
	// marked offloaded.
	OfferedBytes, MarkedBytes uint64

	// ReleasedBytes is the bytes released, and FreedBytes the storage actually
	// returned by retiring segments: to the filesystem by an unlink, or to the
	// spare pool by a recycle.
	ReleasedBytes, FreedBytes uint64

	// RepackedBytes is the bytes moved by repack.
	RepackedBytes uint64

	// ReservationRefused is the number of writes refused at capacity.
	ReservationRefused uint64

	// Syncs is the number of syncs issued, and SyncFailures the number failed.
	Syncs, SyncFailures uint64

	// SyncReappendedBytes is the bytes re-appended to a new segment after a
	// failed sync.
	SyncReappendedBytes uint64

	// HeadroomDraws is the number of records without bytes appended from the
	// reserved headroom while the journal was at its limit.
	HeadroomDraws uint64

	// CorruptExtentsDataLoss is the number of extents dropped as corrupt with
	// their offloaded bit unset, and CorruptExtentsRepairable the number with
	// it set.
	CorruptExtentsDataLoss, CorruptExtentsRepairable uint64

	// StaleExtents is the number of extents dropped as older than offloaded
	// content.
	StaleExtents uint64

	// SupersededExtents is the number of extents dropped as older than
	// metadata.
	SupersededExtents uint64

	// TornTails is the number of torn tails truncated at open.
	TornTails uint64

	// ScanResyncs is the number of scans that found a record by
	// resynchronising past a bad one.
	ScanResyncs uint64

	// IndexRefusals is the number of writes refused at the index entry bound.
	IndexRefusals uint64

	// SegmentReopens is the number of reads that had to reopen a closed
	// segment.
	SegmentReopens uint64

	// SpareRefusals is the number of rolls refused at once because no spare
	// was ready.
	SpareRefusals uint64

	// DoubleRetires is the number of retires that subtracted nothing from the
	// accounted footprint.
	DoubleRetires uint64
}

// Histogram is a distribution of durations over fixed buckets.
//
// decision: Bounds, Counts and Sum are what a metrics exporter needs to
// publish the distribution, and nothing more is kept.
type Histogram struct {
	// Bounds are the buckets' upper bounds, ascending.
	Bounds []time.Duration

	// Counts holds one more entry than Bounds: Counts[i] is the number of
	// samples at or below Bounds[i] and above the bound before it, and the
	// last entry the number above every bound.
	Counts []uint64

	// Sum is the total of every sample.
	Sum time.Duration
}

// RecoveryStats is how an open rebuilt the journal.
type RecoveryStats struct {
	// CatalogSegments is the number of segments read by their catalog, and
	// ScannedSegments the number read by scan.
	CatalogSegments, ScannedSegments int

	// Duration is how long the rebuild took.
	Duration time.Duration
}

// SyncFailure is a failed sync, and when it failed.
type SyncFailure struct {
	Err error
	At  time.Time
}

// TagConflict is a file recovery rebuilt under two tags.
type TagConflict struct {
	File FileID
	Tags [2]ShareTag
}
