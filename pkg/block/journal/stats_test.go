package journal

import (
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestStats_ListsTagsHoldingRecordsWithoutAHandle checks a journal whose share
// handle is closed still lists the tag that holds records, with no limit, and
// the share's figures are the ones its records make.
func TestStats_ListsTagsHoldingRecordsWithoutAHandle(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(5, 1<<20, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	writeReady(t, h, NewFileID([16]byte{1}), "hello")

	attached := j.Stats().Shares[5]
	if attached.Limit != 1<<20 || attached.HeldBytes != 5 || attached.DirtyBytes != 5 || attached.UsedBytes == 0 {
		t.Fatalf("attached share 5 = %+v, want limit 1<<20, held and dirty 5, used > 0", attached)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Handle.Close: %v", err)
	}
	st := j.Stats()
	detached, ok := st.Shares[5]
	if !ok {
		t.Fatalf("Shares = %+v after the handle closed, want tag 5 still listed for its records", st.Shares)
	}
	if detached.Limit != 0 || detached.UsedBytes != attached.UsedBytes {
		t.Fatalf("detached share 5 = %+v, want limit 0 and used %d", detached, attached.UsedBytes)
	}
	if _, ok := st.Shares[0]; ok {
		t.Fatal("Shares lists tag 0")
	}
}

// TestStats_AfterCloseIsZero checks Stats on a closed journal is the zero
// Stats and does not panic, also when it races Close.
func TestStats_AfterCloseIsZero(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	writeReady(t, h, NewFileID([16]byte{1}), "x")

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = j.Stats()
			}
		}()
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if got := j.Stats(); !reflect.DeepEqual(got, Stats{}) {
		t.Fatalf("Stats after Close = %+v, want the zero Stats", got)
	}
}

// TestStats_CountersReadZeroOnAFreshOpen checks a journal that has done
// nothing reports zero counters.
func TestStats_CountersReadZeroOnAFreshOpen(t *testing.T) {
	j := newTestJournal(t)
	if got := j.Stats().Counters; got != (Counters{}) {
		t.Fatalf("Counters on a fresh journal = %+v, want zero", got)
	}
}

// TestStats_ConvertsTheCoresFigures checks the public Stats carries what the
// core's figures say, and that its maps and slices are the caller's.
func TestStats_ConvertsTheCoresFigures(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	writeReady(t, h, NewFileID([16]byte{1}), "hello")

	st := j.Stats()
	if st.Counters.WrittenBytes != 5 || st.HeldBytes != 5 || st.Files != 1 || st.VersionLimit != openLimit {
		t.Fatalf("Stats = written %d held %d files %d limit %v, want 5, 5, 1, %v", st.Counters.WrittenBytes, st.HeldBytes, st.Files, st.VersionLimit, openLimit)
	}
	if len(st.SyncDuration.Counts) != len(st.SyncDuration.Bounds)+1 {
		t.Fatalf("histogram has %d counts for %d bounds, want one more", len(st.SyncDuration.Counts), len(st.SyncDuration.Bounds))
	}
	want := maps.Clone(st.Shares)
	clear(st.Shares)
	st.SyncDuration.Bounds[0] = -1
	again := j.Stats()
	if !maps.Equal(again.Shares, want) || again.SyncDuration.Bounds[0] == -1 {
		t.Fatalf("a caller's change reached the next Stats: shares %+v want %+v", again.Shares, want)
	}
}

// TestStats_NoFieldReportsAnOffloadedBit checks no public statistic is named
// for, or typed as, an extent's offloaded bit: a count of dirty bytes is the
// only way the bit shows.
func TestStats_NoFieldReportsAnOffloadedBit(t *testing.T) {
	for _, ty := range []reflect.Type{reflect.TypeOf(Stats{}), reflect.TypeOf(Counters{}), reflect.TypeOf(ShareStats{})} {
		for i := range ty.NumField() {
			f := ty.Field(i)
			if strings.Contains(strings.ToLower(f.Name), "offloaded") || strings.Contains(strings.ToLower(f.Type.String()), "offloaded") {
				t.Errorf("%s.%s reports an offloaded bit", ty.Name(), f.Name)
			}
		}
	}
}
