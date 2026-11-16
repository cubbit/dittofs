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
// reboot. The failed list and the pieces a Lose or a Place has taken live
// here with it: a reopen does not re-report a loss record found on disk,
// for the piece it names is the directory's, taken once by whichever
// instance reported it. The ring of published events does not: it is the
// Durability's own, so a reopen starts an empty one and no live drain of
// one instance re-reads another's history.
var registry = struct {
	// decision: mu is a leaf mutex over dirs, held for one map lookup or
	// insert and never across I/O; it is outside the six lock domains.
	mu   sync.Mutex
	dirs map[string]*dirState
}{dirs: map[string]*dirState{}}

// dirState is one directory's generation, the sequence numbers whose loss
// record could not be appended, and the pieces a Lose or a Place has taken.
// Its mutex is a leaf: nothing is called while it is held.
//
// ponytail: an entry is one small struct per directory ever opened, and none
// is ever freed. Upgrade to freeing it when a directory is forgotten if a
// process ever opens thousands.
type dirState struct {
	// decision: mu is a leaf mutex over gen and failed, held for a counter
	// step or a list update and never across I/O; it is outside the six lock
	// domains.
	mu     sync.Mutex
	gen    uint64
	failed []core.Seq

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

// takePiece takes the piece of the loss record an open found on disk
// (dropped with ev's extent), reporting false when an earlier Lose or Place
// already had. The call that took the piece is the one that publishes; a
// later one — the previous open's Durability included — says only that the
// piece was named before, and never that its record is on disk.
func (s *dirState) takePiece(dropped core.Seq, ext core.Extent) bool {
	return s.take(piece{dropped, ext})
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

// FailedFor returns the failed list of the directory the journal is opening:
// the sequence numbers an earlier read-only journal on it could not write
// the loss record for, kept for the life of the process. A directory the
// process failed nothing on answers nil. A path that names no directory
// answers the error of resolving it.
//
// decision: the read lives here and not on a Durability because the open
// asks for the list before it builds one — the registry entry the question
// reads is the same one the layer it wires in will take, so a first open on
// a directory with a failed record in the window its process lost receives
// it, and the pieces the previous instance took answer for its loss
// records. Overturned by an open that asks a layer it built.
func FailedFor(dir string) ([]core.Seq, error) {
	st, err := stateFor(dir)
	if err != nil {
		return nil, err
	}
	return st.failedSeqs(), nil
}

// failedSeqs returns a sorted copy of the failed list.
func (s *dirState) failedSeqs() []core.Seq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.failed)
	slices.Sort(out)
	return out
}
