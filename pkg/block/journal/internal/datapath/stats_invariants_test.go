package datapath

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// checkNamed reports a sample of Stats that breaks a documented inequality.
func checkNamed(t *testing.T, when string, s Stats) {
	t.Helper()
	if s.DirtyBytes > s.HeldBytes {
		t.Errorf("%s: DirtyBytes %d > HeldBytes %d", when, s.DirtyBytes, s.HeldBytes)
	}
	if s.ReservedBytes < 0 {
		t.Errorf("%s: ReservedBytes = %d, want >= 0", when, s.ReservedBytes)
	}
}

// TestRFC1D_NamedInequalitiesHold samples Stats in a tight loop while writers,
// truncates and removals run, between Account's counter updates and between a
// record's append and its publish, and every sample keeps the inequalities.
func TestRFC1D_NamedInequalitiesHold(t *testing.T) {
	var c *Core
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.stepSpy = func(step int) { checkNamed(t, fmt.Sprintf("Account step %d", step), c.Stats()) }
		o.afterAppend = func() { checkNamed(t, "after append", c.Stats()) }
	})
	s := mustShare(t, c, 1)
	ctx := context.Background()

	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			checkNamed(t, "tight loop", c.Stats())
		}
	}()

	var workers sync.WaitGroup
	for w := byte(1); w <= 4; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			f := testFile(w)
			for i := 0; i < 60; i++ {
				if err := retryRefused(func() error {
					_, err := c.WriteAt(ctx, s, f, int64(i%5)*4096, make([]byte, 4096+i*64), 1)
					return err
				}, core.LimitSpare); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
				switch i % 7 {
				case 3:
					if err := retryRefused(func() error { _, err := c.Truncate(ctx, s, f, 4096); return err }, core.LimitSpare, core.LimitHeadroom); err != nil {
						t.Errorf("Truncate: %v", err)
						return
					}
				case 6:
					if err := retryRefused(func() error { _, err := c.Delete(ctx, s, f); return err }, core.LimitSpare, core.LimitHeadroom); err != nil {
						t.Errorf("Delete: %v", err)
						return
					}
				}
				if i%10 == 9 {
					syncFiles(t, c, f)
				}
			}
		}()
	}
	workers.Wait()
	close(stop)
	sampler.Wait()
	// The testable form of the second inequality: held bytes live in allocated
	// segments. It is checked quiescent because held is read before used, and
	// a retire can lower used in between.
	if st := c.Stats(); st.HeldBytes > st.UsedBytes {
		t.Errorf("quiescent: HeldBytes %d > UsedBytes %d: held bytes live in allocated segments", st.HeldBytes, st.UsedBytes)
	}
}

// TestRFC1D_DirtyUnderHeldWhenAFallLandsBetweenTheReads lands a whole fall of
// held and dirty bytes between Stats' dirty and held reads: the pair it
// returns still has dirty at most held.
func TestRFC1D_DirtyUnderHeldWhenAFallLandsBetweenTheReads(t *testing.T) {
	var c *Core
	var once sync.Once
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.betweenHeldReads = func() {
			once.Do(func() {
				c.Account(index.HeldDelta{Tag: 7, File: testFile(9), HeldBytes: -4096, DirtyBytes: -4096})
			})
		}
	})
	c.Account(index.HeldDelta{Tag: 7, File: testFile(9), HeldBytes: 4096, DirtyBytes: 4096})
	st := c.Stats()
	if st.DirtyBytes > st.HeldBytes {
		t.Fatalf("DirtyBytes %d > HeldBytes %d after a fall landed between the reads", st.DirtyBytes, st.HeldBytes)
	}
}

// TestRFC1D_SparesAreThePoolsReadySpares checks Spares reports the pool's
// ready content spares, each counted in UsedBytes at the segment cap.
func TestRFC1D_SparesAreThePoolsReadySpares(t *testing.T) {
	c, _ := newTestCore(t)
	st := c.Stats()
	if want := int(c.pool.Counters().Spares); st.Spares != want || st.Spares == 0 {
		t.Fatalf("Spares = %d, want the pool's %d (non-zero)", st.Spares, want)
	}
	if st.UsedBytes < int64(st.Spares)*testSegmentCap {
		t.Fatalf("UsedBytes %d does not cover %d spares at %d", st.UsedBytes, st.Spares, testSegmentCap)
	}
}

