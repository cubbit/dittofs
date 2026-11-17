//go:build engine_conformance

package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// condition reports whether Stats raises the condition name for the
// fixture's device or for any of its shares.
func condition(f *enginetest.Fixture, name string) bool {
	st := f.E.Stats()
	conds := st.Journals[f.Dir].Conditions

	for _, s := range st.Shares {
		conds = append(conds, s.Conditions...)
	}

	for _, c := range conds {
		if c.Name == name {
			return true
		}
	}

	return false
}

// pauseView is a store fault that holds the first view after it is armed,
// before the view reads anything, until release. A view reaches the fault
// with no key (kvtest.FaultKV's view, fault.go:196), so it cannot be scoped to
// a file: armed with nothing else running, it holds a read's metadata step,
// which the test checks. Every other call passes. A view from another
// goroutine armed first would be held instead; nothing else runs on the still
// clock between arm and the read, so none does.
type pauseView struct {
	once    sync.Once
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func newPauseView() *pauseView {
	return &pauseView{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *pauseView) At(op string, key []byte) (time.Duration, int, error) {
	p.mu.Lock()

	if !p.armed || op != "view" {
		p.mu.Unlock()

		return 0, 0, nil
	}

	p.armed = false
	p.mu.Unlock()
	close(p.entered)
	<-p.release

	return 0, 0, nil
}

// free lets the held view go; a second call does nothing.
func (p *pauseView) free() {
	p.once.Do(func() { close(p.release) })
}

func (p *pauseView) arm() {
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

// fileKey is how the store's fault seam names file id's keys: the file kind,
// then the first four bytes of the share and of the file, in hex. The engine's
// conformance package imports no store package, so the name is spelled here.
func fileKey(f *enginetest.Fixture, id entity.FileID) string {
	return "F‖share=" + hex.EncodeToString(f.Share.ID[:4]) + "‖file=" + hex.EncodeToString(id[:4])
}

// settle moves the clock an existence age at a time until the store has
// committed what file h's pending existence holds: done reports it.
func settle(t *testing.T, f *enginetest.Fixture, h entity.Handle, done func(entity.File) bool) {
	t.Helper()

	eventually(t, "the existence commit", func() bool {
		f.Clock.Advance(enginetest.Settings.ExistenceAge)

		return done(f.Committed(h))
	})
}

// §2.5 clone resumed at a full limit, the half this part runs: a clone of a
// dirty extent crashed after its copies, before its phase 1, on a share at
// its journal limit is resumed by the restart without copying again, every
// other file of the share is served, and the clone finishes. The remote
// being down, the destination's open answering DelayCapacity and an SMB write
// into the source held as capacity, are RFC 8's second part's, which composes
// the remote tier; what start reserves is not in the engine's Stats.
func TestRFC8A_CloneResumedAtAFullLimit(t *testing.T) {
	f := enginetest.New(t, enginetest.WithSegmentCap(mib))
	ctx := t.Context()

	const n = 64 << 10

	limitedSecond(t, f, 1<<20)

	src := secondFile(t, f, "src")
	dst := secondFile(t, f, "dst")
	other := secondFile(t, f, "other")
	filler := secondFile(t, f, "filler")
	s := bytes.Repeat([]byte{'s'}, n)

	mustSecond(t, f, src, 0, n, 's')
	mustSecond(t, f, other, 0, 4096, 'o')

	entered, release := f.HoldNextClone()
	cloned := make(chan error, 1)

	go func() {
		cloned <- f.E.Clone(ctx, f.Second.ID, src, f.Second.ID, dst, vfserr.Extent{Len: n})
	}()

	wait(t, "the clone reaches its phase 1", entered)

	full := false
	deadline := time.Now().Add(bound)

	for off := int64(0); off < 2<<20 && !full; off += 4096 {
		if time.Now().After(deadline) {
			t.Fatalf("the share did not reach its journal limit within %v", bound)
		}

		var d *vfserr.Delay

		err := putSecond(t, f, filler, off, 4096, 'f')
		full = errors.As(err, &d) && d.Cause == vfserr.DelayCapacity

		if err != nil && !full {
			t.Fatalf("filling the share at %d: %v, want only the capacity delay", off, err)
		}
	}

	if !full {
		t.Fatal("the share never reached its journal limit")
	}

	// The crash drops what is not durable, so the files the row reads after
	// it are synced first, and used is read once the filler is durable: the
	// share then stays at its limit through the restart.
	h := f.HandleOf(f.Second.Tag)

	for _, id := range []entity.FileID{src, other, filler} {
		if _, err := h.Sync(ctx, journal.NewFileID(id)); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}

	used := f.E.Stats().Journals[f.Dir].Shares[f.Second.ID].UsedBytes
	if used == 0 {
		t.Fatal("the second share's journal bytes read 0: its stats are missing, so no re-copy could show")
	}

	f.Crash()
	release()

	if got := f.E.Stats().Journals[f.Dir].Shares[f.Second.ID].UsedBytes; got != used {
		t.Errorf(
			"the share's journal bytes after the restart: %d, want %d (no copy made again)",
			got,
			used,
		)
	}

	got := make([]byte, 4096)
	if _, err := f.E.Read(ctx, f.Second.ID, other, 0, got); err != nil ||
		!bytes.Equal(got, bytes.Repeat([]byte{'o'}, 4096)) {
		t.Errorf("another file of the share after the restart: %q, %v", got[:8], err)
	}

	eventually(t, "the resumed clone finishes", func() bool {
		f.Clock.Advance(time.Second)

		p := make([]byte, n)
		_, err := f.E.Read(ctx, f.Second.ID, dst, 0, p)

		return err == nil && bytes.Equal(p, s)
	})
}

// §7.1 the join, the rows a journal holding no remote ref answers: a committed
// hole reads zeros, and an extent whose synced record the journal lost reads
// Lost after the restart, never zeros and never the committed bytes it
// superseded. The rows with remote refs, carved and stale, are RFC 8's second
// part's.
func TestRFC8A_TheJoin(t *testing.T) {
	f := enginetest.New(t, enginetest.WithSegmentCap(mib))
	ctx := t.Context()
	h, id := f.File("a")
	old := bytes.Repeat([]byte{'o'}, 4096)
	p := bytes.Repeat([]byte{'n'}, 4096)

	write(t, f, id, 0, old)

	if err := f.E.Truncate(ctx, f.Share.ID, id, 16384); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	settle(t, f, h, func(r entity.File) bool { return r.Size == 16384 })
	write(t, f, id, 0, p)

	if _, err := f.Handle().Sync(ctx, journal.NewFileID(id)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	corrupt(t, f, p)
	f.Crash()

	if got := f.Committed(h).Size; got != 16384 {
		t.Errorf("committed size %d, want 16384", got)
	}

	if got := read(t, f, id, 8192, 4096); !bytes.Equal(got, make([]byte, 4096)) {
		t.Error("the committed hole does not read zeros")
	}

	dst := make([]byte, 4096)

	_, err := f.E.Read(ctx, f.Share.ID, id, 0, dst)
	if !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("the lost extent reads %q, %v; want ErrLost", dst[:8], err)
	}
}

// §7.1 existence above asOf, its hole-fill and growth halves: a read paused
// between its journal step and its metadata step, while a write fills the
// hole it saw, or grows the file past the end it saw, and commits, returns
// zeros or the new bytes, never Lost. The overwrite half needs the journal to
// lose a held record of one file, which the fixture cannot do yet: this
// part's burn-down of its known failures owns it, with that fixture change.
func TestRFC8A_ExistenceAboveAsOf(t *testing.T) {
	for _, c := range []struct {
		name string
		size int64 // the file's committed size before the read
		at   int64 // where the read and the write land
	}{
		{"a write fills the hole the read saw", 16384, 4096},
		{"a write grows the file past the end the read saw", 4096, 4096},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := enginetest.New(t)
			ctx := t.Context()
			h, id := f.File("a")

			write(t, f, id, 0, []byte("head"))

			if err := f.E.Truncate(ctx, f.Share.ID, id, c.size); err != nil {
				t.Fatalf("Truncate: %v", err)
			}

			settle(t, f, h, func(r entity.File) bool { return r.Size == c.size })

			pause := newPauseView()
			f.FaultStore(pause)
			t.Cleanup(pause.free)
			pause.arm()

			type result struct {
				p   []byte
				n   int
				err error
			}

			done := make(chan result, 1)

			go func() {
				p := make([]byte, 4096)
				n, err := f.E.Read(ctx, f.Share.ID, id, c.at, p)

				done <- result{p, n, err}
			}()

			wait(t, "the read's metadata step reaches the store", pause.entered)

			select {
			case r := <-done:
				t.Fatalf("the read answered %v before its metadata step was let go", r.err)
			default:
			}

			before := f.Committed(h).Applied
			p := bytes.Repeat([]byte{'w'}, 4096)
			write(t, f, id, c.at, p)
			settle(t, f, h, func(r entity.File) bool { return r.Applied != before })

			// The read is still held once the commit landed: a held view that is
			// not the read's would let it answer before the write.
			select {
			case r := <-done:
				t.Fatalf("the read answered %v before the write committed: the held view was not the read's", r.err)
			default:
			}

			pause.free()

			var r result
			select {
			case r = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the read never answered once its metadata step was let go")
			}

			if errors.Is(r.err, vfserr.ErrLost) {
				t.Fatalf("the read paused across the commit: %v, want old or new bytes", r.err)
			}

			// Growth: the read saw the end at c.at, so nothing is the old answer;
			// zeros there mean its size came from the store without asking the
			// journal again.
			got := r.p[:r.n]
			hole := c.size > c.at && r.n == len(p) && bytes.Equal(got, make([]byte, r.n))
			eof := c.size <= c.at && r.n == 0

			if r.err != nil || (!bytes.Equal(got, p) && !hole && !eof) {
				t.Errorf(
					"the read paused across the commit: %d bytes %q, %v; want the new bytes, or zeros in the hole, or nothing at the old end",
					r.n,
					got[:min(8, r.n)],
					r.err,
				)
			}
		})
	}
}

// §9.1 source registration rebuilt, adapted to one share: a process stopped
// once a clone's phase 1 committed and before its batches ran restarts with
// the source's extent registration rebuilt from its clone-kind open before
// the source is served, so a write into the frozen extent answers ErrDelay
// until the clone is done, and is served after. The fixture's two shares
// sit in two namespaces, where a clone is refused, so the two-share half is
// owned by this part's burn-down of its known failures, with a fixture share
// in the first share's namespace; a clone across two shards sharing a primary
// is the cluster's.
func TestRFC8A_SourceRegistrationRebuilt(t *testing.T) {
	f := enginetest.NewWithSmallBatches(t, 14, enginetest.WithSegmentCap(mib))
	ctx := t.Context()

	const n = 61440

	_, src := f.File("src")
	_, dst := f.File("dst")

	for off := int64(0); off < n; off += 8192 {
		write(t, f, src, off, bytes.Repeat([]byte{'s'}, 4096))
	}

	// The crash below drops what is not durable, so the source is synced first.
	if _, err := f.Handle().Sync(ctx, journal.NewFileID(src)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	f.HoldBatches()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() { _ = f.E.Clone(cctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}) }()

	eventually(t, "the clone's phase 1 writes the source's clone-kind open", func() bool {
		opens, err := f.FS.Store.Opens().Of(ctx, f.Share.ID, src)

		return err == nil && hasCloneSource(opens)
	})

	frozen := func(what string) {
		t.Helper()

		_, err := f.E.Write(ctx, f.Share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}})

		var d *vfserr.Delay
		if !errors.As(err, &d) || d.Cause != vfserr.DelayClone {
			t.Fatalf(
				"a write into the frozen source extent %s: %v, want ErrDelay(DelayClone)",
				what,
				err,
			)
		}
	}

	frozen("before the stop")
	f.Crash()
	frozen("after the restart, before the clone's batches run")
	f.ReleaseBatches()

	eventually(t, "the source is served once the clone is done", func() bool {
		f.Clock.Advance(time.Second)
		_, err := f.E.Write(ctx, f.Share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}})

		return err == nil
	})

	// An aborted clone frees the source too, so the destination must hold what
	// the source held when the clone began (the write above landed after it).
	got := make([]byte, 4096)
	if _, err := f.E.Read(ctx, f.Share.ID, dst, 0, got); err != nil ||
		!bytes.Equal(got, bytes.Repeat([]byte{'s'}, 4096)) {
		t.Errorf("the destination after the resumed clone: %q, %v, want the source's bytes", got[:8], err)
	}
}

