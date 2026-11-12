package enginetest

import (
	"context"
	"io/fs"
	"slices"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// The fake's figures that RFC 1 §3.7 reports and a test cannot set.
const (
	fakeHeadroom    = 1 << 20
	fakeExtentLimit = 1 << 20
)

// fseg is one segment of the fake's opt-in model (see SegmentCap).
type fseg struct {
	id         journal.SegmentID
	stream     uint32
	sealed     bool // sealed full or early: takes no further append
	capSealed  bool // sealed early by a capacity pass, until repack retires it
	failed     bool // ended by a failed sync or append
	unresolved bool // a failed segment whose window has not resolved
	retired    bool
	selected   bool // a running pass has selected it
	alloc      int64
	fill       int64
	rec        map[journal.ShareTag]int64 // record bytes on disk, live or not
	live       map[journal.ShareTag]int64
	unrec      map[journal.ShareTag]int64
}

// ended reports whether the segment takes no further append.
func (s *fseg) ended() bool { return s.sealed || s.failed }

// segModel is the fake's segments, opt-in: with none, the fake keeps its one
// stream of records and counts held bytes only.
type segModel struct {
	cap        int64
	max        int64
	hold       bool // the sync timer is held: only a Sync makes superseded bytes unreclaimed
	deviceFull bool
	streamOf   map[journal.FileID]uint32
	active     map[uint32]*fseg
	segs       []*fseg
	next       uint64
	freed      uint64
	atSelected func(ctx context.Context) // runs inside Repack after it selected, before it retires
}

// SegmentCap turns the segment model on: segments of cap bytes, and a journal
// maximum of max bytes allocated (zero: none). With it on, a share's limit
// counts the record bytes of its records still on disk, as RFC 1 §3.7's
// UsedBytes does, and a record goes to its file's stream's active segment,
// which rolls when the record does not fit.
func (j *FakeJournal) SegmentCap(capBytes, max int64) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sm = &segModel{
		cap:      capBytes,
		max:      max,
		streamOf: map[journal.FileID]uint32{},
		active:   map[uint32]*fseg{},
	}
}

// StreamOf puts the records of file id on stream s.
func (j *FakeJournal) StreamOf(id journal.FileID, s uint32) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sm.streamOf[id] = s
}

// HoldSyncTimer models the journal's sync timer held: a superseded byte is
// unreclaimed only from the next Sync naming its file.
func (j *FakeJournal) HoldSyncTimer(on bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sm.hold = on
}

// DeviceFull makes every roll refuse with ENOSPC while on.
func (j *FakeJournal) DeviceFull(on bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sm.deviceFull = on
}

// AtRepackSelected arms f to run, on the instance, inside every Repack after
// it selected its segments and before it retires them, with the fake's lock
// released, so a delay wrapper holds a pass mid-way; nil disarms it.
func (j *FakeJournal) AtRepackSelected(f func(ctx context.Context)) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.sm.atSelected = f
}

// AddEnded adds a sealed segment holding n unreclaimed bytes of tag.
func (j *FakeJournal) AddEnded(tag journal.ShareTag, n int64) {
	j.mu.Lock()
	defer j.mu.Unlock()

	s := j.newSeg(0)
	s.sealed = true
	s.rec[tag], s.unrec[tag] = n, n
	s.fill = n
}

// AddFailed adds a segment a failed sync ended, holding n unreclaimed bytes
// of tag; unresolved leaves its window open, which keeps repack off it.
func (j *FakeJournal) AddFailed(tag journal.ShareTag, n int64, unresolved bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	s := j.newSeg(0)
	s.failed, s.unresolved = true, unresolved
	s.rec[tag], s.unrec[tag] = n, n
	s.fill = n
}

// ResolveWindows resolves every failed segment's window.
func (j *FakeJournal) ResolveWindows() {
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, s := range j.sm.segs {
		s.unresolved = false
	}
}

// newSeg allocates a segment on stream. j.mu is held.
func (j *FakeJournal) newSeg(stream uint32) *fseg {
	m := j.sm
	m.next++

	s := &fseg{
		id:     journal.SegmentID(m.next),
		stream: stream,
		alloc:  m.cap,
		rec:    map[journal.ShareTag]int64{},
		live:   map[journal.ShareTag]int64{},
		unrec:  map[journal.ShareTag]int64{},
	}
	m.segs = append(m.segs, s)

	return s
}