// TestRFC1D_UpdateOrderKeepsDirtyUnderHeld applies a rise and a fall of held
// and dirty bytes and reads Stats between each pair of counter updates: dirty
// never exceeds held at any of them.
func TestRFC1D_UpdateOrderKeepsDirtyUnderHeld(t *testing.T) {
	var c *Core
	var seen []Stats
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.stepSpy = func(int) { seen = append(seen, c.Stats()) }
	})
	f := testFile(9)
	c.Account(index.HeldDelta{Tag: 7, File: f, HeldBytes: 100, DirtyBytes: 100})
	c.Account(index.HeldDelta{Tag: 7, File: f, HeldBytes: -100, DirtyBytes: -100})
	if len(seen) != 4 {
		t.Fatalf("the spy ran %d times, want 4: two steps for each of two deltas", len(seen))
	}
	for i, st := range seen {
		if st.DirtyBytes > st.HeldBytes {
			t.Errorf("sample %d between counter updates: DirtyBytes %d > HeldBytes %d", i, st.DirtyBytes, st.HeldBytes)
		}
	}
}

// TestRFC1D_StatsTakeNoWriteLock holds a file's append domain and a write
// inside its append, and checks Stats still answers. It proves only that the
// file append domain is not taken: another lock a write holds would not show.
func TestRFC1D_StatsTakeNoWriteLock(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once, closeOnce sync.Once
	open := func() { closeOnce.Do(func() { close(release) }) }
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.afterAppend = func() {
			once.Do(func() { close(reached); <-release })
		}
	})
	// Runs before the core's own cleanup, so a failed test does not leave the
	// held write blocking Close.
	t.Cleanup(open)
	s := mustShare(t, c, 1)

	unlock := c.domains.Lock(testFile(1))
	defer unlock()
	written := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, testFile(2), 0, make([]byte, 4096), 1)
		written <- err
	}()
	<-reached

	got := make(chan Stats, 1)
	go func() { got <- c.Stats() }()
	select {
	case st := <-got:
		if st.ReservedBytes < 0 {
			t.Fatalf("ReservedBytes = %d", st.ReservedBytes)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Stats did not return while a file's append domain and a write mid-append were held")
	}
	open()
	if err := <-written; err != nil {
		t.Fatalf("the held write: %v", err)
	}
}

// TestRFC1D_StatsCostIsFlat compares the allocations of a Stats call over a
// journal ten times the size in extents and segments: they do not grow. The
// allocation count is a proxy: it cannot see time spent walking a structure
// that allocates nothing.
func TestRFC1D_StatsCostIsFlat(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector inflates allocation counts")
	}
	grow := func(c *Core, s *Share, files int) {
		for i := 0; i < files; i++ {
			f := testFile(byte(1 + i%200))
			base := int64(i/200) * 1 << 20
			for k := int64(0); k < 8; k++ {
				mustWrite(t, c, s, f, base+k*2048, make([]byte, 512))
			}
		}
	}
	allocs := func(c *Core) float64 { return testing.AllocsPerRun(50, func() { _ = c.Stats() }) }

	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	grow(c, s, 20)
	small, smallSeg, smallExt := allocs(c), c.Stats().Segments, c.Stats().ExtentCount
	grow(c, s, 200)
	big, bigSeg, bigExt := allocs(c), c.Stats().Segments, c.Stats().ExtentCount
	if bigExt < 5*smallExt {
		t.Fatalf("extents %d -> %d: the journal did not grow tenfold", smallExt, bigExt)
	}
	t.Logf("extents %d -> %d, segments %d -> %d, allocs per Stats %.0f -> %.0f", smallExt, bigExt, smallSeg, bigSeg, small, big)
	if big != small {
		t.Fatalf("allocations per Stats = %.0f over %d extents, %.0f over %d: want the same", big, bigExt, small, smallExt)
	}
}