// hasCloneSource reports whether opens holds a clone-kind open.
func hasCloneSource(opens []entity.OpenRecord) bool {
	for _, o := range opens {
		if o.Kind == entity.OpenCloneSource {
			return true
		}
	}

	return false
}

// refsFault fails every read of one file's refs as a store whose record does
// not decode would, while failing is set.
type refsFault struct {
	prefix  string
	failing sync.Mutex
	on      bool
}

func (r *refsFault) At(op string, key []byte) (time.Duration, int, error) {
	r.failing.Lock()
	defer r.failing.Unlock()

	if r.on && (op == "scan" || op == "before") &&
		strings.HasPrefix(string(key), r.prefix) {
		return 0, 0, kvstore.ErrInconsistent
	}

	return 0, 0, nil
}

func (r *refsFault) set(on bool) {
	r.failing.Lock()
	r.on = on
	r.failing.Unlock()
}

// §8.1 removal wedge, its truncate half: a file whose refs cannot be read
// keeps its removal retrying, removal_stalled is raised, another file's
// removal completes meanwhile, and the stalled one finishes once its refs
// read again. The half left: the backoff cap's value and the clone-onto half,
// a clone whose source record does not decode, are owned by this part's
// burn-down of its known failures, with the fixture change that corrupts one
// record and reads the condition per file.
func TestRFC8B_RemovalWedge(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	hb, bad := f.File("bad")
	hg, good := f.File("good")

	for _, id := range []entity.FileID{bad, good} {
		write(t, f, id, 0, bytes.Repeat([]byte{'x'}, 8192))
	}

	settle(t, f, hb, func(r entity.File) bool { return r.Size == 8192 })
	settle(t, f, hg, func(r entity.File) bool { return r.Size == 8192 })

	refs := &refsFault{prefix: fileKey(f, bad) + "‖ref"}
	f.FaultStore(refs)
	refs.set(true)

	badDone := make(chan error, 1)

	go func() { badDone <- f.E.Truncate(ctx, f.Share.ID, bad, 0) }()

	eventually(t, "removal_stalled raised for the file whose refs do not read", func() bool {
		f.Clock.Advance(time.Second)

		return condition(f, "removal_stalled")
	})

	// The other file's removal starts while the wedged one is stalled, so a
	// loop that a stalled item holds up never finishes it.
	if err := f.E.Truncate(ctx, f.Share.ID, good, 0); err != nil {
		t.Fatalf("the other file's truncate: %v", err)
	}

	eventually(t, "the other file's removal finishes while the wedged one retries", func() bool {
		f.Clock.Advance(time.Second)

		for _, u := range unfinishedOf(t, f) {
			if u.File == good {
				return false
			}
		}

		return true
	})

	listed := false

	for _, u := range unfinishedOf(t, f) {
		listed = listed || u.File == bad
	}

	if !listed {
		t.Fatal("the stalled removal is not listed unfinished, so its end below proves nothing")
	}

	refs.set(false)

	eventually(t, "the stalled removal finishes once its refs read", func() bool {
		f.Clock.Advance(time.Second)

		if condition(f, "removal_stalled") {
			return false
		}

		// The condition also clears when the removal only advances, so the
		// removal's own end is checked, as the other file's is above.
		for _, u := range unfinishedOf(t, f) {
			if u.File == bad {
				return false
			}
		}

		return true
	})

	// A real-time backstop only: the truncate answered at its phase 1, long
	// before, so a correct engine never meets it.
	select {
	case err := <-badDone:
		if err != nil {
			t.Errorf("the wedged file's truncate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the wedged file's truncate never answered")
	}
}

// deviceFull is a storage fault that fails every create and allocation, of a
// segment file or not, with ENOSPC while on is set, as a device with no room does.
type deviceFull struct {
	mu      sync.Mutex
	on      bool
	refused int
}

func (d *deviceFull) At(op string, _ []byte) (time.Duration, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.on && (op == "create" || op == "allocate") {
		d.refused++

		return 0, 0, syscall.ENOSPC
	}

	return 0, 0, nil
}

// fired is how many creates and allocations the fault has refused.
func (d *deviceFull) fired() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.refused
}

