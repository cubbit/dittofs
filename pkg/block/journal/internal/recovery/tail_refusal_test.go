package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// wantRefused runs the open over r and holds the three things every refusal
// answers: an error that is core.ErrNotImplemented, text naming the segment
// and the range's exact offsets, and a directory of the same files and bytes.
func wantRefused(t *testing.T, r *rig, id core.SegmentID, off, end int64) {
	t.Helper()
	before := r.snapshot()
	listing := slices.Clone(r.listing())
	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("Run got err %v, want one wrapping core.ErrNotImplemented", err)
	}
	for _, want := range []string{fmt.Sprintf("segment %d", id), fmt.Sprintf("[%d,%d)", off, end)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run's refusal reads %q, want it to name %q", err.Error(), want)
		}
	}
	if got := r.listing(); !slices.Equal(got, listing) {
		t.Errorf("the directory's listing got %v after the refusal, want %v", got, listing)
	}
	for name, b := range before {
		if got := r.fileBytes(name); !bytes.Equal(got, b) {
			t.Errorf("%s changed under a refused open, want its bytes untouched", name)
		}
	}
}

// namingSeg makes the segment that names predecessor pred.
func namingSeg(id, pred core.SegmentID) []byte {
	return raw.SegmentHeader{Identity: journalID, ID: uint64(id), Predecessor: uint64(pred)}.Bytes()
}

func TestSyncedDamageInTheNewestSegmentIsCorruptionNeverATornTail(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	seg := buildSeg(1, syncedRec(200, 0x4100, endA), syncedRec(201, 0x8100, endB), tailRec(202, 0xc100))
	seg[format.SegmentHeaderSize] ^= 0xff // A's record header
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	wantRefused(t, r, 1, format.SegmentHeaderSize, endA)
}

func TestAZeroSlotBelowALaterRecordsSyncedThroughIsCorruption(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, slotSeg(1, func(slot int64) int64 { return slot + 16 }))
	wantRefused(t, r, 1, endA, endA+16)
}

func TestTheRefusedRangeBoundaryOnSyncedThrough(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	t.Run("at the slot's start the slot is torn", func(t *testing.T) {
		st := slotRig(t, func(slot int64) int64 { return slot })
		wantFate(t, st, 2, fateEnded, 0)
		wantDamaged(t, st)
	})
	t.Run("one byte further the open refuses", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, slotSeg(1, func(slot int64) int64 { return slot + 1 }))
		wantRefused(t, r, 1, endA, endA+16)
	})
}

func TestANamedPredecessorTruncatedMidRecordFailsNamingItsMissingEnd(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	full := buildSeg(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
	cut := full[:len(full)-5]
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, cut)
	r.plantSeg(2, namingSeg(2, 1))
	wantRefused(t, r, 1, endA, int64(len(cut)))
}

func TestARecordHeaderRottedInASealedSegmentWithoutACatalogFailsTheOpen(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	seg := buildSeg(1, tailRec(200, 0x4100), tailRec(201, 0x8100), tailRec(202, 0xc100))
	seg = append(seg, raw.SealMarker{Identity: journalID, Segment: 1, Offset: uint64(len(seg))}.Bytes()...)
	seg[endA] ^= 0xff // B's record header
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, seg)
	r.plantSeg(2, namingSeg(2, 1))
	wantRefused(t, r, 1, endA, endB)
}

