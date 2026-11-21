package datapath

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// ShareStats is one share's figures; see the journal package's ShareStats.
type ShareStats struct {
	Limit, UsedBytes, ReservedBytes, HeldBytes, DirtyBytes, UnreclaimedBytes int64
	// UnreclaimedEndedBytes is the part of UnreclaimedBytes in segments that
	// take no further append.
	UnreclaimedEndedBytes int64
}

// Counters are the cumulative counts since the core was built.
type Counters struct {
	WrittenBytes, FilledBytes, FillRefused                      uint64
	OfferedBytes, MarkedBytes                                   uint64
	ReleasedBytes, FreedBytes, RepackedBytes                    uint64
	ReservationRefused                                          uint64
	Syncs, SyncFailures, SyncReappendedBytes, HeadroomDraws     uint64
	CorruptExtentsDataLoss, CorruptExtentsRepairable            uint64
	StaleExtents, SupersededExtents, TornTails, ScanResyncs     uint64
	IndexRefusals, SegmentReopens, SpareRefusals, DoubleRetires uint64
}

// Stats is the core's statistics, in the journal package's Stats shape. Every
// map and slice in it belongs to the caller.
type Stats struct {
	MaxBytes, UsedBytes, ReservedBytes, HeldBytes, DirtyBytes int64
	OldestDirty                                               time.Time
	Files                                                     int
	ExtentCount                                               int64
	DropRanges, Segments, Sealed, Spares, HeadroomSparesShort int
	UnreclaimedBytes, HeadroomBytes, RepackReserveBytes       int64
	// UnreclaimedEndedBytes is the part of UnreclaimedBytes in segments that
	// take no further append; see the journal package's Stats.
	UnreclaimedEndedBytes                            int64
	PinnedSegments, OpenDescriptors, DescriptorLimit int
	DamagedSegments                                  int
	LastSyncError                                    *durability.SyncFailure
	SyncDuration                                     durability.Histogram
	RemovalMarkers                                   int
	VersionLimit                                     core.Version
	VersionsLeft                                     uint64
	NextVersion                                      core.Version
	ExtentLimit                                      int64
	Counters                                         Counters
	Losses                                           []core.LossEvent
	LossGeneration                                   uint64
	TagConflicts                                     []index.Conflict
	Shares                                           map[core.ShareTag]ShareStats
	ReadOnly                                         core.ReadOnlyState
}