// TestRFC1D_WedgeIsVisibleDirty fills a bounded journal with unsynced bytes
// until it refuses, and one Stats shows the held bytes all dirty, near the
// bytes used, and the share holding them.
func TestRFC1D_WedgeIsVisibleDirty(t *testing.T) {
	c, _ := newTestCore(t, capacityCoreOption(5*testSegmentCap))
	holder := mustShare(t, c, 1)
	mustShare(t, c, 2)
	var err error
	for i := 0; err == nil && i < 1000; i++ {
		err = retryRefused(func() error {
			_, err := c.WriteAt(context.Background(), holder, testFile(byte(1+i%50)), int64(i/50)*4096, make([]byte, 4096), 1)
			return err
		}, core.LimitSpare)
	}
	_ = refused(t, err, core.LimitJournal)

	st := c.Stats()
	if st.DirtyBytes != st.HeldBytes || st.HeldBytes == 0 {
		t.Fatalf("DirtyBytes %d, HeldBytes %d: want equal and non-zero", st.DirtyBytes, st.HeldBytes)
	}
	// What is not held is each segment's header and padding and the pool's
	// spare segments.
	if st.HeldBytes < st.UsedBytes/2 || st.HeldBytes > st.UsedBytes {
		t.Fatalf("HeldBytes %d, UsedBytes %d: want held within half of used", st.HeldBytes, st.UsedBytes)
	}
	if got := st.Shares[1]; got.DirtyBytes != st.DirtyBytes || got.HeldBytes != st.HeldBytes {
		t.Fatalf("share 1 = %+v, want it named as holding the %d dirty bytes", got, st.DirtyBytes)
	}
	if got := st.Shares[2]; got.DirtyBytes != 0 || got.HeldBytes != 0 {
		t.Fatalf("idle share 2 = %+v, want no dirty bytes", got)
	}
}

// TestRFC1D_CleanBytesCountAsOccupancy checks bytes held but already
// offloaded count in UsedBytes and HeldBytes and not in DirtyBytes.
func TestRFC1D_CleanBytesCountAsOccupancy(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	if err := c.Fill(context.Background(), s, testFile(3), 0, bytes.Repeat([]byte{1}, 4096), ver(0), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	st := c.Stats()
	if st.HeldBytes != 4096 || st.DirtyBytes != 0 || st.UsedBytes < 4096 {
		t.Fatalf("HeldBytes %d, DirtyBytes %d, UsedBytes %d: want 4096, 0, at least 4096", st.HeldBytes, st.DirtyBytes, st.UsedBytes)
	}
	if got := st.Shares[1]; got.HeldBytes != 4096 || got.DirtyBytes != 0 || got.UsedBytes < 4096 {
		t.Fatalf("share 1 = %+v, want held 4096, dirty 0, used at least 4096", got)
	}
}

// indexTruth sums what the index holds for the shares' files: held bytes,
// held bytes not offloaded, and retained bytes, per share.
func indexTruth(c *Core, tags ...core.ShareTag) map[core.ShareTag]shareHeld {
	out := map[core.ShareTag]shareHeld{}
	for _, tag := range tags {
		var h shareHeld
		for _, f := range c.Index().Files(tag) {
			for _, e := range c.Index().FileEntries(f) {
				switch e.Kind {
				case index.EntryHeld:
					h.Held += e.Extent.Len
					if !e.Offloaded {
						h.Dirty += e.Extent.Len
					}
				case index.EntryRetained:
					h.Retained += e.Extent.Len
				}
			}
		}
		out[tag] = h
	}
	return out
}

// statsDiff returns how Stats' journal and share figures differ from what the
// index holds, or "" when they agree.
func statsDiff(c *Core, tag core.ShareTag) string {
	truth := indexTruth(c, tag)[tag]
	st := c.Stats()
	sh := st.Shares[tag]
	_, _, ret := c.heldBytes()
	if st.HeldBytes != truth.Held || st.DirtyBytes != truth.Dirty || ret != truth.Retained ||
		sh.HeldBytes != truth.Held || sh.DirtyBytes != truth.Dirty {
		return fmt.Sprintf("Stats held/dirty %d/%d (retained counter %d), share %d/%d; index held/dirty/retained %+v",
			st.HeldBytes, st.DirtyBytes, ret, sh.HeldBytes, sh.DirtyBytes, truth)
	}
	return ""
}

// wantStatsMatchIndexNow fails unless Stats equals the index at once. Account
// is synchronous with the write or the sync that returned, so a path that
// completed before this call has no lag to wait out.
func wantStatsMatchIndexNow(t *testing.T, c *Core, when string, tag core.ShareTag) {
	t.Helper()
	if d := statsDiff(c, tag); d != "" {
		t.Fatalf("%s: %s", when, d)
	}
}

// wantStatsMatchIndex waits until Stats equals the index and fails with the
// last difference if it never does. It polls only where the figure lags: a
// window's failure and a corrupt read's drop are applied by a worker after the
// call that found them returned.
func wantStatsMatchIndex(t *testing.T, c *Core, when string, tag core.ShareTag) {
	t.Helper()
	var diff string
	deadline := time.Now().Add(10 * time.Second)
	for {
		if diff = statsDiff(c, tag); diff == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", when, diff)
		}
		time.Sleep(time.Millisecond)
	}
}

