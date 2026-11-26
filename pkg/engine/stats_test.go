package engine_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/workqueue"
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
	if err := u.e.CommitExistence(t.Context(), u.share.ID, f); err != nil {
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
	settings := enginetest.Settings
	settings.ExistenceAge = time.Second
	settings.GroupCommitMaxFiles = 16
	if err := u.e.Apply(settings); err != nil {
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
	if err := u.e.CommitExistence(t.Context(), u.share.ID, f); err != nil {
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
	u.until(t, "f's removal is pruned once the fault clears", func() bool { return u.unpruned(t, f) == 0 })
	// The loop prunes before it clears the condition, so the clear is waited
	// for rather than read once.
	u.until(t, "removal_stalled clears once the removal finishes", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return !ok
	})
}

// A failed window's loss event raises journal_sync_failed with the journal
// still writable; it clears an hour after the latest failed window on the
// engine's clock, with no operator step, and a later window moves the hour.
func TestStatsReportsJournalSyncFailedForAnHour(t *testing.T) {
	u := newUnit(t)
	// No existence commit falls due within the test's clock jumps, so no
	// group commit starts a store transaction a later jump would see stalled.
	settings := enginetest.Settings
	settings.ExistenceAge = 24 * time.Hour
	if err := u.e.Apply(settings); err != nil {
		t.Fatal(err)
	}
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

// Stats carries the journal's whole snapshot: after a write, a commit's sync
// and a write refused at capacity, the copy Stats hands out equals the
// journal's own, field for field, so no figure is dropped on the way.
func TestStatsCarriesTheJournalsSnapshot(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("held"))
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 4); err != nil {
		t.Fatal(err)
	}
	// A refused reservation: the small share's own journal limit, two bytes,
	// refuses this write — the journal-wide capacity and its reservation
	// counters are the journal conformance suite's, over the real journal.
	small := enginetest.AddShare(t, u.fs.Store, "small", 2)
	if err := u.e.Bind(t.Context(), small); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), small.ID, entity.NewFileID(), 0, []byte("abc"), engine.WriteOpts{Done: func() {}}); err == nil {
		t.Fatal("a write past the share's journal limit was accepted")
	}
	js := u.e.Stats().Journals["unit"]
	if js.Stats == nil {
		t.Fatal("the carried snapshot is nil, want the journal's own Stats")
	}
	want := u.j.Stats()
	if !reflect.DeepEqual(*js.Stats, want) {
		t.Errorf("the carried snapshot is %+v, want the journal's own %+v", *js.Stats, want)
	}
}