func (d *deviceFull) set(on bool) {
	d.mu.Lock()
	d.on = on
	d.mu.Unlock()
}

// §11.1 journal_full on a full device: with segments of 1 MiB and the device
// refusing every new allocation with ENOSPC, writes run until a roll finds no
// segment to take, and that refusal raises journal_full, which stays while a
// write into an allocated segment is accepted and clears, with nothing
// written, once the device has room.
func TestRFC8B_JournalFullOnAFullDevice(t *testing.T) {
	f := enginetest.New(t, enginetest.WithSegmentCap(mib))
	_, id := f.File("a")

	write(t, f, id, 0, []byte("x"))

	full := &deviceFull{}
	f.Seam.SetFault(full)
	full.set(true)
	t.Cleanup(func() { full.set(false) })

	const chunk = 256 << 10

	var refused error

	buf := bytes.Repeat([]byte{'a'}, chunk)

	for off := int64(0); off < 64*mib && refused == nil; off += chunk {
		_, refused = f.E.Write(
			t.Context(),
			f.Share.ID,
			id,
			off,
			buf,
			engine.WriteOpts{Done: func() {}},
		)
	}

	if refused == nil {
		t.Fatal("no write was refused on a device with no room")
	}

	// The device's own refusal is answered ErrNoSpace; a roll with no spare
	// ready answers ErrDelay and raises nothing (RFC 8 §10.2, §11.1). This
	// check comes first: which of the two the engine meets first depends on
	// the scheduler, and both answer a capacity delay today.
	if !errors.Is(refused, vfserr.ErrNoSpace) {
		t.Fatalf("the refused roll answered %v, want the device's ErrNoSpace", refused)
	}

	// A refusal with no fault fired is the plain capacity limit, not the device.
	if full.fired() == 0 {
		t.Fatalf("no create or allocation was refused ENOSPC; the write was refused for %v", refused)
	}

	if !condition(f, "journal_full") {
		t.Fatalf("a roll refused on a full device (%v) raised no journal_full", refused)
	}

	write(t, f, id, 0, []byte("y"))

	if !condition(f, "journal_full") {
		t.Error("journal_full cleared by a write into an allocated segment")
	}

	full.set(false)

	eventually(t, "journal_full clears once the device has room", func() bool {
		f.Clock.Advance(time.Second)

		return !condition(f, "journal_full")
	})
}

