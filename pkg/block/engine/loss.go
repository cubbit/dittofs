package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"slices"
)

// losses is one journal's loss sequences: a journal-wide floor and an entry
// for each file whose own losses raised it above the floor. A file's sequence
// is the larger of the two. It lives in memory and only rises.
//
// ponytail: a raise of every file of the journal raises the floor past every
// entry and drops the entries, so memory holds only the files raised since;
// one entry per file raised alone stays until the next journal-wide raise.
// Expire entries of closed files when their count shows in a profile.
type losses struct {
	mu    sync.Mutex
	floor uint64
	files map[entity.FileID]uint64
	seq   uint64 // the newest loss event the drain read
	// windows are the files a failed-window event raised whose failed sync
	// has not yet reached a Sync of theirs: that Sync's SyncError raises them
	// no further.
	windows map[entity.FileID]bool

	// kept is each file's loss events that found no pending write of it:
	// the write may be between the journal's WriteAt and its insert, which
	// checks here. It is read and written only under the file's entry lock,
	// and keptMu, taken after that lock, guards the map itself.
	//
	// ponytail: an event of a file never written again, as of a write
	// already committed, stays until the process ends. Prune it once a loss of
	// a committed write is recorded in the store, which takes those events.
	keptMu sync.Mutex
	kept   map[entity.FileID][]journal.LossEvent
}

// keep keeps ev, which found no pending write of f. The caller holds f's
// entry lock.
func (l *losses) keep(f entity.FileID, ev journal.LossEvent) {
	l.keptMu.Lock()
	defer l.keptMu.Unlock()
	if l.kept == nil {
		l.kept = map[entity.FileID][]journal.LossEvent{}
	}
	l.kept[f] = append(l.kept[f], ev)
}

// unmatched is the kept events naming f's write at version v, which is
// being inserted, taken out. The caller holds f's entry lock.
func (l *losses) unmatched(f entity.FileID, v journal.Version) (found []journal.LossEvent) {
	l.keptMu.Lock()
	defer l.keptMu.Unlock()
	evs := l.kept[f]
	if len(evs) == 0 {
		return nil
	}
	// decision: a kept event below v belongs to a write already committed
	// and is discarded, since a file's writes reach the engine in version
	// order under its accept lock (see insert), so any earlier write was in
	// the entry, where markLost would have found it, until it committed.
	// Withdrawn with insert's own marker.
	evs = slices.DeleteFunc(evs, func(e journal.LossEvent) bool {
		if e.Version == v {
			found = append(found, e)
			return true
		}
		return e.Version.Compare(v) < 0
	})
	if len(evs) == 0 {
		delete(l.kept, f)
	} else {
		l.kept[f] = evs
	}
	return found
}

// seqOf is f's loss sequence. The caller holds mu.
func (l *losses) seqOf(f entity.FileID) uint64 { return max(l.floor, l.files[f]) }

// raise raises f's sequence. The caller holds mu.
func (l *losses) raise(f entity.FileID) {
	if l.files == nil {
		l.files = map[entity.FileID]uint64{}
	}
	l.files[f] = l.seqOf(f) + 1
}

// raiseAll raises every file's sequence through the floor. The caller holds
// mu.
func (l *losses) raiseAll() {
	for _, s := range l.files {
		l.floor = max(l.floor, s)
	}
	l.floor++
	clear(l.files)
}

// sync syncs ids of share b in the journal and then drains the journal's
// loss ring, so a caller that commits existence after it never commits a
// write the journal dropped as data.
//
// decision: every Sync the engine makes goes through this function, except
// the one in start step 3's replay, which runs before the journal's group
// commit exists and takes the losses from Since instead, so no other
// existence commit follows a sync whose losses were not drained first. A call
// of b.h.Sync elsewhere breaks that; revisit only if the journal ever reports
// its losses with its Sync's answer.
//
// A failed sync window, the journal's SyncError, is not an error of the sync:
// the drain drops the window's pending writes, each file the error names that
// no failed-window event already raised has its loss sequence raised, so the
// next Commit of each answers the raised sequence and the verifier the raised
// loss generation gives, never an I/O error, and the files are synced again,
// as many times as a window fails.
//
// A journal whose device was lost whole is synced no more: the sync answers
// the journal closed, so no Commit answers a verifier past the loss.
//
// A read-only journal's sync is refused: once the drain finds the journal
// read-only, the sync answers the journal's own refusal when its Sync
// answered one, and otherwise its own, errSyncRefused, which also matches
// journal.ErrJournalReadOnly and wraps the cause the journal entered with, so
// nothing commits existence or a removal over it (RFC 8 §2.5, §11.2). The two
// are told apart by undid: the journal refuses bare only a Sync of a file
// whose removal it undid.
//
// decision: the refusal does not rest on the Sync's own answer. The journal
// answers nil to a Sync whose records were all synced before the transition,
// and a failed window's SyncError only once, so the group commit, a
// removal's phase 1 and a stability point would go on committing over a
// journal that refuses every write, which RFC 8 §2.5 rules out: its reopen
// rebuilds pending existence from the journal rather than commit it. This is
// the one place every sync passes, so the state is read here, after the
// Sync. A sync that covered every record before the transition is refused
// too: its writes stay pending and its removal is retried with the guard
// held, though the journal holds them, until a reopen's recovery commits
// them. Overturned by a journal whose Sync answers which records it covered
// before the transition.
func (e *Engine) sync(ctx context.Context, b *binding, ids ...journal.FileID) error {
	_, err := e.syncMarks(ctx, b, ids...)
	return err
}

