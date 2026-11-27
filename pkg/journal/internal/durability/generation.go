package durability

import (
	"cmp"
	"path/filepath"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
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
// the generation. A piece's claim is its taker's, and every claim retires on
// the same terms — when the instance that took it stopped before any reader
// reached the event the take published: its ring died unread up to that
// event, so nothing answered for the loss it published, letting a reopen
// take the piece over and place the record as found at open again (RFC 1
// section 3.8: at open, every loss record still on disk enters the ring,
// marked as found at open — a reopen's open included). A claim whose event
// a reader took away holds for the life of the directory, whatever became
// of the instance, and only as far as the reach: a later loss of the same
// instance, published past the reader and never taken away, retires its
// own claim. The ring of published events does not live here: it is the
// Durability's own, so a reopen starts an empty one and no live drain of
// one instance re-reads another's history.
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
	failed []failedWord

	// pieces holds every (dropped seq, extent) a Lose or a Place on the
	// directory has taken, each with the claim that took it — the taking
	// Durability's, placed or lost alike — which retires with it when no
	// reader reached the event the take published, so one piece of a
	// dropped record is reported once while the report was genuinely
	// delivered, while another piece of the same record, which a reconcile
	// can drop alone, is reported on its own.
	//
	// ponytail: it grows by one entry per piece lost for the life of the
	// process and is never pruned, so it follows the number of loss records
	// the directory holds, not the ring. Upgrade to dropping an entry once no
	// copy of its dropped record remains to be reported again, when a
	// directory with a long loss history shows up in a heap profile.
	pieces map[piece]claim

	// unsynced holds the taken pieces whose loss record placed but never
	// synced: the take stops a second event, but no durable record names
	// the loss, so a reopen's pass appends the record again. A piece leaves
	// once a later Lose of it takes it back to append.
	unsynced map[piece]struct{}

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
		st = &dirState{pieces: map[piece]claim{}}
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

// claim is one taken piece's holder: the reach of the instance whose Lose
// or Place took it, whose claim retires when the instance stops and no
// reader reached the event the take published into its ring, for then
// nothing answered for the loss it published. The reach is all the registry
// needs of the instance, so that is all the claim holds. The record on the
// disk keeps backing a placed claim's piece whatever the claim does, and
// only the REPORTING of it is at stake: a placed claim that retires lets a
// later open publish the record found at open again, which is the reader
// that reopen's ring is for.
type claim struct {
	r *reach
	// placed marks a Place's claim: the record the disk names is on it
	// already, so a reopen of it owes no append, and only its publication
	// is what the claim answers for.
	placed bool
	// unplaced marks a kept claim whose record never landed: the append
	// that would own one failed while the instance was already stopping,
	// so the record is still owed. It names nothing and bars no taker —
	// the next open plans the piece as unnamed — and serves the takeover
	// alone: the word that the dead instance published the loss, and the
	// reach of its ring decides whether the takeover republishes.
	unplaced bool
	// ev is the event number the claim's Lose published into its instance's
	// ring, so it is the claim's word for how far a reader must have
	// reached for the loss to count as answered. Zero when the loss
	// published no event of its own: a reported loss, whose report another
	// of the instance's events already carried.
	ev uint64
}

// heldLocked answers whether the claim on k still bars d from taking it:
// the caller's own earlier take holds as this instance's report, a live
// instance's claim holds while the ring it published to can still be read,
// and a stopped instance's claim holds as far as a reader reached the
// event the take published — the events a read took away were answered,
// so a re-report could only duplicate what that reader received, while
// the events past the reach answered nothing. s.mu is held by the caller.
//
// decision: only a stopped instance's unread claims retire — no reader
// reached the event the take published, so no one was answered for it —
// placed or lost alike, and each claim retires on its own event alone, for
// the reach says per loss which reports a reader took away. A placement a
// reader reached is the one claim that never dies, while an unread
// placement retires with its instance so a later open reports the record
// found at open to the reader that reopen's ring is for; a loss nobody
// read must not leave the ring by the third reopen. That is what serves
// an in-process reopen: RFC 1 section 3.8 places every loss record still
// on disk in the ring at every open, marked found at open, and the section
// 9.4 row has a first reopen place the record the dead instance took
// while only a later reopen does not report it as new. A live instance's
// claim still bars a second journal instance on one directory, which must
// not re-publish what a ring still readable already answered. The owner
// rules the other way → revert to take-only suppression and amend RFC 1
// section 3.8 to a restart-only reading.
func (s *dirState) heldLocked(k piece, d *Durability) bool {
	c, ok := s.pieces[k]
	if !ok {
		return false
	}
	r := d.reach
	// An unplaced claim bars no other taker: the record it names never
	// landed, so the piece is still owed, and the claim's word is read by
	// the takeover that follows, never by the hold. Its own instance it
	// bars still, however the claim is kept: a second Lose of the piece
	// through the taking Durability itself — a stopped one can queue two —
	// answers the dedup's zero event, never a takeover of its own claim.
	if c.r != r && c.unplaced {
		return false
	}
	// The same-instance arm holds placed or not, live or stopped: the
	// earlier take is this instance's own report.
	return c.r == r || !c.r.claimsDead() || c.r.claimServed(c.ev)
}

// setClaimEvent records that the claim d holds on k published ev into d's
// ring, so the claim, past d's stop, holds only as far as a reader reached
// that event. A claim replaced before the record keeps its own event.
func (s *dirState) setClaimEvent(k piece, d *Durability, ev uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.pieces[k]; ok && c.r == d.reach {
		c.ev = ev
		s.pieces[k] = c
	}
}

// markUnplaced marks the kept claim of d on k as owning no record: the
// append that would own one failed while the instance was already stopping,
// so the next open still owes the record. The claim stays — the takeover's
// word — but names nothing and bars no taker.
func (s *dirState) markUnplaced(k piece, d *Durability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.pieces[k]; ok && c.r == d.reach && !c.placed {
		c.unplaced = true
		s.pieces[k] = c
	}
}

// takeoverLocked replaces the retired claim on k — one whose instance
// stopped before any reader reached the event its take published — with c.
// The unsynced hold on k
// ends for a Lose, which appends the record now: the take's caller pays the
// debt the hold names. A Place keeps the hold, for the record the open
// found was placed but never made durable, and the open's pass still owes
// the append the hold says — the found-at-open event the Place publishes
// places a record the pass is about to re-append. s.mu is held by the
// caller.
func (s *dirState) takeoverLocked(k piece, c claim) {
	if !c.placed {
		delete(s.unsynced, k)
	}
	s.pieces[k] = c
}

// take records that the piece k was reported on the journal by d, reporting
// false when a claim that still holds — the Lose of d itself, of an
// instance still live, or of one whose take's event a reader took away, a
// placement a reader reached among them — already had it. The call that
// took the piece is the one that reports it: a later call says only that
// the piece was named before, and never that its record is on disk. The
// Lose of an instance stopped before any reader reached its take's event
// is taken over, for its claim retired with it: the loss it took is this
// take's to report, and takeover reports the same, so the caller learns
// the only report the loss had answered nothing. A dead unread placed
// claim is taken over the same way, its found-at-open report having
// answered nothing, and any takeover reports the reach of the dead ring
// alone: a reader that reached the event leaves the suppression its
// report already carried standing, while the record the claim never owned
// is still owed.
// The take also answers whether the claim it overtook held an
// unsynced record — a debt the caller's own append now pays, so a failure
// of that append re-holds the piece, never releasing it as though nothing
// named the loss.
func (s *dirState) take(k piece, d *Durability) (took, takeover, unsyncedDebt bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heldLocked(k, d) {
		return false, false, false
	}
	c, had := s.pieces[k]
	_, unsyncedDebt = s.unsynced[k]
	s.takeoverLocked(k, claim{r: d.reach})
	// A claim that reaches the take published its report unread, kept
	// unplaced ones with rings still served beyond it among them: their
	// word is the suppression the caller's Reported already carries.
	return true, had && !c.r.claimServed(c.ev), unsyncedDebt
}

// takePiece takes the piece of the loss record an open's Place found on
// disk (dropped over ext), reporting false when a claim that still holds
// already had it. The call that took the piece is the one that publishes;
// a later one says only that the piece was named before, and never that
// its record is on disk. The Lose of an instance stopped before any reader
// reached its loss's event is taken over as a Place's — no reader was ever
// answered for that loss, so the record the open found is placed anew,
// found at open, into the taking instance's own ring — and the claim that
// replaces it is that instance's, retiring on the same terms as any other:
// a placement a reader reached answers every later open, while an unread
// one retires with its instance and a later open reports the loss found
// at open again. The unsynced hold the dead claim carried stays: the
// record it named never landed durable, so the open's pass still owes the
// append, which clears the hold when its own Lose retakes the piece.
func (s *dirState) takePiece(d *Durability, dropped core.Seq, ext core.Extent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := piece{dropped, ext}
	if s.heldLocked(k, d) {
		return false
	}
	s.takeoverLocked(k, claim{r: d.reach, placed: true})
	return true
}

// named reports whether a Lose or a Place of this journal identity took the
// piece k and still answers for it. A claim whose record landed or is
// deferred holds while its instance lives or as far as a reader reached the
// event the take published; a placed claim answers true regardless, for the
// record it names was on the disk when the take took it, so no open owes the
// piece an append however unread its placement went. A kept
// failed claim — one whose record never landed, marked unplaced — answers
// for nothing however far its ring was read, for it owns no record at all:
// the piece is still owed, and the pass plans its record. The ceiling this
// arms: the answer keeps a record the directory already names only through
// a delivered report, so a claim that stopped before any reader took its
// event retires, named answers false and the pass plans a second record for
// the piece — the first still on disk. Two records naming one piece place
// once, the take answers either, and the later open reports the loss found
// at open exactly once, so the duplicate is a disk residue known and kept,
// never a second report; a deferred loss reaches the same shape, its copy
// still driven by the registry's deferral while the pass plans a record of
// its own. Withdrawn by a wording that owes the dead claim's report a
// delivery, which would make the pass consult the failed list's reach
// instead of this answer. The open's pass consults named apart from its own
// take, to know which losses no record is owed for.
func (s *dirState) named(k piece) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.pieces[k]
	return ok && !c.unplaced && (c.placed || !c.r.claimsDead() || c.r.claimServed(c.ev))
}