// allocated is the bytes the segments not retired allocate. j.mu is held.
func (j *FakeJournal) allocated() int64 {
	var n int64

	for _, s := range j.sm.segs {
		if !s.retired {
			n += s.alloc
		}
	}

	return n
}

// onDisk is the record bytes of tag still on disk. j.mu is held.
func (j *FakeJournal) onDisk(tag journal.ShareTag) int64 {
	var n int64

	for _, s := range j.sm.segs {
		if !s.retired {
			n += s.rec[tag]
		}
	}

	return n
}

// place finds the segment for a record of n bytes of f, refusing as the
// journal does: a share's limit, then a roll past the maximum or on a full
// device. j.mu is held.
func (j *FakeJournal) place(h *FakeHandle, id journal.FileID, n int64) (*fseg, error) {
	m := j.sm

	if h.limit > 0 && j.onDisk(h.tag)+n > h.limit {
		return nil, &journal.NoSpaceError{
			Limit:   journal.LimitShare,
			Missing: j.onDisk(h.tag) + n - h.limit,
			Unit:    journal.UnitBytes,
		}
	}

	stream := m.streamOf[id]

	s := m.active[stream]
	if s != nil && s.fill+n <= m.cap {
		return s, nil
	}

	if m.deviceFull {
		return nil, &fs.PathError{Op: "write", Path: "segment", Err: syscall.ENOSPC}
	}

	if m.max > 0 && j.allocated()+m.cap > m.max {
		return nil, &journal.NoSpaceError{
			Limit:   journal.LimitJournal,
			Missing: j.allocated() + m.cap - m.max,
			Unit:    journal.UnitBytes,
		}
	}

	if s != nil {
		s.sealed = true
	}

	s = j.newSeg(stream)
	m.active[stream] = s

	return s, nil
}

// supersede counts n bytes of f's record in seg unreclaimed, at once or, with
// the sync timer held, at the file's next Sync. j.mu is held.
func (j *FakeJournal) supersede(f *fakeFile, s *fseg, n int64) {
	if s == nil || n == 0 {
		return
	}

	s.live[f.tag] -= n

	if j.sm.hold {
		f.pending = append(f.pending, superseded{s, n})

		return
	}

	s.unrec[f.tag] += n
}

// superseded is bytes whose superseding record is not synced yet.
type superseded struct {
	seg *fseg
	n   int64
}

// flush makes f's superseded bytes unreclaimed: its records are synced.
func (f *fakeFile) flush() {
	for _, p := range f.pending {
		p.seg.unrec[f.tag] += p.n
	}

	f.pending = nil
}

// segStats fills s with the segment model's figures. j.mu is held.
func (j *FakeJournal) segStats(s *journal.Stats) {
	m := j.sm
	s.MaxBytes, s.UsedBytes = m.max, j.allocated()
	s.HeadroomBytes, s.RepackReserveBytes = fakeHeadroom, m.cap
	s.ExtentLimit = fakeExtentLimit
	s.Counters.FreedBytes = m.freed

	for _, f := range j.files {
		s.ExtentCount += int64(len(f.pieces))
	}

	shares := map[journal.ShareTag]journal.ShareStats{}

	for tag, st := range s.Shares {
		st.UsedBytes = 0 // the record bytes on disk are the segments', below
		shares[tag] = st
	}

	for _, seg := range m.segs {
		if seg.retired {
			continue
		}

		for tag, n := range seg.rec {
			st := shares[tag]
			st.UsedBytes += n
			shares[tag] = st
		}

		for tag, n := range seg.unrec {
			st := shares[tag]
			st.UnreclaimedBytes += n
			s.UnreclaimedBytes += n

			if seg.ended() {
				st.UnreclaimedEndedBytes += n
				s.UnreclaimedEndedBytes += n
			}

			shares[tag] = st
		}
	}

	for _, h := range j.handles {
		st := shares[h.tag]
		st.Limit = h.limit
		shares[h.tag] = st
	}

	s.UsedBytes = j.allocated()
	s.Shares = shares
}