// syncMarks is sync, answering the marks the sync that succeeded returned.
func (e *Engine) syncMarks(ctx context.Context, b *binding, ids ...journal.FileID) ([]entity.SyncMark, error) {
	b.gc.mu.Lock()
	lost := b.gc.halted
	b.gc.mu.Unlock()
	if lost {
		return nil, journal.ErrClosed
	}
	for {
		marks, err := b.h.Sync(ctx, ids...)
		ro := e.drain(b)
		var failed *journal.SyncError
		if !errors.As(err, &failed) {
			if ro.ReadOnly && !errors.Is(err, journal.ErrJournalReadOnly) {
				return nil, readOnly(ro)
			}
			return marks, err
		}
		// decision: a file the SyncError names that no failed-window event
		// covers, as when the loss ring overflowed, keeps its pending writes
		// and commits them as data, since the engine cannot tell which of
		// them the window held. Overturned once the journal names the
		// failed versions with its SyncError.
		l := &b.gc.losses
		l.mu.Lock()
		for _, id := range failed.Files {
			if f := entity.FileID(id.Bytes()); l.windows[f] {
				delete(l.windows, f)
			} else {
				l.raise(f)
			}
		}
		l.mu.Unlock()
		// The journal promises nothing durable past a failed window, the
		// other files' writes and later ones included: sync again.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// errSyncRefused is the engine's refusal of a sync of a read-only journal
// whose Sync did not refuse it: see sync.
var errSyncRefused = errors.New("sync refused")

// readOnly is the refusal of a sync of a journal in state ro: it matches
// errSyncRefused and journal.ErrJournalReadOnly and wraps the cause the
// journal entered with.
func readOnly(ro journal.ReadOnlyState) error {
	return fmt.Errorf("%w: %w: %w", errSyncRefused, journal.ErrJournalReadOnly, ro.Err)
}

// undid reports whether err is the journal's own read-only refusal of a
// sync, which it answers, bare, only for a file whose removal it undid when
// it turned read-only, and not the engine's errSyncRefused.
func undid(err error) bool {
	return errors.Is(err, journal.ErrJournalReadOnly) && !errors.Is(err, errSyncRefused)
}

// drain applies the loss events the journal reported since the last drain,
// read by their sequence numbers: each corrupt or failed-window loss of an
// unoffloaded write raises its file's sequence, a gap in the ring raises
// every file's, a corrupt loss of a write still pending marks the run of
// that write it names lost, so its existence commit records that run lost
// and charges nothing for it, and a failed window's loss drops its pending
// write, calling its Done, charging nothing, and records its file as raised
// by its window.
//
// The drain holds the journal's loss lock until every write it marks is
// marked, so a drain that finds an event already applied finds its write
// marked too.
//
// decision: the drain reads the ring on every call rather than only once the
// loss generation moved, since the journal raises the generation before it
// publishes the event behind it (RFC 1 §3.8), and raises it for a range that
// names no record with no event at all: no generation tells which events it
// covers, so one stored as applied would hide an event published after it
// until the next raise. A drain that runs between a raise and its event
// misses that event, and the gap lasts the loss record's fsync on a read's
// loss, and a whole failed window's events after its one raise. A Commit in
// it answers a verifier that already carries the raise, so an NFS client
// resends; but it returns the file's loss sequence still unraised, so an SMB
// flush succeeds and the write commits as data, charged, and the next drain
// raises the sequence but cannot mark a write already committed. That is no
// worse than draining by the generation was. Revisit if the journal ever
// publishes an event before it raises the generation for it.
//
// decision: a corrupt loss of a pending write is taken as of a synced write
// and committed lost, and a failed window's as of an unsynced one, dropped,
// since the journal's loss event says why but not whether the write was
// synced. Revisit when the event says it.
//
// It answers the journal's read-only state, read with the ring.
func (e *Engine) drain(b *binding) journal.ReadOnlyState {
	l := &b.gc.losses
	var dones []func()
	defer func() { runAll(dones) }()
	l.mu.Lock()
	defer l.mu.Unlock()
	var lost []journal.LossEvent
	st := b.j.Stats()
	for _, ev := range st.Losses {
		if ev.SeqNum <= l.seq {
			continue
		}
		if ev.SeqNum > l.seq+1 {
			l.raiseAll() // the ring dropped events this drain never saw
		}
		l.seq = ev.SeqNum
		// decision: a superseded loss is to be skipped here as a stale one
		// is (RFC 8 §5.1: the reconcile raises superseded content, the
		// drain does not). The journal exports no superseded reason yet:
		// whichever of this and the journal's export lands second adds it
		// to this skip.
		if ev.Offloaded || ev.Reason == journal.LossStale {
			continue
		}
		f := entity.FileID(ev.File.Bytes())
		l.raise(f)
		lost = append(lost, ev)
		if ev.Reason == journal.LossFailedWindow {
			if l.windows == nil {
				l.windows = map[entity.FileID]bool{}
			}
			l.windows[f] = true
		}
	}
	if h := e.hooks.drained.Load(); h != nil {
		(*h)()
	}
	for _, ev := range lost {
		dones = append(dones, e.markLost(l, entity.FileID(ev.File.Bytes()), ev)...)
	}
	e.noteReadOnly(st.ReadOnly.ReadOnly, st.ReadOnly.Err)
	return st.ReadOnly
}

// markLost applies ev to the pending write of f whose record it dropped, the
// one at ev's version: a corrupt loss marks the run of it ev names lost, the
// rest of the write committed and charged as written; a failed window's drops
// it, out of the entry, or, when a batch claimed it, out of that batch, which
// neither commits it nor calls its Done. An event that finds no pending write
// is kept, under the entry's lock, for the write's insert, which applies it.
// It answers the Done of a dropped write, which the caller runs once its
// locks are released. The caller holds the journal's loss lock, which is
// taken before an entry's.
func (e *Engine) markLost(l *losses, f entity.FileID, ev journal.LossEvent) (dones []func()) {
	fe := e.files.lockFor(f)
	defer e.files.unlock(f, fe)
	found := false
	for i := 0; i < len(fe.ops); i++ {
		op := &fe.ops[i]
		if op.kind != opWrite || op.dropped || op.version != ev.Version || !overlaps(op.extent, ev.Extent) {
			continue
		}
		found = true
		if ev.Reason != journal.LossFailedWindow {
			op.lost = lostRuns(op.lost, entity.Extent{Offset: op.extent.Off, Length: op.extent.Len}, ev.Extent)
			continue
		}
		if op.done != nil {
			dones = append(dones, op.done)
			op.done = nil
		}
		if op.claimed || op.held {
			op.dropped = true
			continue
		}
		fe.ops = slices.Delete(fe.ops, i, i+1)
		i--
	}
	if !found {
		l.keep(f, ev)
	}
	return dones
}

// LossSeq is the file's loss sequence now, with no sync; 0 for a share with
// no binding.
func (e *Engine) LossSeq(s entity.ShareID, f entity.FileID) uint64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}
	e.drain(b)
	l := &b.gc.losses
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seqOf(f)
}