// release hands back a piece a live take claimed: the loss record it names
// never landed, and the next open owes that record — the failed list the
// same failure appended holds the claim it retries under. A stopped
// instance never releases: its claim retires on its own, and keeping it
// is what carries the report it never answered to the open that takes
// the piece over. Pieces of losses whose record is on disk are never
// released: the take is what stops a second record for them.
func (s *dirState) release(k piece) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pieces, k)
}

// holdUnsynced marks the taken piece k as held by a loss record that placed
// but never synced.
func (s *dirState) holdUnsynced(k piece) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsynced == nil {
		s.unsynced = map[piece]struct{}{}
	}
	s.unsynced[k] = struct{}{}
}

// isUnsynced reports whether a placed but unsynced loss record holds k.
func (s *dirState) isUnsynced(k piece) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.unsynced[k]
	return ok
}

// retakeUnsynced takes back the piece k a placed but unsynced record holds,
// for the caller to append the record it is owed, reporting false when no
// such record holds it. The piece stays taken.
func (s *dirState) retakeUnsynced(k piece) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.unsynced[k]; !ok {
		return false
	}
	delete(s.unsynced, k)
	return true
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

// Failed is one failed list entry in the answer an open reads: the dropped
// sequence number itself, and whether the failure's report still stands to
// be honoured — the reporting instance lives on, its ring readable, or a
// reader reached the event its failing Lose published. An open that owes
// the record this entry names plans it reported while Reported holds, and
// publishes it itself while it does not.
type Failed struct {
	Seq core.Seq
	// Reported says the failing instance's report was published where a
	// reader could take it: the instance is live, its ring readable by
	// whoever asks, or a reader reached the event its failing Lose
	// pushed — the reach, per loss, not the moment.
	Reported bool
}

