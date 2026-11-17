package durability

import (
	"path/filepath"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// registry holds the per-journal state for the life of the process: one
// entry per directory AND identity together.
//
// decision: the loss generation lives here, keyed by directory and journal
// identity, and not in a Durability: a reopen in one process must not hand
// a client a verifier it has seen, and a new Durability on a journal a
// process already opened continues that journal's count rather than
// starting at zero. The count is not persisted, so a restart of the process
// starts it over; a client that sees the verifier change across a restart
// already treats it as a reboot. The identity is half the key because this
// process can make a NEW journal at a path it already used — empty the
// directory and Init one there, the conformance rig's own shape: the new
// journal starts fresh counters, so it re-reaches the sequence numbers the
// dead journal issued, and the dead journal's failed list, taken pieces
// and generation must not answer for it; a path alone would have them
// suppress the new journal's losses as already-taken.
//
// decision: no Forget of a dead journal's entry is offered alongside: the
// dead journal at this process's own hands is the empty-and-Init case, and
// its entry lingers keyed under the identity it died with, answering
// nothing the new journal asks. Withdrawing this by sweeping dead entries
// needs a rule for which identity at a path is live, which no state here
// owns.
//
// The failed list and the pieces a Lose or a Place has taken live here with
// the generation: a reopen does not re-report a loss record found on disk,
// for the piece it names is the journal's, taken once by whichever instance
// of its identity reported it. The ring of published events does not: it is
// the Durability's own, so a reopen starts an empty one and no live drain
// of one instance re-reads another's history.
var registry = struct {
	// decision: mu is a leaf mutex over dirs, held for one map lookup or
	// insert and never across I/O; it is outside the six lock domains.
	mu   sync.Mutex
	dirs map[dirKey]*dirState
}{dirs: map[dirKey]*dirState{}}

// dirState is one journal's generation, the sequence numbers whose loss
// record could not be appended, the pieces a Lose or a Place has taken, and
// the losses whose record a deferred Lose owes to the copy that retires the
// segment holding the dropped record — one journal being one identity at one
// directory, the registry's key. Its mutex is a leaf: nothing is called
// while it is held.
//
// ponytail: an entry is one small struct per journal ever opened — one
// identity at one directory, so a path emptied and Init'd many times holds
// one per identity — and none is ever freed. Upgrade to freeing it when a
// journal is forgotten if a process ever opens thousands.
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

	// deferred holds, per segment, the losses a Lose deferred against it —
	// one entry per deferred piece taken, the entry the copy that retires
	// the segment builds its loss record from. It lives here and not on a
	// Durability because the piece a deferral took is the directory's own,
	// taken once for whichever instance deferred it: a reopen in this
	// process builds a fresh Durability, and the copy that retires the
	// segment reaches the entry through whichever instance — or through the
	// registry reader below, before any instance is built. An entry leaves
	// on EndDeferral alone.
	deferred map[core.SegmentID][]Loss
}

// stateFor returns the state of dir, made on first use, keyed by its cleaned
// absolute path so two spellings of one directory share it.
func stateFor(dir string, id core.JournalID) (*dirState, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	k := dirKey{path: filepath.Clean(abs), id: id}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	st, ok := registry.dirs[k]
	if !ok {
		st = &dirState{pieces: map[piece]struct{}{}}
		registry.dirs[k] = st
	}
	return st, nil
}

// dirKey is one journal's per-process state: its directory and its identity
// together, for a path this process can empty and Init a new journal at,
// whose new identity re-reaches the sequence numbers the dead journal
// issued.
type dirKey struct {
	path string
	id   core.JournalID
}

// take records that the piece k was reported on the journal, reporting
// false when an earlier Lose or a Place, through whichever Durability of the
// same journal identity, already had. The call that took the piece is the
// one that reports it: a later call says only that the piece was named
// before, and never that its record is on disk.
func (s *dirState) take(k piece) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// FailedFor returns the failed list of the journal the open names by dir
// and id: the sequence numbers an earlier read-only journal of the same
// identity could not write the loss record for, kept for the life of the
// process. A journal whose identity failed nothing answers nil; a journal
// Init'd anew at a path an older identity used asks its own empty answer.
// A path that names no directory answers the error of resolving it.
//
// decision: the read lives here and not on a Durability because the open
// asks for the list before it builds one — the registry entry the question
// reads is the same one the layer it wires in will take, so a first open on
// a directory with a failed record in the window its process lost receives
// it, and the pieces the previous instance took answer for its loss
// records. Overturned by an open that asks a layer it built.
func FailedFor(dir string, id core.JournalID) ([]core.Seq, error) {
	st, err := stateFor(dir, id)
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

// deferAgainst stores one deferred loss against seg, the segment holding the
// dropped record its loss record is owed to the copy that retires.
func (s *dirState) deferAgainst(seg core.SegmentID, l Loss) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deferred == nil {
		s.deferred = map[core.SegmentID][]Loss{}
	}
	s.deferred[seg] = append(s.deferred[seg], l)
}

// deferredAgainst returns a copy of the losses stored against seg, in the
// order they deferred.
func (s *dirState) deferredAgainst(seg core.SegmentID) []Loss {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Loss(nil), s.deferred[seg]...)
}

// endDeferral ends one deferred loss of seg: the copy that retires it has the
// loss record durable, so the registry gives the entry up. l names the entry
// as deferredAgainst returned it. The call is idempotent: a second one, or a
// call whose entry already left, answers false and changes nothing; true says
// an entry left.
func (s *dirState) endDeferral(seg core.SegmentID, l Loss) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.deferred[seg]
	for i, e := range es {
		if e != l {
			continue
		}
		es = append(es[:i], es[i+1:]...)
		if len(es) == 0 {
			delete(s.deferred, seg)
		} else {
			s.deferred[seg] = es
		}
		return true
	}
	return false
}

// DeferredFor returns the deferred losses of the journal the open names by
// dir and id: each loss a Lose — of whichever Durability this process built
// on the directory — deferred against the segment holding its dropped record,
// keyed by that segment, for the copy that retires one to read before any
// layer of the open is built. A journal nothing deferred answers nil.
//
// decision: the read lives here and not on a Durability for the same reason
// FailedFor's does: the open asks for the losses before it builds the layer
// that would own them — the emptying runs inside the recovery, ahead of every
// layer — and the registry entry the question reads is the same one the
// layer the open wires in will take, so a loss one instance of the journal
// deferred reaches the copy the next instance retires, through whichever
// path reads it. Overturned by an open that regrows the losses it serves an
// instance it built rather than the registry.
func DeferredFor(dir string, id core.JournalID) (map[core.SegmentID][]Loss, error) {
	st, err := stateFor(dir, id)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[core.SegmentID][]Loss, len(st.deferred))
	for seg, ls := range st.deferred {
		out[seg] = append([]Loss(nil), ls...)
	}
	return out, nil
}