// lostWhole is journal j's device lost whole under the running process: every
// file's sequence is raised, no batch runs on j again, and every uncommitted
// write of j's files leaves its entry with its Done, charging nothing.
//
// decision: the journal reports no device lost while it runs, so nothing
// calls this but a test. Call it from that report once the journal has one.
//
// It runs to the end whatever its caller does, so no uncommitted write is
// left without its Done.
func (e *Engine) lostWhole(j Journal) {
	g := e.groupCommitFor(j)
	g.losses.mu.Lock()
	g.losses.raiseAll()
	g.losses.mu.Unlock()
	for {
		g.mu.Lock()
		g.halted = true
		inFlight, returned := g.inFlight, g.returned
		g.mu.Unlock()
		if !inFlight {
			break
		}
		<-returned
	}
	g.mu.Lock()
	var fs []entity.FileID
	for f := range g.due {
		fs = append(fs, f)
	}
	clear(g.due)
	for f := range g.failing { // no file of a lost journal is left to commit
		g.clearFailingLocked(f)
	}
	g.mu.Unlock()
	var dones []func()
	for _, f := range fs {
		fe := e.files.lockOf(f)
		if fe == nil {
			continue
		}
		kept := fe.ops[:0]
		for _, op := range fe.ops {
			if op.kind != opWrite {
				kept = append(kept, op)
				continue
			}
			if op.done != nil {
				dones = append(dones, op.done)
			}
		}
		fe.ops = kept
		e.files.unlock(f, fe)
	}
	g.mu.Lock()
	g.broadcastLocked() // every waiter then finds the journal lost
	g.mu.Unlock()
	runAll(dones)
}

// lostRuns is runs, a write's lost runs, with the part of the write w that
// x, a loss's extent, covers merged in.
func lostRuns(runs []entity.Extent, w entity.Extent, x journal.Extent) []entity.Extent {
	r := w.Intersect(entity.Extent{Offset: x.Off, Length: x.Len})
	return entity.MergeExtents(append(runs, r))
}

func overlaps(a, b journal.Extent) bool {
	return a.Off < b.Off+b.Len && b.Off < a.Off+a.Len
}