func TestARottedRecordHeaderWithAVerifyingCatalogIsDroppedNotRefused(t *testing.T) {
	t.Run("a content record's entry stands in", func(t *testing.T) {
		b, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100), tailRec(202, 0xc100))
		b[endAfter(tailRec(200, 0x4100))] ^= 0xff // B's record header
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, b)
		r.plantSeg(2, namingSeg(2, 1))
		st := runTails(t, r)
		if got := planted(st); got != 3 {
			t.Errorf("the index holds %d entries, want 3: the catalog names every record", got)
		}
		if len(st.Tails.Corrupt) != 0 || len(st.Tails.Damaged) != 0 {
			t.Errorf("Corrupt %+v Damaged %v, want neither: nothing is dropped", st.Tails.Corrupt, st.Tails.Damaged)
		}
	})
	t.Run("a clone target's drop is confined", func(t *testing.T) {
		clone := rec(7, func(r *raw.Record) {
			r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
		})
		b, _ := buildSealed(1, clone, tailRec(201, 0x8100))
		b[format.SegmentHeaderSize] ^= 0xff // the clone target's record header
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, b)
		r.plantSeg(2, namingSeg(2, 1))
		st := runTails(t, r)
		if len(st.Tails.Corrupt) != 1 || st.Tails.Corrupt[0].Segment != 1 || !st.Tails.Corrupt[0].Drop.Synced {
			t.Fatalf("Corrupt got %+v, want exactly the clone target of segment 1, synced", st.Tails.Corrupt)
		}
		if got := planted(st); got != 1 {
			t.Errorf("the index holds %d entries, want 1: the neighbour attaches", got)
		}
		if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
		}
	})
}

// rottenMarkerSeg is segment 1 holding two records and the seal's footer and
// trailer, its seal marker's checksum flipped; the trailer names the footer
// exactly past the marker's slot. It returns the bytes and the marker's offset.
func rottenMarkerSeg(flipTrailer bool) ([]byte, int64) {
	b, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
	markerOff := int64(len(b)) - raw.TrailerSize - 2*raw.CatalogEntrySize - format.SealMarkerSize
	b[markerOff+format.SealMarkerSize-1] ^= 0xff
	if flipTrailer {
		b[len(b)-1] ^= 0xff
	}
	return b, markerOff
}

func TestANamedPredecessorWhoseSealMarkerAloneFailsIsDamageNeverARefusal(t *testing.T) {
	b, _ := rottenMarkerSeg(false)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, namingSeg(2, 1))
	st := runTails(t, r)
	if got := planted(st); got != 2 {
		t.Errorf("the index holds %d entries, want 2: every record before the marker verifies", got)
	}
	wantFate(t, st, 1, fateSealed, 0)
	wantDamaged(t, st, 1)

	b, markerOff := rottenMarkerSeg(true)
	r = newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, namingSeg(2, 1))
	wantRefused(t, r, 1, markerOff, int64(len(b)))
}

func TestTheMissingEndRangeOfANamedPredecessorHasTheRfcsBounds(t *testing.T) {
	sealed, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100))
	markerOff := int64(len(sealed)) - raw.TrailerSize - 2*raw.CatalogEntrySize - format.SealMarkerSize
	for _, row := range []struct {
		name string
		size int64
	}{
		{"records end at the file's end", markerOff},
		{"half a marker follows the records", markerOff + 16},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, sealed[:row.size])
			r.plantSeg(2, namingSeg(2, 1))
			wantRefused(t, r, 1, markerOff, row.size)
		})
	}
}

// trailerFailFS fails every read of the trailer of a segment file after the
// first, as a device fails every read over a bad sector it served once.
type trailerFailFS struct {
	storage.FS
	at    int64 // the trailer's offset
	reads *atomic.Int32
	ok    *atomic.Int32 // the trailer reads served before the failing ones start
}

func (f trailerFailFS) Open(name string) (storage.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if kind, _ := format.ClassifyName(name); kind != format.NameSegment {
		return file, nil
	}
	return trailerFailFile{File: file, at: f.at, reads: f.reads, ok: f.ok}, nil
}

type trailerFailFile struct {
	storage.File
	at    int64
	reads *atomic.Int32
	ok    *atomic.Int32
}

func (f trailerFailFile) ReadAt(p []byte, off int64) (int, error) {
	if off == f.at && len(p) == raw.TrailerSize && f.reads.Add(1) > 1 {
		return 0, syscall.EIO
	}
	if off == f.at && len(p) == raw.TrailerSize {
		f.ok.Add(1)
	}
	return f.File.ReadAt(p, off)
}