// failedWord is one failed list entry as the registry keeps it: the dropped
// sequence number whose loss record could not be appended, the reach of the
// instance whose Lose could not append it, and the event number that Lose
// published — zero when it published none of its own, a reported loss whose
// report rode one of the window's other events. A read-only journal's
// refusal names a nil reach: its failure reached the writer by the refusal
// every Sync of the file returns, not by a ring event, so its report stands
// whatever became of any ring.
type failedWord struct {
	seq core.Seq
	r   *reach
	ev  uint64
}

// addFailed records a dropped sequence number whose loss record could not be
// appended, with the instance that could not append it and the event its
// failing Lose published. A number is recorded once: a later retry's own
// failure keeps the first's word, which errs on the re-reporting side, for
// a live retry's own ring answers for it whatever the entry says.
func (s *dirState) addFailed(seq core.Seq, d *Durability, ev uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.failed {
		if f.seq == seq {
			return
		}
	}
	var r *reach // nil for a read-only refusal, which reports through itself
	if d != nil {
		r = d.reach
	}
	s.failed = append(s.failed, failedWord{seq: seq, r: r, ev: ev})
}

// FailedFor returns the failed list of the journal the open names by dir
// and id: the sequence numbers whose loss record an earlier journal of the
// same identity, in this process, could not append, kept with the word on
// whether the failing report still stands — honoured by the planner that
// owes the record only while the reporter lives or a reader reached the
// event. A journal whose identity failed nothing answers nil; a journal
// Init'd anew at a path an older identity used asks its own empty answer.
// A path that names no directory answers the error of resolving it.
//
// decision: the read lives here and not on a Durability because the open
// asks for the list before it builds one — the registry entry the question
// reads is the same one the layer it wires in will take, so a first open on
// a directory with a failed record in the window its process lost receives
// it, and the pieces the previous instance took answer for its loss
// records. Overturned by an open that asks a layer it built.
func FailedFor(dir string, id core.JournalID) ([]Failed, error) {
	st, err := stateFor(dir, id)
	if err != nil {
		return nil, err
	}
	return st.failedWords(), nil
}

