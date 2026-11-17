package recovery

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Assembled is what the assembly phase builds over what the earlier phases
// collected: the segment ledger seeded with every segment the rebuild
// scanned — each at its accounted size, its classed state and the record
// bytes the feed charged — and the liveness registrations of everything
// still live on disk, the hand the open makes of its rebuilt state to the
// journal the core opens from it. The phase is a pure transform over the
// State: no storage call and no lock beyond the seed's own.
type Assembled struct {
	// Set is the seeded ledger: one adopted entry per scanned segment, the
	// counters and per-tag figures moving as the running journal's own
	// event paths moved them. Nil only where the phase never ran.
	Set *segment.Set

	// Live holds every attached header-only record the rebuild fed or
	// withheld — a release, truncate, deallocate, delete, clone target,
	// offloaded or unmark record — at the placement its own slot spells,
	// one entry per copy, and each synced loss record beside them the same
	// way the journal's own append path registers it: plain, with no paired
	// holder set, for its marker's settle decides its liveness.
	Live []reclaim.Rec

	// Forgets holds one entry per attached forget record, with the snapshot
	// of the segments the feed collected holding records of its share tag
	// below it: the registration a forget record's liveness runs against.
	Forgets []reclaim.ForgetSet

	// Unsynced holds one entry per attached unsynced loss record, paired
	// with the segments still holding copies of the record it names: the
	// presence marks that keep the loss answering live while any holder
	// remains, beside the plain registration its own record takes.
	Unsynced []reclaim.UnsyncedLoss
}