// Repack seals, selects and retires as RFC 1 §7.2 words it. A pass whose scope
// names no segments first seals each active segment holding unreclaimed bytes
// of the scope, at most one per stream, and none on a stream with a segment
// sealed that way that no repack has retired; then it takes the ended segments
// of the scope, no other pass has selected and no unresolved window holds,
// fewest live bytes first, while the bytes it copies stay within budget, copies
// their live bytes into the stream's active segment and retires them. It
// answers the storage the retirements returned. Without the segment model it
// answers journal.ErrNotImplemented, as the journal does.
func (j *FakeJournal) Repack(
	ctx context.Context,
	budget int64,
	scope journal.RepackScope,
) (int64, error) {
	j.mu.Lock()

	if j.sm == nil {
		j.mu.Unlock()

		return 0, journal.ErrNotImplemented
	}

	if j.closed {
		j.mu.Unlock()

		return 0, journal.ErrClosed
	}

	m := j.sm

	if len(scope.Segments) == 0 {
		j.sealForCapacity(scope.Share)
	}

	var picked []*fseg

	var copied int64

	cands := slices.Clone(m.segs)
	slices.SortFunc(cands, func(a, b *fseg) int {
		return int(sum(a.live) - sum(b.live))
	})

	for _, s := range cands {
		if !j.candidate(s, scope) || copied+sum(s.live) > budget {
			continue
		}

		s.selected = true
		copied += sum(s.live)
		picked = append(picked, s)
	}

	hook := m.atSelected

	j.mu.Unlock()

	if hook != nil {
		hook(ctx)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if err := ctx.Err(); err != nil {
		for _, s := range picked {
			s.selected = false
		}

		return 0, err
	}

	var freed int64

	for _, s := range picked {
		freed += j.retire(s)
	}

	return freed, nil
}

// sum is the total of a per-tag figure.
func sum(m map[journal.ShareTag]int64) int64 {
	var n int64

	for _, v := range m {
		n += v
	}

	return n
}

// candidate reports whether repack may select s for scope. j.mu is held.
func (j *FakeJournal) candidate(s *fseg, scope journal.RepackScope) bool {
	switch {
	case s.retired, s.selected, !s.ended(), s.failed && s.unresolved, sum(s.unrec) == 0:
		return false
	case len(scope.Segments) > 0:
		return slices.Contains(scope.Segments, s.id)
	case scope.Share != 0:
		return s.rec[scope.Share] > 0
	}

	return true
}

// sealForCapacity seals, for tag (zero: any), each active segment holding its
// unreclaimed bytes, at most one per stream. j.mu is held.
func (j *FakeJournal) sealForCapacity(tag journal.ShareTag) {
	m := j.sm

	for stream, s := range m.active {
		if j.awaitingRepack(stream) {
			continue
		}

		held := sum(s.unrec)
		if tag != 0 {
			held = s.unrec[tag]
		}

		if held == 0 {
			continue
		}

		s.sealed, s.capSealed = true, true
		s.alloc = s.fill // an early seal returns the free tail

		delete(m.active, stream)
	}
}

// awaitingRepack reports whether stream has a capacity-sealed segment repack
// has not retired. j.mu is held.
func (j *FakeJournal) awaitingRepack(stream uint32) bool {
	for _, s := range j.sm.segs {
		if s.stream == stream && s.capSealed && !s.retired {
			return true
		}
	}

	return false
}

// retire copies s's live bytes into its stream's active segment, retires it
// and answers the storage it returned. j.mu is held.
func (j *FakeJournal) retire(s *fseg) int64 {
	m := j.sm

	into := m.active[s.stream]
	if into == nil {
		into = j.newSeg(s.stream)
		m.active[s.stream] = into
	}

	for _, f := range j.files {
		for i := range f.pieces {
			p := &f.pieces[i]
			if p.seg != s {
				continue
			}

			n := int64(len(p.b))
			into.rec[f.tag] += n
			into.live[f.tag] += n
			into.fill += n
			p.seg = into
		}
	}

	freed := s.alloc
	s.retired, s.selected = true, false
	m.freed += uint64(freed)

	return freed
}