// A sync window that fails drops the write's record; the figures follow the
// index, which serves the older write again.
func TestRFC1D_FailedSyncWindowFiguresMatchIndex(t *testing.T) {
	h, c := newHeld(t, true)
	s := mustShare(t, c, 1)
	f := testFile(3)
	h.f = f
	a := mustWrite(t, c, s, f, 0, make([]byte, 4096))
	syncFiles(t, c, f)
	wantStatsMatchIndexNow(t, c, "after the first write synced", 1)
	h.arm()
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	h.open()
	if err := <-h.done; err == nil {
		t.Fatal("Sync returned no error although its sync failed")
	}
	eventually(t, "the older write to be served again", func() bool {
		p, ok := c.Index().Lookup(f, 0)
		return ok && p.Version == a
	})
	wantStatsMatchIndex(t, c, "after the failed window", 1)
	if st := c.Stats(); st.HeldBytes != 4096 || st.DirtyBytes != 4096 {
		t.Fatalf("held/dirty = %d/%d, want the older write's 4096 each", st.HeldBytes, st.DirtyBytes)
	}
}

// A sync that completes lifts the retained extent a newer write left behind,
// and the figures follow.
func TestRFC1D_SyncLiftingRetainedFiguresMatchIndex(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	wantStatsMatchIndexNow(t, c, "with the older write retained", 1)
	if _, _, r := c.heldBytes(); r != 4096 {
		t.Fatalf("retained = %d, want 4096 before the sync", r)
	}
	syncFiles(t, c, f)
	wantStatsMatchIndexNow(t, c, "after the sync lifted it", 1)
	if st := c.Stats(); st.HeldBytes != 4096 || st.DirtyBytes != 4096 {
		t.Fatalf("held/dirty = %d/%d, want 4096 each", st.HeldBytes, st.DirtyBytes)
	}
}

// A read that finds a record that does not verify drops it, and the figures
// follow the index.
func TestRFC1D_CorruptReadFiguresMatchIndex(t *testing.T) {
	c, seam := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, pattern(1<<20, 5), 1); err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	wantStatsMatchIndexNow(t, c, "before the corruption", 1)
	r := recordOf(t, c, f, 0).Record
	flip(t, seam, r.Segment, payloadStart(r)+1<<20-1)
	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
	wantStatsMatchIndex(t, c, "after the drop", 1)
	if st := c.Stats(); st.HeldBytes != 0 || st.DirtyBytes != 0 {
		t.Fatalf("held/dirty = %d/%d after the only record was dropped, want 0", st.HeldBytes, st.DirtyBytes)
	}
}