// §2.4 settings, the engine's half: each setting Apply checks, at 0, is
// refused and leaves the earlier settings in force, so a clone longer than
// the clone_max_len in force is refused and one of exactly it accepted. Its
// pool settings, Min and Target, and a share's profile
// change reported as a migration are RFC 8's second part's, which has the
// cache those settings size.
func TestRFC8C_Settings(t *testing.T) {
	for _, c := range []struct {
		name  string
		spoil func(*engine.Settings)
	}{
		{"existence_age 0", func(s *engine.Settings) { s.ExistenceAge = 0 }},
		{"group_commit.max_files 0", func(s *engine.Settings) { s.GroupCommitMaxFiles = 0 }},
		{"clone_max_len 0", func(s *engine.Settings) { s.CloneMaxLen = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			// One spare per stream's first roll and one for the single writer, so
			// the clone's roll never waits on the pool's refill in real time.
			f := enginetest.New(t, enginetest.WithSpares(3))
			ctx := t.Context()
			s := enginetest.Settings
			s.CloneMaxLen = 4096

			if err := f.E.Apply(s); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			bad := s
			bad.CloneMaxLen = 1 << 30
			c.spoil(&bad)

			if err := f.E.Apply(bad); err == nil {
				t.Fatalf("Apply took %s", c.name)
			}

			_, src := f.File("src")
			_, dst := f.File("dst")

			write(t, f, src, 0, make([]byte, 8192))

			// A clone waits on nothing the test clock moves, so a bound only turns a
			// hang into a failure, as settings the engine should have refused can cause.
			bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			err := f.E.Clone(bctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: 4097})
			if !errors.Is(err, vfserr.ErrInvalid) {
				t.Errorf(
					"a clone longer than the clone_max_len in force: %v, want ErrInvalid",
					err,
				)
			}

			err = f.E.Clone(bctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: 4096})
			if err != nil {
				t.Errorf("a clone of exactly the clone_max_len in force: %v, want it accepted", err)
			}
		})
	}
}