// TagShares names the tag of every share attached to the journal, and no
// other: once a share is dropped, its tag is gone from the map.
func TestStatsTagSharesNameEveryAttachedShare(t *testing.T) {
	u := newUnit(t)
	two := enginetest.AddShare(t, u.fs.Store, "two", 1<<30)
	two.Namespace = u.share.Namespace
	if err := u.e.Bind(t.Context(), two); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), two.ID, entity.NewFileID(), 0, []byte("two's"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	js := u.e.Stats().Journals["unit"]
	want := map[journal.ShareTag]entity.ShareID{u.share.Tag: u.share.ID, two.Tag: two.ID}
	if !maps.Equal(js.TagShares, want) {
		t.Errorf("TagShares is %v, want each attached share's tag: %v", js.TagShares, want)
	}
	if err := u.e.Unbind(t.Context(), two.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Drop(t.Context(), two.ID, two.Tag); err != nil {
		t.Fatal(err)
	}
	js = u.e.Stats().Journals["unit"]
	if _, ok := js.TagShares[two.Tag]; ok {
		t.Errorf("TagShares names the dropped share's tag %d, want it gone", two.Tag)
	}
	if len(js.TagShares) != 1 || js.TagShares[u.share.Tag] != u.share.ID {
		t.Errorf("TagShares is %v after the drop, want only the bound share's tag %d", js.TagShares, u.share.Tag)
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
	if js.Stats.UsedBytes != want.UsedBytes || js.Stats.UsedBytes != 13 {
		t.Errorf("the journal's UsedBytes %d in the carried snapshot, want the journal's %d", js.Stats.UsedBytes, want.UsedBytes)
	}
	// The per-share figures read through the carried snapshot's own map,
	// each share's tag named by the entry's TagShares.
	tags := map[entity.ShareID]journal.ShareTag{}
	for tag, sid := range js.TagShares {
		tags[sid] = tag
	}
	if _, named := tags[u.share.ID]; !named {
		t.Fatal("share one is not in TagShares")
	}
	if _, named := tags[two]; !named {
		t.Fatal("share two is not in TagShares")
	}
	if got := js.Stats.Shares[tags[u.share.ID]].UsedBytes; got != 10 {
		t.Errorf("share one's UsedBytes %d, want its tag's 10", got)
	}
	if got := js.Stats.Shares[tags[two]].UsedBytes; got != 3 {
		t.Errorf("share two's UsedBytes %d, want its tag's 3", got)
	}
	if js.Stats.NextVersion != want.NextVersion || js.Stats.VersionLimit != want.VersionLimit {
		t.Errorf("versions %v and %v in the carried snapshot, want the journal's %v and %v", js.Stats.NextVersion, js.Stats.VersionLimit, want.NextVersion, want.VersionLimit)
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
	settings := enginetest.Settings
	settings.ExistenceAge = time.Millisecond
	settings.GroupCommitMaxFiles = 8
	if err := u.e.Apply(settings); err != nil {
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
	js := u.e.Stats().Journals["unit"]
	if got := js.Stats.Shares[u.share.Tag].UsedBytes; got != 100 {
		t.Errorf("the share's UsedBytes %d, want the 100 bytes written", got)
	}
}

// A stalled removal whose phase 2 run advances without finishing clears
// removal_stalled.
func TestStatsClearsRemovalStalledOnAdvance(t *testing.T) {
	u := newCutUnit(t)
	f := u.create(t, "f")
	u.holed(t, f, 3)
	u.fault(t, removalSet(2, 0, errInjected)) // every batch fails, so no run advances
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 0); err != nil {
		t.Fatal(err)
	}
	u.until(t, "f removal_stalled", func() bool {
		_, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f)
		return ok
	})
	// The next run commits a batch and fails the next one, so it advances
	// without finishing; the one run again at once fails, its backoff not
	// passed on the clock left still.
	before := u.e.Stats().RemovalRuns
	u.fault(t, removalSet(2, 0, errInjected))
	eventually(t, "the removal loop waits on its backoff", func() bool { return u.e.LoopParked(u.share.ID) })
	u.clock.Advance(time.Second)
	eventually(t, "an advanced run and a failed one after it", func() bool {
		st := u.e.Stats().RemovalRuns
		return st["advanced"] == before["advanced"]+1 && st["error"] == before["error"]+1
	})
	if _, ok := shareCondition(u.e.Stats(), u.share.ID, "removal_stalled", f); ok {
		t.Error("removal_stalled still raised after a run that advanced")
	}
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
	if err := u.e.CommitExistence(t.Context(), u.share.ID, f); err != nil {
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

// errorType is the one interface a returned value may hold.
var errorType = reflect.TypeFor[error]()

// valuePointees are the other packages' types a returned value may point to:
// snapshots the caller owns, never a component.
var valuePointees = []reflect.Type{
	reflect.TypeFor[journal.Stats](),
	reflect.TypeFor[journal.SyncFailure](),
}

// componentFields reports every place in t, reached through struct fields,
// pointers, map keys and values, and slice and array elements, that could hand
// the caller a component: an interface but error, a func or a channel
// anywhere, and a pointer to another package's type but a snapshot's. Another
// package's value type is walked as the engine's own; a struct's unexported
// fields are its own implementation and are not walked.
func componentFields(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	if seen[t] {
		return nil
	}

	seen[t] = true

	switch t.Kind() {
	case reflect.Interface:
		if t != errorType {
			return []string{path + ": interface " + t.String()}
		}
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return []string{path + ": " + t.Kind().String() + " " + t.String()}
	case reflect.Pointer:
		el := t.Elem()
		if el.PkgPath() != "" && el.PkgPath() != reflect.TypeFor[engine.Stats]().PkgPath() && !slices.Contains(valuePointees, el) {
			return []string{path + ": pointer to " + el.String()}
		}

		return componentFields(el, path+".*", seen)
	case reflect.Map:
		return append(componentFields(t.Key(), path+"[key]", seen), componentFields(t.Elem(), path+"[]", seen)...)
	case reflect.Slice, reflect.Array:
		return componentFields(t.Elem(), path+"[]", seen)
	case reflect.Struct:
		var out []string

		for f := range t.Fields() {
			if f.IsExported() {
				out = append(out, componentFields(f.Type, path+"."+f.Name, seen)...)
			}
		}

		return out
	}

	return nil
}

// Share, Stats and their parts hold values and identifiers, never a flow, a
// store, a scheduler or a journal (RFC 8 §12.1), so nothing the node reads
// or hands in reaches a component through them.
func TestStatsAndShareHoldNoComponent(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[engine.Share](),
		reflect.TypeFor[engine.Stats](),
		reflect.TypeFor[engine.ShareStats](),
		reflect.TypeFor[engine.JournalStats](),
	} {
		for _, bad := range componentFields(typ, typ.Name(), map[reflect.Type]bool{}) {
			t.Errorf("%s could hand the caller a component", bad)
		}
	}
}

// The walk refuses each way a component could be returned: a scheduler, a
// journal, a func and a channel, at a field, behind a map and in a slice.
func TestComponentFieldsRefusesAComponent(t *testing.T) {
	for _, c := range []struct {
		name string
		typ  reflect.Type
	}{
		{"a scheduler", reflect.TypeFor[struct{ S *workqueue.Scheduler }]()},
		{"a journal", reflect.TypeFor[struct{ J engine.Journal }]()},
		{"a func", reflect.TypeFor[struct{ F func() }]()},
		{"a channel in a map", reflect.TypeFor[struct{ M map[string]chan int }]()},
		{"a scheduler in a slice", reflect.TypeFor[struct{ S []*workqueue.Scheduler }]()},
		{"a scheduler as a map key", reflect.TypeFor[struct{ M map[*workqueue.Scheduler]int }]()},
	} {
		if len(componentFields(c.typ, c.name, map[reflect.Type]bool{})) == 0 {
			t.Errorf("the walk took %s", c.name)
		}
	}
}

// frozenStatsFields is every offload field of Stats and its parts, with its
// type: the readers of each, the node's adapter and the red rows, compile
// against them before offload fills them.
var frozenStatsFields = map[reflect.Type][]string{
	reflect.TypeFor[engine.ShareStats](): {
		"DirtyBytes int64", "DrainBytesPerSecond float64", "DrainTime time.Duration",
		"OldestUnoffloaded time.Duration", "RemovalsPending int64", "OldestRemoval time.Duration",
	},
	reflect.TypeFor[engine.Stats](): {
		"DirtyBytes int64", "DrainBytesPerSecond float64", "DrainTime time.Duration",
		"OldestUnoffloaded time.Duration", "RemovalsPending int64", "OldestRemoval time.Duration",
		"OffloadPasses map[string]int64", "PipelineStateSeconds map[string]engine.Series",
		"PipelineRetries map[string]int64", "AttemptsAbandoned int64", "IntentsAbandoned int64",
		"FailingFiles int64", "PassesAborted int64", "IntentMissing int64",
		"StageBusy map[string]float64", "Fetches map[string]int64", "FetchBytes int64",
		"Fills map[string]int64", "SpeculationBytes int64", "SpeculationUsedBytes int64",
		"Reresolves int64", "EvictedBytes int64", "PacingSeconds engine.Series",
		"LedgerMismatches int64", "ShareTableLockWaits engine.Series", "WorkQueueLockWaits engine.Series",
	},
	reflect.TypeFor[engine.JournalStats](): {
		"Work map[string]map[entity.ShardID]workqueue.Stats", "WorkLimits map[string]workqueue.Limits",
		"QueueEvents map[string]int64", "ReseedFilesRemaining int64",
	},
}

// Each offload field of Stats and its parts, and WaitOffloaded's signature,
// is as frozen: one renamed, dropped or retyped fails here.
func TestOffloadStatsSurfaceIsFrozen(t *testing.T) {
	for typ, fields := range frozenStatsFields {
		for _, want := range fields {
			name, _, _ := strings.Cut(want, " ")

			f, ok := typ.FieldByName(name)
			if !ok {
				t.Errorf("%s has no field %s, want %q", typ, name, want)

				continue
			}

			if got := f.Name + " " + f.Type.String(); got != want {
				t.Errorf("%s field %q, want %q", typ, got, want)
			}
		}
	}

	const wait = "func(*engine.Engine, context.Context) error"
	if got := reflect.TypeOf((*engine.Engine).WaitOffloaded).String(); got != wait {
		t.Errorf("WaitOffloaded is %s, want %s", got, wait)
	}
}
