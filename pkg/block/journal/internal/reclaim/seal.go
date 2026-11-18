package reclaim

import (
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Sealer seals the active segments a share-scoped pass needs to reach: an
// active segment is no candidate, so the bytes a share holds there stay out
// of any selection until it is sealed. Each stream's open segment that holds
// bytes of the share is sealed early, its marker admitted from the reserved
// headroom, and at most one segment per stream stays sealed this way until it
// is retired.
//
// decision: this is the rule RFC 1 §10 question 5 words, which §7.2 does not
// state yet: a pass scoped to a share seals the scope's active segments, the
// marker drawing on the headroom, and the seals are capped at one per stream
// until repack retires it. It stands on that open question. A §7.2 text that
// differs — another reserve for the marker, or no cap of one per stream —
// replaces it. The cap's own ceiling: a sealed segment the retirement cannot
// take, one a hold keeps or a window pins, keeps its stream from sealing
// another until it goes.
type Sealer struct {
	streams *segment.Streams
	set     *segment.Set
	// admit asks the headroom for the marker, drawn counts the admitted
	// draw once the seal landed and refund returns it when the seal failed.
	// All three may be nil: a sealer over a journal with no headroom to
	// draw.
	admit  func(capacity.RecordClass) (capacity.Draw, error)
	drawn  func(capacity.Draw)
	refund func(capacity.Draw)

	// decision: mu is unranked: it guards the book alone and is held across
	// one read of the set's state, whose lock is a leaf that takes nothing
	// back; the seal's I/O runs outside it, behind the entry the book already
	// carries. Withdraw the unranking if a path under it ever takes a lock
	// that could ask the sealer back.
	mu sync.Mutex
	// sealed is, per stream, the segment this sealer closed and the set has
	// not yet retired.
	sealed map[int]core.SegmentID
}

// NewSealer returns a sealer over streams and the set they share, drawing the
// marker's admission through admit, drawn and refund.
func NewSealer(streams *segment.Streams, set *segment.Set, admit func(capacity.RecordClass) (capacity.Draw, error), drawn, refund func(capacity.Draw)) *Sealer {
	if streams == nil || set == nil {
		panic("reclaim: NewSealer needs its streams and its segment set")
	}
	return &Sealer{streams: streams, set: set, admit: admit, drawn: drawn, refund: refund, sealed: map[int]core.SegmentID{}}
}

// SealShare seals, on each stream that may take one, the open segment holding
// bytes of share, and returns how many it sealed. A stream whose earlier seal
// is still in the set, or whose open segment holds none of the share's bytes,
// seals nothing; a refused admission or a failed seal skips the stream and
// leaves it as it was, for the pass runs on with what it can already select.
func (s *Sealer) SealShare(share core.ShareTag) int {
	rows := s.set.Snapshot()
	n := 0
	for i := range s.streams.Len() {
		st := s.streams.Stream(i)
		id, ok := st.OpenSegment()
		if !ok || !holds(rows, id, share) {
			continue
		}
		if !s.reserve(i, id) {
			continue
		}
		if s.seal(st, i, id) {
			n++
		}
	}
	return n
}

// holds reports whether the row of id holds bytes of share.
func holds(rows []segment.Row, id core.SegmentID, share core.ShareTag) bool {
	for _, r := range rows {
		if r.ID == id {
			return r.Tags[share] > 0
		}
	}
	return false
}

// reserve enters id as stream i's sealed segment, unless the stream's earlier
// one is still in the set. The entry is taken before the seal's I/O, so two
// passes asking at once seal one segment, and dropped again when the seal
// fails.
func (s *Sealer) reserve(i int, id core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.sealed[i]; ok {
		if _, err := s.set.State(prev); err == nil {
			return false
		}
	}
	s.sealed[i] = id
	return true
}

// seal closes id on stream i early, with its marker admitted first.
func (s *Sealer) seal(st *segment.Stream, i int, id core.SegmentID) bool {
	var draw capacity.Draw
	if s.admit != nil {
		d, err := s.admit(capacity.Seal)
		if err != nil || !d.Append {
			s.release(i, id)
			return false
		}
		draw = d
	}
	if _, err := st.Seal(true); err != nil {
		if s.refund != nil {
			s.refund(draw)
		}
		// A segment the failure ended, or another seal closed, stays booked
		// until it goes; one still open was never closed, so its stream may
		// be asked again.
		if state, serr := s.set.State(id); serr != nil || state == segment.Active {
			s.release(i, id)
		}
		return false
	}
	if s.drawn != nil {
		s.drawn(draw)
	}
	return true
}

// release drops stream i's booking of id.
func (s *Sealer) release(i int, id core.SegmentID) {
	s.mu.Lock()
	if s.sealed[i] == id {
		delete(s.sealed, i)
	}
	s.mu.Unlock()
}