// failedWords returns the failed list in seq order, its Reported word
// answered per entry.
func (s *dirState) failedWords() []Failed {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Failed, 0, len(s.failed))
	for _, f := range s.failed {
		// A nil reporter's report stands on its own: the refusal that
		// told the writer lost nothing the ring could fail to carry.
		standalone := f.r == nil
		out = append(out, Failed{Seq: f.seq,
			Reported: standalone || !f.r.claimsDead() || f.r.claimServed(f.ev)})
	}
	slices.SortFunc(out, func(a, b Failed) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// failedSeqs returns a sorted copy of the failed list's sequence numbers.
func (s *dirState) failedSeqs() []core.Seq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Seq, 0, len(s.failed))
	for _, f := range s.failed {
		out = append(out, f.seq)
	}
	slices.Sort(out)
	return out
}

// deferAgainst stores one deferred loss against seg, the segment holding the
// dropped record its loss record is owed to the copy that retires. One entry
// per piece: a loss re-planned after the deferring instance's ring died
// unread reaches the same piece against the same segment, and the retiring
// copy writes one record per piece however many passes deferred it.
func (s *dirState) deferAgainst(seg core.SegmentID, l Loss) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.deferred[seg] {
		if e.Seq == l.Seq && e.Extent == l.Extent {
			return
		}
	}
	if s.deferred == nil {
		s.deferred = map[core.SegmentID][]Loss{}
	}
	s.deferred[seg] = append(s.deferred[seg], l)
}

// deferredOfTag answers the losses stored against any segment whose dropped
// content belonged to share tag, each with the segment it is deferred
// against: the one census a whole-tag forget needs, over a registry the set's
// own per-tag figures cannot stand in for — an unsynced record whose bytes
// never entered the set's counters carries no figure there, while the
// registry keeps the loss owed to whatever copy retires its segment. The
// answer is a copy; the sweep that ends a deferral reads it once and hands
// each entry back by its segment.
func (s *dirState) deferredOfTag(tag core.ShareTag) map[core.SegmentID][]Loss {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[core.SegmentID][]Loss)
	for seg, es := range s.deferred {
		for _, l := range es {
			if l.Share != tag {
				continue
			}
			out[seg] = append(out[seg], l)
		}
	}
	return out
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
