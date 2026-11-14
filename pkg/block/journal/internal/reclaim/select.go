package reclaim

import (
	"cmp"
	"math"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
)

// Scope narrows a selection. The zero Scope selects from every segment; a
// Share keeps the segments holding bytes of that share, Segments keeps the
// listed ids only, and both together must both hold.
type Scope struct {
	Share    core.ShareTag
	Segments []core.SegmentID
}

// Candidate is one segment a selection offers for reclamation. Live is the
// bytes of it the index still backs, held and retained.
type Candidate struct {
	ID   core.SegmentID
	Live int64
	// ponytail: the per-segment unreclaimed figure stays internal until the
	// spec names it for export; it is the segment's size less its live bytes,
	// floored at zero.
	Unreclaimed int64
}

// Durability is what a selection asks of the durability service.
type Durability interface {
	WindowOpen(core.SegmentID) bool
	ReclaimBlocked(core.SegmentID) bool
}

// Pins is the set of segments something outside the selector holds back. A
// nil *Pins pins nothing. It is safe for concurrent use.
type Pins struct {
	// decision: mu is unranked: a Pin asks, with mu held, whether the
	// selector already claimed the segment, and the answer walks the
	// selector's own mutex and the capacity lock that Selected reads
	// under — so two locks are taken under this one. The order holds
	// because neither of them asks the pins back. Withdraw the unranking
	// if a selector path ever pins with its own lock held.
	mu   sync.Mutex
	segs map[core.SegmentID]struct{}
	// selected asks the selector this set was given to whether a segment is
	// selected; nil until NewSelector wires it.
	selected func(core.SegmentID) bool
}

// NewPins returns an empty set of pins.
func NewPins() *Pins { return &Pins{segs: map[core.SegmentID]struct{}{}} }

// Pin holds seg back from selection until Unpin. It is refused, returning
// false, for a segment a pass has selected: that segment takes no new pin
// until the pass ends with it. A nil *Pins holds nothing, so it refuses too.
func (p *Pins) Pin(seg core.SegmentID) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.selected != nil && p.selected(seg) {
		return false
	}
	p.segs[seg] = struct{}{}
	return true
}

// Unpin lets seg be selected again; unpinning a segment not pinned changes
// nothing.
func (p *Pins) Unpin(seg core.SegmentID) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.segs, seg)
}

// Pinned reports whether seg is pinned.
func (p *Pins) Pinned(seg core.SegmentID) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.segs[seg]
	return ok
}

// Selector picks the segments worth reclaiming.
type Selector struct {
	segs *segment.Set
	ix   *index.Index
	un   *unread.Set
	dur  Durability
	pins *Pins

	// mu guards mine, the segments this selector claimed and has not been
	// Done with. The set's own claim flag cannot say it: a retirement's
	// claim shows there too.
	//
	// decision: mu is a leaf: it guards the claim book's map work alone and
	// takes nothing while held, so the set is asked only after it is
	// released.
	mu   sync.Mutex
	mine map[core.SegmentID]struct{}

	// afterSnapshot runs between the ledger snapshot and the claims; set only
	// from tests.
	afterSnapshot func()

	// afterClaim runs in claim once the claim is recorded and before the pin
	// is read; set only from tests.
	afterClaim func(core.SegmentID)
}

// NewSelector returns a selector over the given facts; pins may be nil. A
// nil dur disables the window and damage checks and exists for tests only;
// production passes one. A journal has one selector, safe for concurrent
// Select calls: it wires itself into pins, so NewSelector panics when pins is
// already wired to another selector, which would otherwise be unwired
// silently and leave a pin free to land on a segment it holds. It panics on a
// nil segs, ix or un.
func NewSelector(segs *segment.Set, ix *index.Index, un *unread.Set, dur Durability, pins *Pins) *Selector {
	if segs == nil || ix == nil || un == nil {
		panic("reclaim: NewSelector needs a segment set, an index and an unread set")
	}
	s := &Selector{segs: segs, ix: ix, un: un, dur: dur, pins: pins, mine: map[core.SegmentID]struct{}{}}
	if pins != nil {
		pins.mu.Lock()
		if pins.selected != nil {
			pins.mu.Unlock()
			panic("reclaim: Pins is already wired to a selector")
		}
		pins.selected = s.Selected
		pins.mu.Unlock()
	}
	return s
}

// Selected reports whether this selector holds a claim on seg: true from the
// claim Select took until Done, or until the segment is retired.
func (s *Selector) Selected(seg core.SegmentID) bool {
	s.mu.Lock()
	_, ok := s.mine[seg]
	s.mu.Unlock()
	if !ok {
		return false
	}
	_, err := s.segs.State(seg) // a retired segment's claim left with it
	return err == nil
}

