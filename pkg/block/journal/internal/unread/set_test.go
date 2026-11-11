package unread

import (
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

func TestTwoRecordsKeepSegmentSuspect(t *testing.T) {
	s := New(kit.NewStopwatchClock())
	s.mark(Ref{Segment: 7, Off: 0, Len: 10, Kind: Content})
	s.mark(Ref{Segment: 7, Off: 64, Len: 20, Kind: HeaderOnly})

	want := []core.SegmentRange{{Segment: 7, Off: 0, Len: 10}, {Segment: 7, Off: 64, Len: 20}}
	if got := s.Ranges(7); !reflect.DeepEqual(got, want) {
		t.Errorf("Ranges(7) after two marks = %v, want %v", got, want)
	}

	s.clear(Ref{Segment: 7, Off: 0, Len: 10, Kind: Content})
	if got := s.Suspect(7); !got {
		t.Errorf("Suspect(7) with one record still unread = %v, want true", got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() with one record left = %d, want 1", got)
	}

	s.clear(Ref{Segment: 7, Off: 64, Len: 20, Kind: HeaderOnly})
	if got := s.Suspect(7); got {
		t.Errorf("Suspect(7) after clearing both records = %v, want false", got)
	}
	if got := s.SuspectSegments(); got != 0 {
		t.Errorf("SuspectSegments() after clearing both records = %d, want 0", got)
	}
}

func TestMarkTwiceCountsOnce(t *testing.T) {
	s := New(kit.NewStopwatchClock())
	ref := Ref{Segment: 7, Off: 32, Len: 8, Kind: Content}
	s.mark(ref)
	s.mark(ref)

	if got := len(s.Ranges(7)); got != 1 {
		t.Errorf("len(Ranges(7)) after marking the same ref twice = %d, want 1", got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after marking the same ref twice = %d, want 1", got)
	}
}

func TestRetiredDropsEntriesAndGauge(t *testing.T) {
	s := New(kit.NewStopwatchClock())
	seven := Ref{Segment: 7, Off: 0, Len: 10, Kind: Content}
	eight := Ref{Segment: 8, Off: 16, Len: 20, Kind: HeaderOnly}
	s.mark(seven)
	s.mark(eight)

	s.Retired(7)
	if got := s.Unread(seven); got {
		t.Errorf("Unread(segment 7's ref) after Retired(7) = %v, want false", got)
	}
	if got := s.Unread(eight); !got {
		t.Errorf("Unread(segment 8's ref) after Retired(7) = %v, want true", got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after Retired(7) = %d, want 1 (segment 8 unread)", got)
	}

	s.Retired(99)
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after Retired(99) = %d, want 1 (nothing to drop)", got)
	}
	if got := s.Unread(eight); !got {
		t.Errorf("Unread(segment 8's ref) after Retired(99) = %v, want true", got)
	}
}

func TestRangesIsACopy(t *testing.T) {
	s := New(kit.NewStopwatchClock())
	s.mark(Ref{Segment: 7, Off: 0, Len: 10, Kind: Content})
	s.mark(Ref{Segment: 7, Off: 64, Len: 20, Kind: HeaderOnly})

	got := s.Ranges(7)
	got[0].Len = 999
	grown := append(got, core.SegmentRange{})
	grown[1].Len = 777

	want := []core.SegmentRange{{Segment: 7, Off: 0, Len: 10}, {Segment: 7, Off: 64, Len: 20}}
	if again := s.Ranges(7); !reflect.DeepEqual(again, want) {
		t.Errorf("Ranges(7) after mutating an earlier result = %v, want %v", again, want)
	}
}

func TestGaugeMatchesDistinctSegmentsConcurrently(t *testing.T) {
	const goroutines, segments = 8, 16
	s := New(kit.NewStopwatchClock())

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + 1))
			for i := 0; i < 1000; i++ {
				seg := core.SegmentID(1 + rng.Intn(segments))
				ref := Ref{Segment: seg, Off: int64(8 * rng.Intn(32))}
				switch rng.Intn(3) {
				case 0:
					s.mark(ref)
				case 1:
					s.clear(ref)
				case 2:
					s.Suspect(seg)
				}
			}
		}(g)
	}
	wg.Wait()

	want := 0
	for seg := 1; seg <= segments; seg++ {
		if len(s.Ranges(core.SegmentID(seg))) > 0 {
			want++
		}
	}
	if got := s.SuspectSegments(); got != want {
		t.Errorf("SuspectSegments() = %d, want %d distinct segments holding entries", got, want)
	}
}

func TestSuspectSegmentsAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("race build changes allocation counts too much for a comparison")
	}

	s := New(kit.NewStopwatchClock())
	for seg := 0; seg < 100; seg++ {
		for off := int64(0); off < 1000; off++ {
			s.mark(Ref{Segment: core.SegmentID(seg + 1), Off: off * 64, Len: 32})
		}
	}

	allocs := testing.AllocsPerRun(100, func() { _ = s.SuspectSegments() })
	if allocs != 0 {
		t.Errorf("SuspectSegments() allocates %v times per call, want 0", allocs)
	}
}