func TestAReadErrorOnTheExemptionProbeRefusesTheOpenOnTheRange(t *testing.T) {
	b, _ := rottenMarkerSeg(false)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, namingSeg(2, 1))

	deps := r.runDeps()
	ok := new(atomic.Int32)
	deps.FS = trailerFailFS{FS: deps.FS, at: int64(len(b)) - raw.TrailerSize, reads: new(atomic.Int32), ok: ok}
	_, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run with the exemption probe's read failing got err %v, want an UnreadableRangesError", err)
	}
	want := core.SegmentRange{Segment: 1, Off: int64(len(b)) - raw.TrailerSize, Len: raw.TrailerSize}
	if !slices.Contains(ure.Ranges, want) {
		t.Errorf("the refusal names ranges %v, want one to be the trailer %+v", ure.Ranges, want)
	}
	if errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("Run's refusal %v also answers ErrNotImplemented, want the read's range alone", err)
	}
	// The catalog phase's read of the trailer is the only one served, so the
	// failing read is the exemption probe's own.
	if got := ok.Load(); got != 1 {
		t.Errorf("%d trailer reads were served before the failure, want 1: the catalog's, leaving the probe's to fail", got)
	}
}

// TestAVerifyingTrailerNamingAFooterElsewhereIsNotTheExemption: the trailer
// verifies but does not name the footer at the marker's slot plus the marker's
// size, so the missing end is not a marker alone and the open refuses.
func TestAVerifyingTrailerNamingAFooterElsewhereIsNotTheExemption(t *testing.T) {
	b, markerOff := rottenMarkerSeg(false)
	entries := rawEntries([]format.CatalogEntry{})
	copy(b[len(b)-raw.TrailerSize:], raw.Trailer(1, uint64(markerOff+format.SealMarkerSize+8), entries...))
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, b)
	r.plantSeg(2, namingSeg(2, 1))
	wantRefused(t, r, 1, markerOff, int64(len(b)))
}

// TestARottedRecordHeaderInAnUnnamedSealedSegmentIsConfinedByItsFooter plants
// a sealed segment no successor names, record B's header rotted. The footer
// verifying, the range holds the record the footer names: one synced corrupt
// drop. The footer's checksum broken, nothing names it and the open refuses.
func TestARottedRecordHeaderInAnUnnamedSealedSegmentIsConfinedByItsFooter(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	endB := endAfter(tailRec(200, 0x4100), tailRec(201, 0x8100))
	plant := func() ([]byte, *rig) {
		b, _ := buildSealed(1, tailRec(200, 0x4100), tailRec(201, 0x8100), tailRec(202, 0xc100))
		b[endA] ^= 0xff // B's record header
		r := newRig(t)
		r.writeFormat()
		return b, r
	}
	t.Run("the footer verifies", func(t *testing.T) {
		b, r := plant()
		r.plantSeg(1, b)
		st := runTails(t, r)
		if len(st.Tails.Corrupt) != 1 {
			t.Fatalf("Corrupt got %+v, want exactly B", st.Tails.Corrupt)
		}
		c := st.Tails.Corrupt[0]
		if c.Segment != 1 || !c.Drop.Synced || c.at() != endA || !errors.Is(c.Drop.Err, core.ErrCorrupt) {
			t.Errorf("Corrupt[0] got segment %d synced %v at %d err %v, want segment 1, synced, at %d, ErrCorrupt",
				c.Segment, c.Drop.Synced, c.at(), c.Drop.Err, endA)
		}
		if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
			t.Errorf("Damaged got %v, want [1]", st.Tails.Damaged)
		}
	})
	t.Run("the footer's checksum is broken", func(t *testing.T) {
		b, r := plant()
		b[len(b)-raw.TrailerSize-1] ^= 0xff // the last entry's byte: the footer checksum fails
		r.plantSeg(1, b)
		wantRefused(t, r, 1, endA, endB)
	})
}
