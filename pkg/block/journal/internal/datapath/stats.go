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
	// take no further append; zero, like UnreclaimedBytes, until Repack seals.
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
	DropRanges, Segments, Sealed, Spares                      int
	UnreclaimedBytes, HeadroomBytes, RepackReserveBytes       int64
	PinnedSegments, OpenDescriptors, DescriptorLimit          int
	DamagedSegments                                           int
	LastSyncError                                             *durability.SyncFailure
	SyncDuration                                              durability.Histogram
	RemovalMarkers                                            int
	VersionLimit                                              core.Version
	VersionsLeft                                              uint64
	NextVersion                                               core.Version
	ExtentLimit                                               int64
	Counters                                                  Counters
	Losses                                                    []core.LossEvent
	LossGeneration                                            uint64
	TagConflicts                                              []index.Conflict
	Shares                                                    map[core.ShareTag]ShareStats
	ReadOnly                                                  core.ReadOnlyState
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
// the record overhead. Held and retained are read first, then the set's bytes
// and overhead together in one lock hold. A write in flight inflates the
// figure by its payload, the set being charged before held rises; that is not
// negative. The clamp's remaining job is a release followed by a retire: held
// falls at publish and the set falls later, at the retire, so a reader that
// read held first can see the set below it once retirement is wired. Nothing
// retires a segment the index still holds yet, so production cannot reach
// that case; the clamp is the guard for when it can.
//
// ponytail: the figure ignores footer entries, segment headers and trailers,
// which the set does not count, and a dead record's header and padding stay
// counted as backed until its segment retires. Ceiling: about one record
// header plus padding per dead record. Upgrade to a per-record backed-bytes
// refcount in the index, which costs index memory against the bound of RFC 1
// §4.2, once that error shows in a capacity decision.
func (c *Core) Stats() Stats {
	var s Stats
	s.MaxBytes = c.maxBytes
	reserved, reservedBy := c.cap.ReservedBytes(), c.cap.ReservedByTag()
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
	s.Spares, s.HeadroomBytes, s.RepackReserveBytes = int(pc.Spares), pc.HeadroomBytes, pc.RepackReserveBytes
	ds := c.cache.Stats()
	s.OpenDescriptors, s.DescriptorLimit = ds.Open, ds.Limit

	s.VersionLimit, s.VersionsLeft, s.NextVersion = c.counters.Limit(), c.counters.VersionsLeft(), c.counters.NextVersion()

	d := c.dur.Stats()
	s.DamagedSegments, s.LastSyncError, s.SyncDuration = int(d.DamagedSegments), d.LastSyncError, d.SyncDuration
	s.Losses, s.LossGeneration, s.ReadOnly = d.Losses, d.LossGeneration, d.ReadOnly

	s.Counters = Counters{
		WrittenBytes: c.WrittenBytes(), FilledBytes: c.FilledBytes(), FillRefused: c.FillRefused(),
		FreedBytes:         c.retirer.Counters().FreedBytes,
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
	figs := c.shareFigures()
	used, over := c.set.TagFigures()
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
	for t, ss := range s.Shares {
		ss.UsedBytes = used[t]
		ss.HeldBytes, ss.DirtyBytes = figs[t].Held, figs[t].Dirty
		ss.ReservedBytes = reservedBy[t]
		ss.UnreclaimedBytes = unreclaimed(used[t], over[t], figs[t])
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