// runAssemble is the assembly phase of the open, over the state the rebuild
// and the kinds pass left: it raises the counters above every number the
// disk holds that the rebuild's feed never saw — the numbers the feed
// withheld, the aside records' own, the corrupt drops' — and above every
// number on the failed list, one the disk holds or not, never lowering one.
// It seeds the segment ledger with one adopted entry per scanned segment,
// the §7.3 accounted size over the scan's own facts: the cap for an active
// or ended segment, and the post-seal size of a sealed one, a size past the
// cap deps.SegmentCap names being a shape the adoption refuses. And it
// spells the registration lists the journal's open hands its liveness
// tracker, each record keyed at the placement and with the tag, file and
// numbers its own header says, paired with the holder facts the feed
// collected.
//
// The fates decide a segment's classed state: a torn tail seeds it Active —
// the one state a seal takes, for the later phase cuts the segment's torn end
// and seals it, the segment accounted at its cap until the cut — a fate the
// classing ended seeds it Ended, a scan the seal marker closed seeds it
// Sealed, and every other scanned segment — fate or no fate — seeds it
// Active, an empty own segment accounted at its cap beside the loaded ones.
//
// A read-only journal's failed list is raised above whatever else the disk
// holds: a number the feed already saw keeps its counter where it was, and
// one nothing on disk carries still raises the sequence counter, so the
// first number the next open issues stays above the record whose loss the
// read-only one failed to write.
func runAssemble(_ context.Context, deps Deps, _ *reader, st *State, _ core.JournalID, _ core.Version, _ []core.SyncMark) error {
	ctr := st.Rebuilt.Counters
	for _, seq := range deps.Failed {
		if err := ctr.Observe(index.Numbers{Seq: seq}); err != nil {
			return err
		}
	}
	// The withheld fold the feed collected covers the content records its
	// rules withheld whole or confined; the aside headers and the corrupt
	// drops beside it hold numbers the feed never gave the builder, and the
	// counters must open above every one of them on both axes: a record
	// whose number is issued again would be outranked by its own earlier
	// self wherever it lands.
	if err := ctr.Observe(st.Kinds.Withheld); err != nil {
		return err
	}
	for i := range st.Rebuilt.Scans {
		for _, a := range st.Rebuilt.Scans[i].Aside {
			if err := ctr.Observe(index.Numbers{Version: a.Header.Version, Seq: a.Header.Seq}); err != nil {
				return err
			}
		}
		for _, d := range st.Rebuilt.Scans[i].Dropped {
			if err := ctr.Observe(index.Numbers{Version: d.Header.Version, Seq: d.Header.Seq}); err != nil {
				return err
			}
		}
	}

	// The seed: every segment the rebuild scanned owns an entry, for every
	// scanned segment is a segment of the journal, records or not, and its
	// bytes are accounted until it is unlinked. The ids ascend, so a
	// refusal names the lowest segment the scan could not adopt.
	ended := make(map[core.SegmentID]bool, len(st.Tails.Fates))
	for _, f := range st.Tails.Fates {
		ended[f.ID] = f.Kind == fateEnded
	}
	set := segment.NewSet(deps.SegmentCap, deps.Log)
	scans := slices.Clone(st.Rebuilt.Scans)
	slices.SortFunc(scans, func(a, b segmentScan) int { return cmp.Compare(a.ID, b.ID) })
	for i := range scans {
		sc := &scans[i]
		state, size, full := segment.Active, deps.SegmentCap, false
		switch {
		case sc.Sealed:
			state, size = segment.Sealed, sc.Size
			// A full seal puts its trailer in the cap's last 32 bytes, and
			// only the catalogued footer that verified under it names that
			// shape: an early seal whose truncation failed keeps its trailer
			// below the cap and a free tail on top, bytes the catalog read
			// cannot verify, so the walk the demotion takes answers no full
			// word whatever the file's size.
			//
			// decision: a full word is kept from every walk-scanned seal at
			// the cap — a full seal a damaged or torn footer demoted, or one
			// no successor names and whose footer is therefore never read,
			// among them — so a recycle the mark alone admits is missed and
			// never taken wrongly; and an early seal's marker ending exactly
			// at the cap's trailer, the one shape no bytes on disk can tell
			// from a full seal, takes the full word with it, for the segment
			// it names is closed at its cap with no free tail to cut, the
			// shape a recycle reuses as it stands. Overturned by a fact that
			// separates the two shapes on the scan's own answer.
			full = sc.Size == deps.SegmentCap && sc.SealedByCatalog
		case ended[sc.ID]:
			state = segment.Ended
		}
		tags, over := tagCharges(st.Kinds.Charging[sc.ID])
		if err := set.AdoptRebuilt(sc.ID, size, state, full, tags, over); err != nil {
			return fmt.Errorf("the rebuilt seed of segment %d: %w", sc.ID, err)
		}
	}

	a := Assembled{Set: set}
	a.Live = slices.Clone(st.Kinds.Records)
	for _, rec := range st.Kinds.LossRecs {
		if rec.Synced {
			a.Live = append(a.Live, rec)
			continue
		}
		a.Unsynced = append(a.Unsynced, reclaim.UnsyncedLoss{Rec: rec, HeldBy: holdersOf(st.Kinds.Losses, rec)})
	}
	for _, rec := range st.Kinds.ForgetRecs {
		// decision: every forget record of a tag registers against the
		// tag's one collected snapshot, the greatest forget's holder set —
		// not the snapshot its own append took, which the rebuild never
		// collected (the kinds pass keeps one forget per tag). An earlier
		// forget of the same tag therefore over-holds: its re-check set
		// includes segments holding records of the tag above it, so the
		// record answers live until more segments retire than its own
		// snapshot would have kept it for — the safe direction, a forget
		// kept too long. Overturned by a rebuild collecting one snapshot
		// per forget record.
		f := reclaim.ForgetSet{Rec: rec}
		for _, fo := range st.Kinds.Forgets {
			if fo.Tag == rec.Tag {
				f.Segs = slices.Clone(fo.Segments)
				break
			}
		}
		a.Forgets = append(a.Forgets, f)
	}
	st.Assembled = a
	return nil
}

// tagCharges turns one segment's collected (segment, tag) charge table into
// the per-tag record bytes and overhead maps the ledger's seed takes: the
// whole sizes the feed charged and the same less the payloads. A segment
// nothing charged — an own segment holding no record — answers nil, which
// the seed adopts at its accounted size alone.
func tagCharges(m map[core.ShareTag]Charge) (tags, over map[core.ShareTag]int64) {
	if len(m) == 0 {
		return nil, nil
	}
	tags = make(map[core.ShareTag]int64, len(m))
	over = make(map[core.ShareTag]int64, len(m))
	for tag, ch := range m {
		tags[tag] = ch.Bytes
		over[tag] = ch.Overhead
	}
	return tags, over
}

// holdersOf answers the segments the feed collected holding copies of the
// record the unsynced loss rec names, from the losses the kinds pass
// delivered: the pieces a carried-forward loss shares deliver once, so every
// copy of the record the same piece names finds the one holder list. An
// effective loss always finds its piece there — its first copy delivered it
// — so the nil answer names a shape the pass's own rules never produce; it
// holds the loss to its outranking guard alone, the safe direction.
func holdersOf(losses []OpenLoss, rec reclaim.Rec) []core.SegmentID {
	for _, lo := range losses {
		if lo.Dropped == rec.Named && lo.Ev.Extent == rec.Ext {
			return lo.Segments
		}
	}
	return nil
}