// Stats reads the maintained counters. It walks neither the index nor the
// segment set and takes no lock a write holds across its append.
//
// decision: the snapshot is not consistent, and the read order keeps only the
// documented inequalities. A writer raises held before dirty and lowers dirty
// before held, so dirty <= held holds at every instant, but that protects only
// a reader seeing both at one instant: a fall landing between a dirty read and
// a held read leaves the pair dirty above held. The pair is therefore re-read
// up to four times until dirty <= held, which a pair read with no fall between
// satisfies. The retries are bounded so a lasting dirty > held (an accounting
// bug) cannot hang a Stats call on a commit path: a pair still out of order
// after them is returned as read, so for that one read the RFC 1 section 3.7
// guarantee DirtyBytes <= HeldBytes does not hold: that signals an accounting
// bug rather than hanging a commit path. A reservation is released only after
// its record is counted in its share's UsedBytes, so ReservedBytes and
// ReservedByTag are read before the set's figures: a record in flight is then
// counted in at least one of them. UsedBytes comes from the set, which
// allocation raises before a record is reserved in it. Any other pair may
// differ by one operation in flight.
//
// decision: UnreclaimedBytes is clamped at zero per share. It is the record
// bytes the set holds for a share less the share's held and retained bytes and
// the record overhead. Held and retained are read first, then the set's
// overhead and bytes, in that order, as atomic loads with no lock held. A write in flight inflates the
// figure by its payload, the set being charged before held rises; that is not
// negative. The clamp's remaining job is a release followed by a retire: held
// falls at publish and the set falls later, at the retire, so a reader that
// read held first can see the set below it once retirement is wired, and a
// retire or a forget that lands between the set's two reads (it lowers the
// bytes before the overhead) can read the figure low as well. Nothing
// retires a segment the index still holds yet, so production cannot reach
// that case; the clamp is the guard for when it can. No bound RFC 1 section 3.7
// states involves the retained, overhead or per-tag byte reads, so their order
// moves only this clamped figure, by one operation in flight. While held is at
// most used, which only the release-then-retire case above breaks, held plus
// unreclaimed is at most used whatever that order is.
//
// ponytail: the figure ignores footer entries, segment headers and trailers,
// which the set does not count, and the header and padding of every record on
// disk stay counted as backed until its segment retires. Ceiling: one record
// header plus padding per dead record, so a segment of dead small writes or
// dead removal records reads near zero while wholly reclaimable. Upgrade to a
// per-record backed-bytes count once repack picks segments by this figure or
// that error shows in a capacity decision.
//
// The components this function reads are the input of the lock test
// (statsLocks in stats_locks_test.go), which holds every mutex they own while
// calling it. A component Stats starts reading must be added there as a root,
// or a lock it takes is not found; the test cannot discover one on its own.
func (c *Core) Stats() Stats {
	var s Stats
	s.MaxBytes = c.maxBytes
	// The reservation is read before the set's per-share figures and must stay
	// so: a write moves its record into the set's figures before it releases
	// its reservation, so this order finds the record in one of the two.
	if c.statsStep != nil {
		c.statsStep("reserved:before")
	}
	reserved, reservedBy := c.cap.ReservedBytes(), c.cap.ReservedByTag()
	if c.statsStep != nil {
		c.statsStep("reserved:after")
	}
	for range 4 {
		s.DirtyBytes = c.dirty.Load()
		if c.betweenHeldReads != nil {
			c.betweenHeldReads()
		}
		if s.HeldBytes = c.held.Load(); s.DirtyBytes <= s.HeldBytes {
			break
		}
	}
	s.OldestDirty = c.oldestDirty()

	sc := c.set.Counters()
	s.UsedBytes, s.Segments, s.Sealed = sc.UsedBytes, int(sc.Segments), int(sc.Sealed)

	ix := c.index
	s.Files, s.ExtentCount, s.DropRanges = int(ix.FilesHeld()), ix.ExtentCount(), ix.DropRanges()
	s.RemovalMarkers, s.ExtentLimit = int(ix.RemovalMarkers()), ix.ExtentLimit()
	s.TagConflicts = ix.TagConflicts()

	pc := c.pool.Counters()
	s.Spares, s.RepackReserveBytes = int(pc.Spares), pc.RepackReserveBytes
	// decision: HeadroomBytes is the headroom's unused reservation in the
	// admission's ledger, not the headroom "as allocated now" that RFC 1 §3.7
	// names. The §9.4 row "removal records dropped per extent" asserts that
	// HeadroomBytes stays above half its size while 10^5 removals draw on it,
	// which only the ledger meaning can show: the allocated headroom spares
	// do not move with the records drawn. The ceiling is a dashboard that
	// reads it as the space set aside on disk: a journal with 16 streams
	// allocates 16 headroom segments of 256 MiB while this reads about 13 MiB.
	// Overturned by an amendment of §3.7, or by a separate gauge of the
	// allocated headroom bytes beside this one.
	s.HeadroomBytes = c.headroom.Unused()
	s.HeadroomSparesShort = int(pc.HeadroomSparesShort)
	ds := c.cache.Stats()
	s.OpenDescriptors, s.DescriptorLimit = ds.Open, ds.Limit

	s.VersionLimit, s.VersionsLeft, s.NextVersion = c.counters.Limit(), c.counters.VersionsLeft(), c.NextVersion()

	d := c.dur.Stats()
	s.DamagedSegments, s.LastSyncError, s.SyncDuration = int(d.DamagedSegments), d.LastSyncError, d.SyncDuration
	s.Losses, s.LossGeneration, s.ReadOnly = d.Losses, d.LossGeneration, d.ReadOnly

	s.Counters = Counters{
		WrittenBytes: c.WrittenBytes(), FilledBytes: c.FilledBytes(), FillRefused: c.FillRefused(),
		FreedBytes:         c.retirer.Counters().FreedBytes,
		RepackedBytes:      c.repackedBytes.Load(),
		ReservationRefused: c.cap.RefusedCount(),
		Syncs:              d.Syncs, SyncFailures: d.SyncFailures, SyncReappendedBytes: d.ReappendedBytes,
		HeadroomDraws:          c.headroom.Draws(),
		CorruptExtentsDataLoss: d.CorruptDataLoss, CorruptExtentsRepairable: d.CorruptRepairable,
		StaleExtents: d.StaleExtents, SupersededExtents: d.SupersededExtents,
		IndexRefusals: ix.Refused(), SegmentReopens: ds.Reopens,
		SpareRefusals: uint64(pc.SpareRefusals), DoubleRetires: sc.DoubleRetires,
	}

	// One entry per tag with an open handle or a record on disk, tag 0 (the
	// journal's own records) excepted.
	if c.statsStep != nil {
		c.statsStep("set:before")
	}
	figs := c.shareFigures()
	used, over := c.set.TagFigures()
	if c.statsStep != nil {
		c.statsStep("set:after")
	}
	s.Shares = make(map[core.ShareTag]ShareStats)
	if p := c.registry.Load(); p != nil {
		for _, h := range *p {
			s.Shares[h.Tag] = ShareStats{Limit: h.Limit()}
		}
	}
	for t := range used {
		if _, ok := s.Shares[t]; !ok {
			s.Shares[t] = ShareStats{}
		}
	}
	delete(s.Shares, 0)
	// The journal-wide half of the unreclaimed bytes of the segments that
	// take no further append — the sealed ones and the failed ones, a
	// running repack's selected among them until they are retired — served
	// from maintained figures, never a walk: the accounted bytes the set
	// counts per state change, less the record overhead those segments
	// carry — so the answer names live payload only, the same basis
	// UnreclaimedBytes rides and never above it — less the live bytes
	// those segments still back. The live half rides the identity that the shares' held and
	// retained figures sum every segment's live bytes, so the no-append
	// side is that sum less the bytes still backed by the segments an
	// append can reach — one per stream, a fixed count. The live figures
	// and the per-segment counters they ride move at the same edges the
	// index publishes, so the pair is off by at most one operation in
	// flight, the bias the rest of Stats documents; the clamp keeps an
	// in-flight publishing read from answering below zero.
	liveAll := int64(0)
	for _, f := range figs {
		liveAll += f.Held + f.Retained
	}
	for i := range c.streams.Len() {
		if id, ok := c.streams.Stream(i).OpenSegment(); ok {
			b := c.index.SegmentBytes(id)
			liveAll -= b.Held + b.Retained
		}
	}
	s.UnreclaimedEndedBytes = max(0, sc.NoAppendBytes-sc.NoAppendOverBytes-liveAll)
	for t, ss := range s.Shares {
		ss.UsedBytes = used[t]
		ss.HeldBytes, ss.DirtyBytes = figs[t].Held, figs[t].Dirty
		ss.ReservedBytes = reservedBy[t]
		ss.UnreclaimedBytes = unreclaimed(used[t], over[t], figs[t])
		// The share's half is the record bytes its records hold in the
		// segments no append can reach — the set maintains it beside its
		// own per-tag bytes at each state change, charge and removal. It
		// is the candidate measure a share-scoped pass reads: zero iff no
		// such pass has a segment holding the share's bytes to free, and
		// never the byte figure itself, which the per-share bookkeeping
		// below this loop's shapes would need live counters per open
		// segment per tag. positive answers the retry-later the
		// journal-wide figure's byte answer stays for.
		ss.UnreclaimedEndedBytes = c.set.NoAppendBytesOf(t)
		s.UnreclaimedBytes += ss.UnreclaimedBytes
		s.Shares[t] = ss
	}
	s.ReservedBytes = reserved
	return s
}

// unreclaimed is the record bytes of a share that back no held or retained
// extent and no live record: its record bytes less its held and retained
// payload and the non-payload bytes of its records, never below zero.
func unreclaimed(tagBytes, overhead int64, f shareHeld) int64 {
	return max(0, unreclaimedRaw(tagBytes, overhead, f))
}

// unreclaimedRaw is the figure before the clamp.
func unreclaimedRaw(tagBytes, overhead int64, f shareHeld) int64 {
	return tagBytes - f.Held - f.Retained - overhead
}
