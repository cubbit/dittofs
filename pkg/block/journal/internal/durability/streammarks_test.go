package durability

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// wantMarks fails the test unless the figure is exactly want, ordered by stream.
func wantMarks(t *testing.T, what string, d *Durability, want ...core.SyncMark) {
	t.Helper()
	if got := d.Marks(); !slices.Equal(got, want) {
		t.Fatalf("%s: Marks() = %+v, want %+v", what, got, want)
	}
}

// TestMarksFollowEachSuccessfulSync covers the figure of a stream's last
// completed sync: each Sync that returns a mark leaves it as the stream's
// entry, and a later one replaces it.
func TestMarksFollowEachSuccessfulSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f, g := file(1), file(2)
	wantMarks(t, "before any sync", r.d)

	r.publish(t, f, 0, "aaaa")
	a, err := r.d.Sync(context.Background(), f)
	if err != nil || len(a) != 1 {
		t.Fatalf("the first Sync = %+v, %v, want one mark", a, err)
	}
	wantMarks(t, "after the first sync", r.d, a[0])

	r.publish(t, g, 0, "gggg")
	b, err := r.d.Sync(context.Background(), g)
	if err != nil || len(b) != 1 {
		t.Fatalf("the second Sync = %+v, %v, want one mark", b, err)
	}
	if !markNewer(b[0], a[0]) {
		t.Fatalf("the second mark %+v is not newer than the first %+v", b[0], a[0])
	}
	wantMarks(t, "after the second sync", r.d, b[0])
}

// TestMarksIgnoreAFailedSync covers a window that failed: the Sync answers a
// SyncError with no marks, and the figure stays what the open seeded.
func TestMarksIgnoreAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})
	fr := newFailRig(t, nil)
	seed := core.SyncMark{Stream: 0, Segment: 1, Through: 100, Offset: 90, Sequence: 3}
	fr.d.SeedMarks([]core.SyncMark{seed})
	f := file(1)
	loc, _ := fr.publish(t, f, 0, "aaaa")

	fr.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	wantSyncError(t, mustReturn(t, "Sync", fr.syncAsync(context.Background(), f)), loc.Segment, f)
	wantMarks(t, "after the failed sync", fr.d, seed)
}

// TestSeedMarksIsACopyAndRaiseNeverLowers covers the figure's own rules: the
// seed is copied both ways, a mark naming no later record than its stream's
// entry changes nothing, a later one replaces it, whatever its segment's id, and a stream with no entry gains one.
func TestSeedMarksIsACopyAndRaiseNeverLowers(t *testing.T) {
	d := &Durability{}
	m0 := core.SyncMark{Stream: 0, Segment: 5, Through: 100, Offset: 90, Sequence: 7}
	seed := []core.SyncMark{m0}
	d.SeedMarks(seed)
	seed[0].Sequence = 99
	got := d.Marks()
	got[0].Sequence = 98
	wantMarks(t, "a seed and a read are copies", d, m0)

	for _, c := range []struct {
		name string
		m    core.SyncMark
		want core.SyncMark
	}{
		{"an earlier record", core.SyncMark{Stream: 0, Segment: 9, Through: 900, Offset: 800, Sequence: 6}, m0},
		{"the same mark", m0, m0},
		{"the same record, its copy in an earlier segment", core.SyncMark{Stream: 0, Segment: 4, Through: 900, Offset: 800, Sequence: 7}, m0},
		{"the same record, synced through less", core.SyncMark{Stream: 0, Segment: 5, Through: 50, Offset: 40, Sequence: 7}, m0},
		{"the same record, synced through more", core.SyncMark{Stream: 0, Segment: 5, Through: 200, Offset: 190, Sequence: 7}, core.SyncMark{Stream: 0, Segment: 5, Through: 200, Offset: 190, Sequence: 7}},
		{"a later record in a lower segment", core.SyncMark{Stream: 0, Segment: 3, Through: 10, Offset: 9, Sequence: 8}, core.SyncMark{Stream: 0, Segment: 3, Through: 10, Offset: 9, Sequence: 8}},
	} {
		d.RaiseMarks([]core.SyncMark{c.m})
		wantMarks(t, c.name, d, c.want)
	}
	other := core.SyncMark{Stream: 1, Segment: 2, Through: 5, Offset: 4, Sequence: 2}
	d.RaiseMarks([]core.SyncMark{other})
	if got := d.Marks(); len(got) != 2 || got[1] != other {
		t.Fatalf("a mark of a stream with no entry: Marks() = %+v, want it added after stream 0's", got)
	}
}

// TestMarksNeverFallUnderConcurrentSyncs covers syncs of one stream racing:
// each round, eight files are synced by eight goroutines at once while a
// reader polls the figure; no read is older than the one before, and the
// figure ends at the newest mark any call returned.
func TestMarksNeverFallUnderConcurrentSyncs(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	const files, rounds = 8, 5
	var newest core.SyncMark
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var last core.SyncMark
		for {
			select {
			case <-stop:
				return
			default:
			}
			if m := r.d.Marks(); len(m) == 1 {
				if markNewer(last, m[0]) {
					t.Errorf("the figure fell from %+v to %+v", last, m[0])
					return
				}
				last = m[0]
			}
		}
	}()
	for round := range rounds {
		for i := range files {
			r.publish(t, file(byte(1+i)), int64(round)*4, "dddd")
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := range files {
			wg.Add(1)
			go func() {
				defer wg.Done()
				marks, err := r.d.Sync(context.Background(), file(byte(1+i)))
				if err != nil {
					t.Errorf("Sync of file %d: %v", i, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, m := range marks {
					if markNewer(m, newest) {
						newest = m
					}
				}
			}()
		}
		wg.Wait()
	}
	close(stop)
	<-readerDone
	wantMarks(t, "after the concurrent syncs", r.d, newest)
}
