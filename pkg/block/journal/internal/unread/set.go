// Package unread holds the journal's records whose bytes no read attempt
// returned, and the segments they make suspect.
package unread

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
)

// Kind distinguishes a record's body from a header the journal can hold
// alone: a body waits for its block, while a header-only record — a removal,
// an offloaded one or a loss — never had bytes to read.
type Kind uint8

const (
	// Content is a record body.
	Content Kind = iota
	// HeaderOnly is a record header without its body.
	HeaderOnly
)

// Ref names one unread range of one segment, and the kind of record it holds.
// Its identity is (Segment, Off): marking and clearing find the same entry
// regardless of the Kind or Len a later pass carries.
type Ref struct {
	core.SegmentRange
	Kind Kind
}

// Set holds the journal's records whose bytes no read attempt returned,
// keyed by the segment and file offset each record's range starts at.
//
// ponytail: in-memory only, so a restart forgets what was unread and the
// open meets it again by reading; one map under one mutex, O(1) per access.
// Upgrade to a sharded map if a profile shows the mutex once ReadAt calls
// it, and to state held across a restart once unread records are held at
// open.
//
// ponytail: Try marks a range after its read returns, so a mark landing
// after Retired(seg) of the same segment re-creates the entry and leaves
// the segment suspect for good, and the same ordering leaves a stale mark
// after a concurrent success: two Trys on a marked range, one reading bytes
// and clearing it while the other still in its read fails and marks it
// again, leave the range unread and its segment suspect after a read
// returned bytes, until the next access clears it. The set relies on its
// caller never retiring a segment while a read of it is in flight and on
// the next access following a concurrent failure. Upgrade to a mark
// conditional on the segment still being live and on nothing having
// cleared the range since the read started, once a caller exists that
// retires or races on one.
type Set struct {
	mu      sync.Mutex
	entries map[core.SegmentID]map[int64]Ref
	suspect int
	clk     core.Clock
}

// New returns an empty set reading the journal's clock.
func New(clk core.Clock) *Set {
	return &Set{entries: map[core.SegmentID]map[int64]Ref{}, clk: clk}
}

// Try reads ref's bytes through the bound's retries and keeps the custody
// the set exists to hold: a range no attempt reads is marked unread, and a
// range that reads is cleared, so the next access of it goes to the block
// store alone.
//
// A range already unread gets one attempt only — it has failed once, so
// nothing but the one quick try is spent asking the device again — while a
// range no read has failed yet gets readretry.Attempts of them: a fresh
// failure may be the transient one the bound was written for. Both run
// inside the bound's window, on the journal's clock.
//
// The set keeps its record even when a context ends the retries: a range
// whose bytes no attempt read is unread whatever ended them, so one whose
// read failed before the context did is marked — while nothing is marked of
// a context that ended before any attempt started, since no read ever
// failed. The context's error is returned when it is what ended the
// retries, bare, so the caller answers at its deadline with its own
// cancellation; a context ending during the last permitted attempt returns
// that attempt's failure, wrapped in ErrUnreadable, as the bound does: the
// attempt, not the context, was what spent it. A read ending in nil has
// said everything — bytes or none — and the range is cleared whatever it
// was before.
func (s *Set) Try(ctx context.Context, ref Ref, read func() ([]byte, error)) ([]byte, error) {
	attempts := readretry.Attempts
	if s.Unread(ref) {
		attempts = 1
	}
	var b []byte
	tried, err := readretry.Do(ctx, s.clk, attempts, func() (err error) {
		b, err = read()
		return err
	})
	switch {
	case err == nil:
		s.clear(ref)
		return b, nil
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		if tried > 0 {
			s.mark(ref)
		}
		return nil, err
	default:
		s.mark(ref)
		return nil, fmt.Errorf("%w: segment %d offset %d: %w",
			core.ErrUnreadable, ref.Segment, ref.Off, err)
	}
}

// Unread reports whether the range starting at ref.Off of ref.Segment is one
// the open met unread.
func (s *Set) Unread(ref Ref) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[ref.Segment][ref.Off]
	return ok
}

// Suspect reports whether seg holds any unread range, so a caller writing a
// fresh copy of it must expect torn or missing bodies.
func (s *Set) Suspect(seg core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries[seg]) > 0
}

// Ranges returns seg's unread ranges as SegmentRange values, sorted by file
// offset. The slice is a copy: mutating or appending to it leaves the set
// unchanged.
func (s *Set) Ranges(seg core.SegmentID) []core.SegmentRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	ranges := make([]core.SegmentRange, 0, len(s.entries[seg]))
	for _, ref := range s.entries[seg] {
		ranges = append(ranges, ref.SegmentRange)
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Off < ranges[j].Off })
	return ranges
}

// Retired drops every unread range of seg: the segment is gone from the
// node, so none of its unread remains. Retiring a segment the set never saw
// changes nothing.
func (s *Set) Retired(seg core.SegmentID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries[seg]) == 0 {
		return
	}
	delete(s.entries, seg)
	s.suspect--
}

// SuspectSegments returns how many distinct segments hold unread ranges.
// SuspectSegments is a maintained value, never a walk: a locked int read
// allocates nothing.
func (s *Set) SuspectSegments() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspect
}

// mark records ref as unread. Marking a range twice counts it once.
func (s *Set) mark(ref Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[ref.Segment][ref.Off]; ok {
		return
	}
	if s.entries[ref.Segment] == nil {
		s.entries[ref.Segment] = map[int64]Ref{}
		s.suspect++
	}
	s.entries[ref.Segment][ref.Off] = ref
}

// clear drops the range starting at ref.Off of ref.Segment, if it is unread.
func (s *Set) clear(ref Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offs, ok := s.entries[ref.Segment]
	if !ok {
		return
	}
	delete(offs, ref.Off)
	if len(offs) == 0 {
		delete(s.entries, ref.Segment)
		s.suspect--
	}
}
