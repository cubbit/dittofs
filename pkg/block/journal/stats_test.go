package journal

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
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
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 50 {
				_ = j.Stats()
			}
		}()
	}
	close(start)
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

// TestStatsFromDistinct hands the conversion a core Stats whose every value is
// different, so an assignment to the wrong public field fails, and checks the
// result shares no slice or map with its input.
func TestStatsFromDistinct(t *testing.T) {
	at := time.Unix(1700000000, 0)
	boom := errors.New("sync boom")
	ro := errors.New("read-only cause")
	in := datapath.Stats{
		MaxBytes: 1, UsedBytes: 2, ReservedBytes: 3, HeldBytes: 4, DirtyBytes: 5,
		OldestDirty: at, Files: 6, ExtentCount: 7, DropRanges: 8, Segments: 9, Sealed: 10, Spares: 23,
		UnreclaimedBytes: 11, HeadroomBytes: 12, RepackReserveBytes: 13, PinnedSegments: 14,
		OpenDescriptors: 15, DescriptorLimit: 16, DamagedSegments: 17,
		LastSyncError:  &durability.SyncFailure{Err: boom, At: at.Add(time.Second)},
		SyncDuration:   durability.Histogram{Bounds: []time.Duration{1, 2}, Counts: []uint64{3, 4, 5}, Sum: 6},
		RemovalMarkers: 18, VersionLimit: Version{Counter: 19}, VersionsLeft: 20, NextVersion: Version{Counter: 21},
		ExtentLimit: 22,
		Counters: datapath.Counters{
			WrittenBytes: 101, FilledBytes: 102, FillRefused: 103, OfferedBytes: 104, MarkedBytes: 105,
			ReleasedBytes: 106, FreedBytes: 107, RepackedBytes: 108, ReservationRefused: 109, Syncs: 110,
			SyncFailures: 111, SyncReappendedBytes: 112, HeadroomDraws: 113, CorruptExtentsDataLoss: 114,
			CorruptExtentsRepairable: 115, StaleExtents: 116, SupersededExtents: 117, TornTails: 118,
			ScanResyncs: 119, IndexRefusals: 120, SegmentReopens: 121, SpareRefusals: 122, DoubleRetires: 123,
		},
		Losses:         []LossEvent{{SeqNum: 1, Share: 7, Extent: Extent{Off: 8, Len: 9}}, {SeqNum: 2}},
		LossGeneration: 24,
		TagConflicts:   []index.Conflict{{File: NewFileID([16]byte{1}), Tags: [2]ShareTag{3, 4}}},
		Shares:         map[ShareTag]datapath.ShareStats{5: {Limit: 31, UsedBytes: 32, ReservedBytes: 33, HeldBytes: 34, DirtyBytes: 35, UnreclaimedBytes: 36}},
		ReadOnly:       ReadOnlyState{ReadOnly: true, Err: ro, SpareMade: true, Check: boom},
	}
	got := statsFrom(in)

	want := Stats{
		MaxBytes: 1, UsedBytes: 2, ReservedBytes: 3, HeldBytes: 4, DirtyBytes: 5,
		OldestDirty: at, Files: 6, ExtentCount: 7, DropRanges: 8, Segments: 9, Sealed: 10, Spares: 23,
		UnreclaimedBytes: 11, HeadroomBytes: 12, RepackReserveBytes: 13, PinnedSegments: 14,
		OpenDescriptors: 15, DescriptorLimit: 16, DamagedSegments: 17,
		LastSyncError:  &SyncFailure{Err: boom, At: at.Add(time.Second)},
		SyncDuration:   Histogram{Bounds: []time.Duration{1, 2}, Counts: []uint64{3, 4, 5}, Sum: 6},
		RemovalMarkers: 18, VersionLimit: Version{Counter: 19}, VersionsLeft: 20, NextVersion: Version{Counter: 21},
		ExtentLimit: 22,
		Counters: Counters{
			WrittenBytes: 101, FilledBytes: 102, FillRefused: 103, OfferedBytes: 104, MarkedBytes: 105,
			ReleasedBytes: 106, FreedBytes: 107, RepackedBytes: 108, ReservationRefused: 109, Syncs: 110,
			SyncFailures: 111, SyncReappendedBytes: 112, HeadroomDraws: 113, CorruptExtentsDataLoss: 114,
			CorruptExtentsRepairable: 115, StaleExtents: 116, SupersededExtents: 117, TornTails: 118,
			ScanResyncs: 119, IndexRefusals: 120, SegmentReopens: 121, SpareRefusals: 122, DoubleRetires: 123,
		},
		Losses:         []LossEvent{{SeqNum: 1, Share: 7, Extent: Extent{Off: 8, Len: 9}}, {SeqNum: 2}},
		LossGeneration: 24,
		TagConflicts:   []TagConflict{{File: NewFileID([16]byte{1}), Tags: [2]ShareTag{3, 4}}},
		Shares:         map[ShareTag]ShareStats{5: {Limit: 31, UsedBytes: 32, ReservedBytes: 33, HeldBytes: 34, DirtyBytes: 35, UnreclaimedBytes: 36}},
		ReadOnly:       ReadOnlyState{ReadOnly: true, Err: ro, SpareMade: true, Check: boom},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statsFrom =\n%+v\nwant\n%+v", got, want)
	}

	// Recovery is a field no core figure feeds, so it stays zero.
	if got.Recovery != (RecoveryStats{}) {
		t.Errorf("Recovery = %+v, want zero", got.Recovery)
	}

	// The result is the caller's: changing it leaves the input as it was.
	got.SyncDuration.Bounds[0], got.SyncDuration.Counts[0] = 99, 99
	got.Losses[0].SeqNum = 99
	got.TagConflicts[0].Tags[0] = 99
	got.LastSyncError.Err = nil
	got.Shares[5] = ShareStats{}
	delete(got.Shares, 5)
	if in.SyncDuration.Bounds[0] != 1 || in.SyncDuration.Counts[0] != 3 || in.Losses[0].SeqNum != 1 ||
		in.TagConflicts[0].Tags[0] != 3 || in.LastSyncError.Err != boom || len(in.Shares) != 1 || in.Shares[5].Limit != 31 {
		t.Fatalf("changing the converted Stats reached the input: %+v", in)
	}
}
