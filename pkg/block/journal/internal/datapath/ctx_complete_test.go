package datapath

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// writeResult is what one public call returned.
type writeResult struct {
	v   core.Version
	err error
}

// TestCancelledAfterAppendCompletes checks the completeness rule for writes: a WriteAt whose
// context is cancelled once its record is in its segment still returns its
// version, never the context's error, and the extent reads back. Both legs
// stop the write on that one rule at the two points the tests can reach below
// the record's future: one parks it in the gap between the append and the
// publish, one holds the record's own segment write at the seam.
//
// The mutation both legs catch: a publish tail that consults the context —
// one returning ctx.Err() after the record is appended — answers the
// cancelled caller with the context's error and fails the version assertion.
func TestCancelledAfterAppendCompletes(t *testing.T) {
	legs := []struct {
		name  string
		build func(t *testing.T) (c *Core, s *Share, f core.FileID, reached <-chan struct{}, unPark func())
	}{
		{
			name: "held between the append and the publish",
			build: func(t *testing.T) (*Core, *Share, core.FileID, <-chan struct{}, func()) {
				parked := make(chan struct{})
				release := make(chan struct{})
				c, _ := newTestCore(t, func(o *coreOptions) {
					// The leg's write is the core's first and only one, so
					// the seam fires for it alone, parked with the file's
					// append domain held.
					o.afterAppend = func() {
						close(parked)
						<-release
					}
				})
				// A failure past the park must not leave the write parked
				// for the core's cleanup to wait on.
				unPark := releaser(t, release)
				return c, mustShare(t, c, 1), testFile(1), parked, unPark
			},
		},
		{
			name: "held at the record's own segment write",
			build: func(t *testing.T) (*Core, *Share, core.FileID, <-chan struct{}, func()) {
				c, seam := newTestCore(t)
				s, f := mustShare(t, c, 1), testFile(1)
				// The first write lands the file's stream on its segment,
				// so the first segment write the hold below can stop is the
				// record's own, not the header the stream wrote when it took
				// the segment.
				warm(t, c, s, f)
				hold := seam.HoldFirst(t, kit.OpWrite, "*")
				// decision: the seam cannot see the moment the record
				// commits in the core's own memory, only the write to the
				// segment. The held segment write is the first point at
				// which the record's bytes are committed to storage, so
				// cancelling there is cancelling after the append: the call
				// must complete rather than leave a write the caller was
				// told failed but that recovery serves. Overturned only
				// when the core gains an observable commit point below the
				// segment write.
				return c, s, f, hold.Reached(), hold.Release
			},
		},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			c, s, f, reached, unPark := leg.build(t)
			payload := []byte("cancelled after the append")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			wrote := make(chan writeResult, 1)
			go func() {
				v, err := c.WriteAt(ctx, s, f, 0, payload, 1)
				wrote <- writeResult{v, err}
			}()
			within(t, reached, "the write to reach the leg's park")
			cancel()
			unPark()

			got := within(t, wrote, "the write cancelled after its record was appended")
			if got.err != nil {
				t.Fatalf("the WriteAt cancelled after its record was appended returned %v, want its version", got.err)
			}
			if got.v == (core.Version{}) {
				t.Fatal("the WriteAt cancelled after its record was appended returned no version")
			}
			if rec := recordOf(t, c, f, 0); rec.Version != got.v {
				t.Fatalf("the piece the index holds carries version %v, want the returned %v", rec.Version, got.v)
			}
			p := make([]byte, len(payload))
			n, missing, _, _, err := c.ReadAt(context.Background(), s, f, 0, p)
			if err != nil || n != len(payload) || len(missing) != 0 || !bytes.Equal(p, payload) {
				t.Fatalf("the read of the cancelled write = %d %q missing %v (%v), want %q", n, p, missing, err, payload)
			}
		})
	}
}

