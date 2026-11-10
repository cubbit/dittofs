package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// shareCondition is the condition named name that Stats reports for share s
// naming file f, and whether there is one.
func shareCondition(st engine.Stats, s entity.ShareID, name string, f entity.FileID) (engine.Condition, bool) {
	for _, c := range st.Shares[s].Conditions {
		if c.Name == name && strings.Contains(c.Detail, f.String()) {
			return c, true
		}
	}
	return engine.Condition{}, false
}

// journalCondition is the condition named name that Stats reports for the
// unit's device, and whether there is one.
func journalCondition(st engine.Stats, name string) (engine.Condition, bool) {
	for _, c := range st.Journals["unit"].Conditions {
		if c.Name == name {
			return c, true
		}
	}
	return engine.Condition{}, false
}

// A file the group commit backs off alone is existence_commit_failing in
// Stats, for its share, naming it, with the store's error as its cause; its
// next existence commit clears it.
func TestStatsReportsExistenceCommitFailing(t *testing.T) {
	c := &conflicting{}
	u := newUnit(t, onCommit(c.commit))
	f := committedFile(t, u, "a", 4)
	c.on.Store(true)
	u.write(t, f, 0, []byte("over"))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := u.e.Commit(ctx, u.share.ID, f, 0, 4); err == nil {
		t.Fatal("Commit under a conflict succeeded")
	}
	got, ok := shareCondition(u.e.Stats(), u.share.ID, "existence_commit_failing", f)
	if !ok {
		t.Fatalf("Stats reports no existence_commit_failing naming %v: %+v", f, u.e.Stats().Shares)
	}
	if got.Subject.Scope != engine.ScopeShare || got.Subject.ID != u.share.ID.String() || got.Cause == nil {
		t.Errorf("existence_commit_failing is %+v, want the share's subject and a cause", got)
	}
	c.on.Store(false)
	u.clock.Advance(time.Second) // past the backoff
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if _, ok := shareCondition(u.e.Stats(), u.share.ID, "existence_commit_failing", f); ok {
		t.Error("existence_commit_failing still reported after the file's existence committed")
	}
}

// A failure that blames no file is existence_commit_failing for the journal,
// cleared once a batch commits.
func TestStatsReportsABatchFailingWhole(t *testing.T) {
	on := new(atomic.Bool)
	u := newUnit(t, onCommit(closedWhile(on)))
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Second, GroupCommitMaxFiles: 16, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("x"))
	on.Store(true)
	eventually(t, "Stats reports the batch failing", func() bool {
		u.clock.Advance(time.Second)
		_, ok := journalCondition(u.e.Stats(), "existence_commit_failing")
		return ok
	})
	if c, _ := journalCondition(u.e.Stats(), "existence_commit_failing"); c.Subject.Scope != engine.ScopeJournal || c.Subject.Name != "unit" || c.Cause == nil {
		t.Errorf("the batch's existence_commit_failing is %+v, want the device's subject and a cause", c)
	}
	on.Store(false)
	eventually(t, "the file commits once the failure ends", func() bool {
		u.clock.Advance(time.Minute)
		return u.e.FileEntries() == 0
	})
	if _, ok := journalCondition(u.e.Stats(), "existence_commit_failing"); ok {
		t.Error("the batch's existence_commit_failing still reported after a batch committed")
	}
}

// A removal failing five times in a row is removal_stalled in Stats, for its
// share, naming the file and the kind; it clears once the removal finishes.
func TestStatsReportsRemovalStalled(t *testing.T) {
	x := newResuming()
	u := newUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x })
	f := u.create(t, "f")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	x.failing[f] = true
	x.mu.Unlock()
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "Stats reports f removal_stalled", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return ok
	})
	c, _ := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
	if c.Subject.ID != u.share.ID.String() || !strings.Contains(c.Detail, entity.RemoveTruncate.String()) || c.Cause == nil {
		t.Errorf("removal_stalled is %+v, want the share's subject, the kind and a cause", c)
	}
	x.mu.Lock()
	x.failing[f] = false
	x.mu.Unlock()
	u.until(t, "f's removal is pruned once the fault clears", func() bool { _, ok := x.prunedAt(f); return ok })
	if _, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f); ok {
		t.Error("removal_stalled still reported after the removal finished")
	}
}

