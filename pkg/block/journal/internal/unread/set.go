// Package unread tracks the record bodies and headers Open met unread while
// attaching segment tails, so a later ReadAt that needs one knows to fetch it
// from the block store rather than the segment file.
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

// Kind distinguishes a record body from its header, which stay unread for
// different reasons: a body waits for its block, a header for a second read
// or a torn tail the open already rejected.
type Kind uint8

const (
	// Content is a record body.
	Content Kind = iota
	// HeaderOnly is a record header without its body.
	HeaderOnly
)

// Ref names one unread range of one segment. Its identity is (Segment, Off):
// marking and clearing find the same entry regardless of the Kind or Len a
// later pass carries.
type Ref struct {
	Segment core.SegmentID
	Off     int64
	Len     int64
	Kind    Kind
}

// Set holds the unread ranges one open met, keyed by the segment and file
// offset each starts at.
//
// ponytail: in-memory only, so a restart forgets what was unread and the
// open meets it again by reading; one map under one mutex, O(1) per access.
// Upgrade to a sharded map if a profile shows the mutex once ReadAt calls
// it, and to state held across a restart once unread records are held at
// open.
//
// ponytail: Try marks a range after its read returns, so a mark landing
// after Retired(seg) of the same segment re-creates the entry and leaves
// the segment suspect for good; the set relies on its caller never retiring
// a segment while a read of it is in flight. Upgrade to a mark conditional
// on the segment still being live once a caller exists that retires one
// concurrently with reads.
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
// The set marks nothing on a context error: the caller ended the retries at
// its own deadline, and the cancellation says nothing about the device. A
// read ending in nil has said everything — bytes or none — and the range is
// cleared whatever it was before.
func (s *Set) Try(ctx context.Context, ref Ref, read func() ([]byte, error)) ([]byte, error) {
	attempts := readretry.Attempts
	if s.Unread(ref) {
		attempts = 1
	}
	var b []byte
	_, err := readretry.Do(ctx, s.clk, attempts, func() (err error) {
		b, err = read()
		return err
	})
	switch {
	case err == nil:
		s.clear(ref)
		return b, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
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
		ranges = append(ranges, core.SegmentRange{Segment: ref.Segment, Off: ref.Off, Len: ref.Len})
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