// TestRemovalWaitsWithoutContext checks the completeness rule's half a removal carries: a
// Delete whose context is cancelled while its durability wait runs still
// waits — the wait takes no context, so a cancelled ctx after the record's
// append returns the removal's own answer, never the context's error — and
// the removal returns the version its record drew.
//
// The mutation it catches: a wait made context-aware — one that answers the
// cancelled caller with the context's error instead of sitting on the held
// sync — returns while the hold below keeps its sync parked, and fails the
// bounded wait that is the assertion.
func TestRemovalWaitsWithoutContext(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// The first write lands the file's stream on its segment and the pool
	// re-readies its spares, so the delete's header-only record rolls no
	// segment and triggers no pool refill: its record's sync is the first
	// the core makes after the hold is armed, so the hold cannot stop a
	// refill's spare sync instead.
	warm(t, c, s, f)
	hold := seam.HoldFirst(t, kit.OpSync, "*")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan writeResult, 1)
	go func() {
		v, err := c.Delete(ctx, s, f)
		done <- writeResult{v, err}
	}()
	within(t, hold.Reached(), "the delete's record sync to reach storage")
	cancel()
	// The bounded wait is the assertion, not a sleep before one: the removal
	// must not return while its sync is held, whatever the context says, so
	// a wait that consulted the context would answer here and fail. The
	// window is generous because the mutant answers at once; the call under
	// test never does, and pays the window once.
	select {
	case got := <-done:
		t.Fatalf("the delete returned (%v, %v) after the ctx was cancelled but while its record's sync was still held", got.v, got.err)
	case <-time.After(250 * time.Millisecond):
	}
	if hold.Waiting() < 1 {
		t.Fatal("no call waits on the held sync: the delete left its durability wait instead")
	}
	hold.Release()
	got := within(t, done, "the delete whose ctx was cancelled during its wait")
	if got.err != nil {
		t.Fatalf("the delete = %v, want its version, not the context's error", got.err)
	}
	rec := removalOf(t, seam, format.KindDelete)
	if got.v == (core.Version{}) || rec.Header.Version != got.v {
		t.Fatalf("the delete returned %v, want the version its record carries %v", got.v, rec.Header.Version)
	}
}