// A failed window's loss event raises journal_sync_failed with the journal
// still writable; it clears an hour after the latest failed window on the
// engine's clock, with no operator step, and a later window moves the hour.
func TestStatsReportsJournalSyncFailedForAnHour(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	failCommit := func() {
		t.Helper()
		u.write(t, f, 0, []byte("lost"))
		u.failWindow(f, 0, 4)
		if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := journalCondition(u.e.Stats(), "journal_sync_failed"); ok {
		t.Fatal("journal_sync_failed before any window failed")
	}
	failCommit()
	c, ok := journalCondition(u.e.Stats(), "journal_sync_failed")
	if !ok || c.Subject.Scope != engine.ScopeJournal || c.Subject.Name != "unit" || c.Cause == nil {
		t.Fatalf("after a failed window journal_sync_failed is %+v, %v; want raised for the device", c, ok)
	}
	u.clock.Advance(30 * time.Minute)
	failCommit() // the latest window, half an hour later
	u.clock.Advance(59 * time.Minute)
	if _, ok := journalCondition(u.e.Stats(), "journal_sync_failed"); !ok {
		t.Error("journal_sync_failed cleared within the hour after the latest failed window")
	}
	u.write(t, f, 0, []byte("kept")) // still writable
	u.clock.Advance(time.Minute)
	if _, ok := journalCondition(u.e.Stats(), "journal_sync_failed"); ok {
		t.Error("journal_sync_failed still reported an hour after the latest failed window")
	}
}

// A failed window's loss found at open failed before the restart: it raises
// no journal_sync_failed, nor does any other loss.
func TestStatsRaisesNoJournalSyncFailedForALossAtOpen(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("held"))
	id := journal.NewFileID(f)
	u.j.Report(journal.LossEvent{File: id, Extent: journal.Extent{Len: 4}, Reason: journal.LossFailedWindow, AtOpen: true})
	u.lose(f, 0, 4) // a corrupt loss
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil {
		t.Fatal(err)
	}
	if c, ok := journalCondition(u.e.Stats(), "journal_sync_failed"); ok {
		t.Errorf("journal_sync_failed %+v for a loss found at open or a corrupt one", c)
	}
}

// Each share's figures in Stats are the journal's for its own tag.
func TestStatsReportsEachSharesFiguresFromItsTag(t *testing.T) {
	u := newUnit(t)
	two := u.bindShare(t, "two", u.share.Namespace)
	if _, err := u.e.Write(t.Context(), u.share.ID, u.create(t, "a"), 0, make([]byte, 10), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), two, entity.NewFileID(), 0, make([]byte, 3), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	js := u.e.Stats().Journals["unit"]
	want := u.j.Stats()
	if js.UsedBytes != want.UsedBytes || js.UsedBytes != 13 {
		t.Errorf("the journal's UsedBytes %d, want the journal's %d", js.UsedBytes, want.UsedBytes)
	}
	if got := js.Shares[u.share.ID].UsedBytes; got != 10 {
		t.Errorf("share one's UsedBytes %d, want its tag's 10", got)
	}
	if got := js.Shares[two].UsedBytes; got != 3 {
		t.Errorf("share two's UsedBytes %d, want its tag's 3", got)
	}
	if js.NextVersion != want.NextVersion || js.VersionLimit != want.VersionLimit {
		t.Errorf("versions %v and %v, want the journal's %v and %v", js.NextVersion, js.VersionLimit, want.NextVersion, want.VersionLimit)
	}
}

// Stats takes no file's state lock: with one held, it returns.
func TestStatsTakesNoFileStateLock(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("x"))
	release := u.e.HoldEntry(f)
	defer release()
	done := make(chan struct{})
	go func() { u.e.Stats(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stats waited on a file's state lock")
	}
}

// Stats runs beside 100 writes and the group commit committing them.
func TestStatsBesideWrites(t *testing.T) {
	u := newUnit(t)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Millisecond, GroupCommitMaxFiles: 8, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				u.e.Stats()
				u.clock.Advance(time.Millisecond)
			}
		}
	}()
	var writes sync.WaitGroup
	for i := range 100 {
		f := u.create(t, string(rune('a'+i%26))+string(rune('a'+i/26)))
		writes.Add(1)
		go func() {
			defer writes.Done()
			if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
				t.Error(err)
			}
		}()
	}
	writes.Wait()
	eventually(t, "every write committed", func() bool { return u.e.FileEntries() == 0 })
	close(stop)
	wg.Wait()
	if got := u.e.Stats().Journals["unit"].Shares[u.share.ID].UsedBytes; got != 100 {
		t.Errorf("the share's UsedBytes %d, want the 100 bytes written", got)
	}
}

// A stalled removal whose phase 2 run advances without finishing clears
// removal_stalled.
func TestStatsClearsRemovalStalledOnAdvance(t *testing.T) {
	x := &cutResume{resuming: newResuming()}
	u := newUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x })
	f := u.create(t, "f")
	x.f = f
	x.cut.Store(-1 << 30) // no run advances yet
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	x.failing[f] = true
	x.mu.Unlock()
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "f removal_stalled", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return ok
	})
	x.cut.Store(1 << 30) // every run advances, none finishes
	u.until(t, "removal_stalled cleared on advance", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return !ok
	})
}

// A stalled removal whose share is dropped while its phase 2 runs clears
// removal_stalled when the run returns.
func TestStatsClearsRemovalStalledOnDrop(t *testing.T) {
	var f entity.FileID
	var hold atomic.Bool
	entered, open := make(chan struct{}), make(chan struct{})
	var once sync.Once
	u := newUnit(t, onResume(func(ctx context.Context, next engine.Content, s entity.ShareID, file entity.FileID) (bool, error) {
		if file != f {
			return next.Resume(ctx, s, file)
		}
		if hold.Load() {
			once.Do(func() { close(entered) })
			<-open
		}
		return false, errors.New("an injected phase 2 failure")
	}))
	f = u.create(t, "f")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "f removal_stalled", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return ok
	})
	hold.Store(true)
	u.until(t, "f's next run is held", func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	})
	if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
		t.Fatal(err)
	}
	close(open)
	u.until(t, "removal_stalled cleared once the dropped share's run returned", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return !ok
	})
}