// RFC 8 §7.5, a carved run fails unavailable: a run the store records as carved, which
// nothing in this part fetches, fails the read ErrUnavailable, never zeros or
// Lost. Its fetch from the remote is RFC 8's second part's.
func TestRFC8A_CarvedRunFailsUnavailable(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	h, id := f.File("a")

	const n = 1 << 20

	// The existence of [0, 2 MiB) is another writer's, committed to the store
	// alone, so the journal holds nothing of the file and the read asks the
	// store; the engine writes no ref in this part either.
	v := entity.VersionOf(1 << 62)

	if _, err := f.FS.Store.Existence().Commit(ctx, []kvstore.PendingExistence{{
		Share: f.Share.ID,
		File:  id,
		Ops: []kvstore.PendingOp{{
			Version: v,
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Length: 2 * n},
		}},
	}}, nil); err != nil {
		t.Fatalf("commit another writer's existence: %v", err)
	}

	if got := f.Committed(h).Size; got != 2*n {
		t.Fatalf("committed size %d, want %d", got, 2*n)
	}

	var ns entity.NamespaceID

	for sh, err := range f.FS.Store.ControlPlane().Shares(ctx) {
		if err != nil {
			t.Fatal(err)
		}

		if sh.ID == f.Share.ID && len(sh.Namespaces) > 0 {
			ns = sh.Namespaces[0].Namespace
		}
	}

	if ns == (entity.NamespaceID{}) {
		t.Fatal("the fixture's share has no namespace in the control plane")
	}

	var name entity.BlockName

	name[31] = 1

	var chunk entity.ChunkHash

	chunk[31] = 1

	intent := kvstore.Intent{Domain: kvstore.DomainShard}
	if err := f.FS.Store.Content().Intend(ctx, ns, name, intent); err != nil {
		t.Fatalf("Intend: %v", err)
	}

	res, err := f.FS.Store.Content().Commit(ctx, kvstore.BlockCommit{
		Namespace: ns,
		Name:      name,
		Size:      n,
		Chunks:    []kvstore.ChunkAt{{Hash: chunk, Length: n}},
		Files: []kvstore.FileCommit{{
			Share: f.Share.ID,
			File:  id,
			Refs:  []entity.ChunkRef{{Offset: n, Length: n, Chunk: chunk, Oldest: v, Newest: v}},
		}},
	})
	if err != nil || len(res[id]) != 1 {
		t.Fatalf("Commit a carved ref: %v, %v; want the file's run recorded", res, err)
	}

	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err = f.E.Read(bctx, f.Share.ID, id, n, make([]byte, 4096))
	// ErrUnavailable answers many paths, so the carved run's own text is pinned.
	if !errors.Is(err, vfserr.ErrUnavailable) || !strings.Contains(err.Error(), "is carved") {
		t.Errorf("a read of the carved run: %v, want ErrUnavailable naming the carved run", err)
	}
}