// TestAppendDomainWaitNotAbandoned checks the un-abandoned domain wait: a write waiting on its file's
// append domain is not abandoned when its context is done — the domain's
// acquisition is a plain wait, and once the holder releases, the cancelled
// waiter runs to its own result and returns its own version.
//
// The mutations the legs catch: a domain acquisition that answered a done
// context — a check of ctx.Err() after the acquisition returns — has the
// waiter return the context's error instead of its version and fails the
// version assertion below; a waiter that is never released trips the
// watchdog inside within, so the test fails rather than hangs.
func TestAppendDomainWaitNotAbandoned(t *testing.T) {
	t.Run("one file, two writes", func(t *testing.T) {
		parked := make(chan struct{})
		release := make(chan struct{})
		var parkN atomic.Int32
		c, seam := newTestCore(t, func(o *coreOptions) {
			// The parked write is the only one inside the domain — the
			// second blocks on acquiring it, below the seam — so the seam
			// is arming for the first arrival alone; the second write's
			// own arrival passes through.
			o.beforeAppend = func() {
				if parkN.Add(1) == 1 {
					close(parked)
					<-release
				}
			}
		})
		s := mustShare(t, c, 1)
		f := testFile(1)
		tbl, ok := c.domains.(*fileLocks)
		if !ok {
			t.Fatal("the test core's domain table is not the file append table")
		}
		stop := releaser(t, release)

		first, second := []byte("first"), []byte("second")
		w1 := make(chan writeResult, 1)
		go func() {
			v, err := c.WriteAt(context.Background(), s, f, 0, first, 1)
			w1 <- writeResult{v, err}
		}()
		within(t, parked, "the first write to park before its append")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w2 := make(chan writeResult, 1)
		go func() {
			v, err := c.WriteAt(ctx, s, f, int64(len(first)), second, 1)
			w2 <- writeResult{v, err}
		}()
		// A waiter counts itself in its file's entry the moment it asks for
		// the domain, so two refs say the second write is on it, waiting.
		eventually(t, "the second write to wait on the file's append domain", func() bool {
			return tbl.refs(f) >= 2
		})
		cancel()
		// The bounded wait is the assertion, not a sleep before one: the
		// cancelled waiter must not return while the first write still holds
		// the domain, so an abandoned wait — one that answered the done
		// context — returns here and fails. The window is generous because
		// the mutant answers at once; the call under test never does, and
		// pays the window once.
		select {
		case got := <-w2:
			t.Fatalf("the cancelled write returned (%v, %v) while the first write still held the file's append domain", got.v, got.err)
		case <-time.After(250 * time.Millisecond):
		}
		stop()

		got1 := within(t, w1, "the parked first write")
		got2 := within(t, w2, "the cancelled second write")
		if got1.err != nil || got2.err != nil {
			t.Fatalf("the two writes of one file = (%v, %v) and (%v, %v), want no error", got1.v, got1.err, got2.v, got2.err)
		}
		if got2.v == (core.Version{}) {
			t.Fatal("the cancelled write returned no version: its wait on the domain was abandoned")
		}
		var onDisk bool
		for _, r := range fileWrites(t, seam, f) {
			if r.Header.Version == got2.v && r.Header.Extent.Off == int64(len(first)) {
				onDisk = true
			}
		}
		if !onDisk {
			t.Fatalf("no record of the cancelled write at %d under version %v is on disk", len(first), got2.v)
		}
		want := append(append([]byte{}, first...), second...)
		p := make([]byte, len(want))
		n, missing, _, _, err := c.ReadAt(context.Background(), s, f, 0, p)
		if err != nil || n != len(p) || len(missing) != 0 || !bytes.Equal(p, want) {
			t.Fatalf("the read of both writes = %d %q missing %v (%v), want %q", n, p, missing, err, want)
		}
	})

	t.Run("two files, two streams, two shares", func(t *testing.T) {
		// The race envelope: both shares' writers running, one cancellation
		// per file. Each file's domain is held by its own parked write and
		// its second write waits cancelled on it; neither wait is abandoned,
		// so both waiters return their own versions once the holders
		// release.
		parked := make(chan struct{}, 2)
		release := make(chan struct{})
		var parkN atomic.Int32
		c, seam := newTestCore(t, func(o *coreOptions) {
			o.beforeAppend = func() {
				// Only the two park-holding writes reach the seam before the
				// release: each waiter is blocked below the domain, and
				// every arrival after the release is one of the waiters,
				// which pass through.
				if parkN.Add(1) <= 2 {
					parked <- struct{}{}
					<-release
				}
			}
		})
		s0, s1 := mustShare(t, c, 1), mustShare(t, c, 2)
		a := testFile(1)
		b := otherStreamFile(c, a)
		tbl, ok := c.domains.(*fileLocks)
		if !ok {
			t.Fatal("the test core's domain table is not the file append table")
		}
		stop := releaser(t, release)

		type pair struct {
			s      *Share
			id     core.FileID
			first  []byte
			second []byte
			w1     chan writeResult
			w2     chan writeResult
			ctx    context.Context
			cancel context.CancelFunc
		}
		pairs := []pair{
			{s: s0, id: a, first: []byte("A-first"), second: []byte("A-second")},
			{s: s1, id: b, first: []byte("B-first"), second: []byte("B-second")},
		}
		for i := range pairs {
			pairs[i].w1 = make(chan writeResult, 1)
			pairs[i].w2 = make(chan writeResult, 1)
			pairs[i].ctx, pairs[i].cancel = context.WithCancel(context.Background())
		}
		defer func() {
			for i := range pairs {
				pairs[i].cancel()
			}
		}()

		for i := range pairs {
			go func(i int) {
				v, err := c.WriteAt(context.Background(), pairs[i].s, pairs[i].id, 0, pairs[i].first, 1)
				pairs[i].w1 <- writeResult{v, err}
			}(i)
		}
		for range 2 {
			within(t, parked, "a first write to park before its append")
		}
		for i := range pairs {
			go func(i int) {
				v, err := c.WriteAt(pairs[i].ctx, pairs[i].s, pairs[i].id, int64(len(pairs[i].first)), pairs[i].second, 1)
				pairs[i].w2 <- writeResult{v, err}
			}(i)
		}
		eventually(t, "both cancelled writes to wait on their files' append domains", func() bool {
			return tbl.refs(a) >= 2 && tbl.refs(b) >= 2
		})
		for i := range pairs {
			pairs[i].cancel()
		}
		// The bounded wait is the assertion, not a sleep before one: a wait
		// abandoned by its context returns here and fails.
		for i := range pairs {
			select {
			case got := <-pairs[i].w2:
				t.Fatalf("file %d's cancelled write returned (%v, %v) while its first write still held the file's append domain", i, got.v, got.err)
			case <-time.After(250 * time.Millisecond):
			}
		}
		stop()

		for i := range pairs {
			got1 := within(t, pairs[i].w1, "the parked first write")
			got2 := within(t, pairs[i].w2, "the cancelled second write")
			if got1.err != nil || got2.err != nil {
				t.Fatalf("file %d's two writes = (%v, %v) and (%v, %v), want no error", i, got1.v, got1.err, got2.v, got2.err)
			}
			if got2.v == (core.Version{}) {
				t.Fatalf("file %d's cancelled write returned no version: its wait on the domain was abandoned", i)
			}
			var onDisk bool
			for _, r := range fileWrites(t, seam, pairs[i].id) {
				if r.Header.Version == got2.v {
					onDisk = true
				}
			}
			if !onDisk {
				t.Fatalf("file %d has no record of its cancelled write under version %v", i, got2.v)
			}
			want := append(append([]byte{}, pairs[i].first...), pairs[i].second...)
			p := make([]byte, len(want))
			n, missing, _, _, err := c.ReadAt(context.Background(), pairs[i].s, pairs[i].id, 0, p)
			if err != nil || n != len(p) || len(missing) != 0 || !bytes.Equal(p, want) {
				t.Fatalf("file %d's read of both writes = %d %q missing %v (%v), want %q", i, n, p, missing, err, want)
			}
		}
	})
}