// Done ends a pass with ids: it releases the claim Select took on each one the
// pass did not retire. An id this selector did not claim, or that is retired,
// changes nothing. The caller passes every id it was handed, including those
// it retired, so the selector's bookkeeping empties.
//
// A pass that panics never releases its claims, which carry no lease: the pass
// must `defer sel.Done(ids...)`.
//
// decision: Done is the only release. A retirement of a selected segment
// cannot take the claim over, because the set's claim flag is shared and
// ownerless, so the caller must call Done before retiring; in that gap Selected
// is false, so a pin or another claimant can win the segment. The hand-over of
// a held claim to the retirement is owed by the pass that retires. Withdraw
// this once a retirement can take over a claim Select holds.
func (s *Selector) Done(ids ...core.SegmentID) {
	for _, id := range ids {
		s.mu.Lock()
		_, ok := s.mine[id]
		delete(s.mine, id)
		s.mu.Unlock()
		if ok {
			s.segs.Release(id)
		}
	}
}

// claim takes the segment's claim for this selector, refusing when it is gone,
// held elsewhere, or pinned. The pin is read after the claim: Pin reads the
// claim under the pins' lock, so a pin and a claim racing cannot both stand.
func (s *Selector) claim(id core.SegmentID) bool {
	if !s.segs.Claim(id) {
		return false
	}
	s.mu.Lock()
	s.mine[id] = struct{}{}
	s.mu.Unlock()
	if s.afterClaim != nil {
		s.afterClaim(id)
	}
	if s.pins.Pinned(id) {
		s.Done(id)
		return false
	}
	return true
}

// Select returns up to limit candidates inside scope, the least live first and
// the lowest id first among equals, and claims each one it returns: a segment
// a concurrent pass holds, or that retired since the listing, is left out. It
// reads the ledger and the index's counters only, never a segment's storage.
// The caller ends the pass with Done.
//
// A negative limit means no limit.
//
// decision: the window, damage and unread checks are read before the claim and
// not re-read after it, so a window can open on a segment after it is selected.
// The pass that carries records owes the re-read under the file index; withdraw
// this once it does.
//
// decision: selection never consults a deferred-loss flag, so a segment holding
// a dropped record whose loss is deferred is an ordinary candidate and its copy
// carries the loss record. Withdraw if candidacy ever has to exclude it.
func (s *Selector) Select(scope Scope, limit int) []Candidate {
	var out []Candidate
	rows := s.segs.Snapshot()
	if s.afterSnapshot != nil {
		s.afterSnapshot()
	}
	for _, r := range rows {
		if r.State != segment.Sealed && r.State != segment.Ended {
			continue
		}
		if s.pins.Pinned(r.ID) {
			continue
		}
		if s.dur != nil && (s.dur.WindowOpen(r.ID) || s.dur.ReclaimBlocked(r.ID)) {
			continue
		}
		if scope.Share != 0 && r.Tags[scope.Share] <= 0 {
			continue
		}
		if len(scope.Segments) > 0 && !slices.Contains(scope.Segments, r.ID) {
			continue
		}
		if s.unreadLive(r.ID) {
			continue
		}
		b := s.ix.SegmentBytes(r.ID)
		live := b.Held + b.Retained
		out = append(out, Candidate{ID: r.ID, Live: live, Unreclaimed: max(0, r.Size-live)})
	}
	// The snapshot is id-ascending, so a stable sort keeps the oldest first
	// among equals.
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(a.Live, b.Live) })
	got := out[:0]
	for _, c := range out {
		if limit >= 0 && len(got) >= limit {
			break
		}
		if s.claim(c.ID) {
			got = append(got, c)
		}
	}
	return got
}

// unreadLive reports whether seg holds an unread record that something still
// needs, so a repack of it would lose that something.
//
// decision: a segment recovery ended is not asked about here. Selection relies
// on open emptying and unlinking every such segment before a pass can see it;
// this tree does not contain that emptying yet. Withdraw this decision, and
// exclude recovery-ended segments here, if a recovered segment can ever be
// Sealed or Ended and visible to Select before the emptying lands.
//
// decision: an unread content record whose file is unknown always pins: with
// no file there is no way to ask the index whether anything names it, so the
// check fails closed. Revisit once every caller that marks a content record
// knows its file.
//
// decision: an unread header-only record always pins. It carries no extent to
// ask the index about, and what it says (a removal, an offloaded mark, a loss)
// may still be the only thing that does. Revisit if the unread set learns what
// a header-only record names.
//
// decision: an unread content record that no file of the segment names, held
// or retained, pins nothing: its bytes back no read and no retained piece, so
// a repack would carry nothing of it. Revisit if snapshot retention lets a
// superseded record be reached without being retained.
func (s *Selector) unreadLive(seg core.SegmentID) bool {
	if !s.un.Suspect(seg) {
		return false
	}
	for _, r := range s.un.Refs(seg) {
		if r.Kind == unread.HeaderOnly {
			return true
		}
		// An offset outside uint32 cannot name an indexed record, which only
		// the unread set's caller can say; treat it as live.
		if r.Off < 0 || r.Off > math.MaxUint32 {
			return true
		}
		if r.File == (core.FileID{}) {
			return true
		}
		if s.ix.Names(r.File, index.RecordAt{Segment: seg, Offset: uint32(r.Off)}) {
			return true
		}
	}
	return false
}
