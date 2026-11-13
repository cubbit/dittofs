package durability

import (
	"path/filepath"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// registry holds the per-directory state for the life of the process.
//
// decision: the loss generation lives here, keyed by directory, and not in
// a Durability: a reopen in one process must not hand a client a verifier it
// has seen, and a new Durability on a directory a process already opened
// continues that directory's count rather than starting at zero. The count
// is not persisted, so a restart of the process starts it over; a client
// that sees the verifier change across a restart already treats it as a
// reboot. The directory's loss ring, the count that numbers its events and
// the pieces a Lose or a Place has taken live here with it, for the same
// reason: a reopen neither re-reports a loss record found on disk nor
// restarts the numbering, and keeps the ring history already published.
var registry = struct {
	mu   sync.Mutex
	dirs map[string]*dirState
}{dirs: map[string]*dirState{}}

// dirState is one directory's generation, the sequence numbers whose loss
// record could not be appended, the ring of its published loss events with
// the count that numbers them, and the pieces a Lose or a Place has taken.
// Its mutex is a leaf: nothing is called while it is held.
//
// ponytail: an entry is one small struct per directory ever opened, and none
// is ever freed. Upgrade to freeing it when a directory is forgotten if a
// process ever opens thousands.
type dirState struct {
	mu     sync.Mutex
	gen    uint64
	failed []core.Seq

	// ring holds the directory's published loss events, head the index of the
	// oldest and count how many the ring holds, all under mu.
	ring      [core.LossRingSize]core.LossEvent
	head      int
	count     int
	nextEvent uint64

	// pieces holds every (dropped seq, extent) a Lose or a Place on the
	// directory has taken, so one piece of a dropped record is reported once,
	// while another piece of the same record, which a reconcile can drop
	// alone, is reported on its own.
	//
	// ponytail: it grows by one entry per piece lost for the life of the
	// process and is never pruned, so it follows the number of loss records
	// the directory holds, not the ring. Upgrade to dropping an entry once no
	// copy of its dropped record remains to be reported again, when a
	// directory with a long loss history shows up in a heap profile.
	pieces map[piece]struct{}
}

// stateFor returns the state of dir, made on first use, keyed by its cleaned
// absolute path so two spellings of one directory share it.
func stateFor(dir string) (*dirState, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	key := filepath.Clean(abs)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	st, ok := registry.dirs[key]
	if !ok {
		st = &dirState{pieces: map[piece]struct{}{}}
		registry.dirs[key] = st
	}
	return st, nil
}

// take records that the piece k was reported on the directory, reporting
// false when an earlier Lose or a Place, through whichever Durability, already
// had. The call that took the piece is the one that reports it: a later call
// says only that the piece was named before, and never that its record is on
// disk.
func (s *dirState) take(k piece) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pieces[k]; ok {
		return false
	}
	s.pieces[k] = struct{}{}
	return true
}

// takeLocked is take with the caller holding mu.
func (s *dirState) takeLocked(k piece) bool {
	if _, ok := s.pieces[k]; ok {
		return false
	}
	s.pieces[k] = struct{}{}
	return true
}

// place takes the piece of the loss record an open found on disk (dropped with
// ev's extent) and publishes its event, in one section. A piece an earlier
// Lose or Place on the directory took — the previous open included —
// publishes nothing, so a later open does not report the same loss record as
// new; another piece of the same record is placed on its own.
func (s *dirState) place(ev core.LossEvent, dropped core.Seq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.takeLocked(piece{dropped, ev.Extent}) {
		return
	}
	s.pushLocked(ev)
}

// push publishes ev with the directory's next event number, overwriting the
// oldest when the ring is full. Every Durability on the directory shares the
// numbering.
func (s *dirState) push(ev core.LossEvent) core.LossEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushLocked(ev)
}

// pushLocked is push with the caller holding mu.
func (s *dirState) pushLocked(ev core.LossEvent) core.LossEvent {
	s.nextEvent++
	ev.SeqNum = s.nextEvent
	if s.count < core.LossRingSize {
		s.ring[(s.head+s.count)%core.LossRingSize] = ev
		s.count++
		return ev
	}
	s.ring[s.head] = ev
	s.head = (s.head + 1) % core.LossRingSize
	return ev
}

// losses returns a copy of the ring's events, oldest first, with the
// generation, read in the section a raise and its event are published in, so
// the pair never holds the raise without its event.
func (s *dirState) losses() ([]core.LossEvent, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.LossEvent, s.count)
	for i := range out {
		out[i] = s.ring[(s.head+i)%core.LossRingSize]
	}
	return out, s.gen
}

// raise raises the generation by one and returns it.
func (s *dirState) raise() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	return s.gen
}

// load returns the generation.
func (s *dirState) load() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// addFailed records a dropped sequence number whose loss record could not be
// appended. A number is recorded once.
func (s *dirState) addFailed(seq core.Seq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.failed, seq) {
		s.failed = append(s.failed, seq)
	}
}

// failedSeqs returns a sorted copy of the failed list.
func (s *dirState) failedSeqs() []core.Seq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.failed)
	slices.Sort(out)
	return out
}
