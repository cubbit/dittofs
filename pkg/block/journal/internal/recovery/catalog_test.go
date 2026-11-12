package recovery

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// catalogSteps is the workload the catalog checks drive: a deterministic
// step list holding one or more records of every kind the journal can append
// today — the content kinds 1-7 the scan converts, offloaded (8) and unmark
// (9), and one loss (10) and one forget (14) — over the shared workFiles and
// tagOf, so the kinds seededSteps never draws meet the catalog path exactly
// as the content kinds do. The numbers come from the shared notebook, so no
// two fed records share a pair; the forget names only its share tag, so it
// carries neither a file nor a version, as its kind's rules spell.
func catalogSteps() []step {
	notebook := &stepNotebook{}
	next := func(st *step) {
		st.seq = notebook.nextSeq()
		st.version = notebook.nextVersion()
	}
	add := func(kind uint8, file core.FileID, off, n int64) step {
		st := step{kind: kind, file: file, off: off, n: n, tag: tagOf(file)}
		next(&st)
		return st
	}
	staged := func(kind uint8, file core.FileID, off, n int64) step {
		st := add(kind, file, off, n)
		if kind == 1 || kind == 2 { // the two kinds whose payload the record carries
			st.payload = make([]byte, n)
			for i := range st.payload {
				st.payload[i] = byte(i + 1)
			}
			st.mtime = 1
		}
		return st
	}

	steps := []step{
		staged(1, workFiles[0], 0, 16),
		staged(2, workFiles[1], 32, 8),
		add(3, workFiles[0], 64, 8),
		add(4, workFiles[2], 128, 0), // a truncate: off is the new size
		add(5, workFiles[3], 8, 16),
		add(6, workFiles[1], 0, 0), // a delete: no extent
	}
	clone := add(7, workFiles[2], 64, 4)
	clone.clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
	loss := add(10, workFiles[3], 64, 8)
	loss.loss = &raw.LossBody{Dropped: 1, Flags: 1 | 3<<8} // the first write's seq, synced, a named reason
	forget := step{kind: 14, tag: 2, seq: notebook.nextSeq()}
	return append(steps, clone,
		add(8, workFiles[0], 0, 16),
		add(9, workFiles[1], 32, 8),
		loss,
		forget,
		staged(1, workFiles[2], 8, 8),
		staged(2, workFiles[3], 0, 24),
	)
}

// plantSealed plants the workload the way plantSteps does and then seals
// every segment but the newest: when chain is true each segment's header
// names the one before it as predecessor, and every sealed segment's bytes
// end with a raw seal marker, one footer entry per record it holds —
// ascending by RecordOffset, as the footer's own rules keep them — and the
// 32-byte trailer, the footer starting at the marker's offset + 32. The
// newest is left unsealed, the shape a crash leaves; a check that wants it
// sealed too appends its trailer with sealSeg. Each segment's entries come
// back so a check can mutate and re-plant them.
func (r *rig) plantSealed(steps []step, perSegment int, chain bool) ([]core.SegmentID, map[core.SegmentID][]format.CatalogEntry) {
	r.t.Helper()
	var (
		segs    []core.SegmentID
		entries = map[core.SegmentID][]format.CatalogEntry{}
	)
	for first := 0; first < len(steps); {
		id := core.SegmentID(len(segs) + 1)
		n := min(perSegment, len(steps)-first)
		h := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}
		if chain && len(segs) > 0 {
			h.Predecessor = uint64(id - 1)
		}
		b := h.Bytes()
		var ents []format.CatalogEntry
		for _, st := range steps[first : first+n] {
			recOff := int64(len(b))
			b = appendStep(b, id, st)
			ents = append(ents, format.CatalogEntry{
				File:         st.file,
				Extent:       core.Extent{Off: st.off, Len: st.n},
				RecordOffset: recOff,
				Seq:          core.Seq(st.seq),
				Version:      st.version,
				Kind:         format.Kind(st.kind),
				Tag:          st.tag,
			})
		}
		entries[id] = ents
		if first+n < len(steps) { // every segment but the newest
			b = appendSealed(b, id, ents)
		}
		r.plantSeg(id, b)
		segs = append(segs, id)
		first += n
	}
	return segs, entries
}

// appendSealed appends a seal marker, the footer of one entry per record the
// segment holds, and the trailer to b, as the journal's seal writer leaves
// them: the marker at the record slot just past the last record, the
// footer's entries at the marker's offset + 32, the trailer the file's last
// 32 bytes.
func appendSealed(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
	markerOff := len(b)
	ident := journalID
	b = append(b, raw.SealMarker{Identity: ident, Segment: uint64(id), Offset: uint64(markerOff)}.Bytes()...)
	var raws [][raw.CatalogEntrySize]byte
	for _, e := range ents {
		raws = append(raws, raw.Entry{
			FileID:       e.File.Bytes(),
			Offset:       uint64(e.Extent.Off),
			RecordOffset: uint32(e.RecordOffset),
			Length:       uint64(e.Extent.Len),
			Seq:          uint64(e.Seq),
			Version:      raw.Version(e.Version),
			Kind:         uint8(e.Kind),
			Tag:          uint32(e.Tag),
		}.Bytes())
	}
	// 1 is the catalog layout version this format writes. The entries come
	// first — raw.Trailer checksums them but appends only its own 32 bytes.
	var footer []byte
	for _, e := range raws {
		footer = append(footer, e[:]...)
	}
	return append(append(b, footer...), raw.Trailer(1, uint64(markerOff+raw.SealMarkerSize), raws...)...)
}

// sealSeg appends the seal a segment's tail lacks — the marker, its footer
// of ents and the trailer — writing the file back as a synced writer would
// have left it.
func (r *rig) sealSeg(id core.SegmentID, ents []format.CatalogEntry) {
	r.t.Helper()
	name := format.SegmentName(id)
	r.rewrite(name, appendSealed(r.fileBytes(name), id, ents))
}

// TestCatalogAndScanBuildTheSameIndex drives the rebuild's two sources over
// two identical directories and holds they give one index: run A reads the
// catalogue of every segment a verified successor names, run B scans every
// segment under the test-only force-scan switch, and the per-file entries
// must be equal — the catalogue is an accelerator, so whatever one source
// reads the other must answer with.
func TestCatalogAndScanBuildTheSameIndex(t *testing.T) {
	for _, row := range []struct {
		name  string
		steps []step
	}{
		{"all kinds", catalogSteps()},
		{"breadth", seededSteps(7)},
	} {
		t.Run(row.name, func(t *testing.T) {
			build := func(t *testing.T, forceScan bool) map[string][]index.Entry {
				t.Helper()
				r := newRig(t)
				r.writeFormat()
				segs, _ := r.plantSealed(row.steps, stepsPerSegment, true)
				if len(segs) < 2 {
					t.Fatalf("fixture: the workload planted %d segments, want a sealed one beside the newest", len(segs))
				}
				deps := r.runDeps()
				deps.rebuildForceScan = forceScan
				st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
				if err != nil {
					t.Fatalf("Run over the %s directory: %v", row.name, err)
				}
				want := len(segs) - 1 // every sealed segment a successor names; the newest scans
				if forceScan {
					want = 0
				}
				if got := st.Rebuilt.ByCatalog; got != want {
					t.Fatalf("Run rebuilt %d segments by catalog, want %d", got, want)
				}
				return entriesOf(st.Rebuilt.Index)
			}
			catalog := build(t, false)
			scan := build(t, true)
			if !sameEntries(catalog, scan) {
				t.Errorf("Run: the catalog rebuild built a different index than the scan of the same directory")
			}
		})
		t.Run(row.name+" all named", func(t *testing.T) {
			plant := func() (*rig, []core.SegmentID) {
				r := newRig(t)
				r.writeFormat()
				segs, ents := r.plantSealed(row.steps, stepsPerSegment, true)
				newest := segs[len(segs)-1]
				r.sealSeg(newest, ents[newest])
				// The successor whose header names the newest. It holds no
				// record, so nothing is built for it, but it is the
				// stream's newest now and scans at this open.
				r.plantSeg(newest+1, raw.SegmentHeader{
					Identity:    journalID,
					ID:          uint64(newest + 1),
					Predecessor: uint64(newest),
				}.Bytes())
				return r, segs
			}
			do := func(forceScan bool) map[string][]index.Entry {
				t.Helper()
				r, segs := plant()
				deps := r.runDeps()
				deps.rebuildForceScan = forceScan
				st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
				if err != nil {
					t.Fatalf("Run with every segment named and sealed: %v", err)
				}
				want := len(segs) // each plant's segment is named and sealed; the successor alone scans
				if forceScan {
					want = 0
				}
				if got := st.Rebuilt.ByCatalog; got != want {
					t.Fatalf("Run rebuilt %d segments by catalog, want %d", got, want)
				}
				return entriesOf(st.Rebuilt.Index)
			}
			catalog, scan := do(false), do(true)
			if !sameEntries(catalog, scan) {
				t.Errorf("Run: the catalog rebuild of a wholly named stream built a different index than the scan did")
			}
		})
	}
}

// TestCatalogReadsOnlySegmentsASuccessorNames holds the successor set rule
// through the open: a segment is offered its catalogue only when an own
// header names it as predecessor. The newest segment — even one whose own
// trailer is a valid, verifying catalogue — is scanned, and a directory
// where no header names a predecessor anywhere reads no catalogue at all
// and still rebuilds the same index.
func TestCatalogReadsOnlySegmentsASuccessorNames(t *testing.T) {
	steps := catalogSteps()
	var chained map[string][]index.Entry

	t.Run("the newest is scanned whatever its trailer says", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		segs, ents := r.plantSealed(steps, stepsPerSegment, true)
		if len(segs) < 2 {
			t.Fatalf("fixture: the workload planted %d segments, want a named one beside the newest", len(segs))
		}
		r.sealSeg(segs[len(segs)-1], ents[segs[len(segs)-1]])
		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over a directory whose every segment holds a verifying trailer: %v", err)
		}
		if got, want := st.Rebuilt.ByCatalog, len(segs)-1; got != want {
			t.Fatalf("Run rebuilt %d segments by catalog, want %d: the newest scans whatever its trailer says", got, want)
		}
		chained = entriesOf(st.Rebuilt.Index)
	})

	t.Run("no header names a predecessor anywhere", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		segs, _ := r.plantSealed(steps, stepsPerSegment, false)
		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over a directory where no header names a predecessor: %v", err)
		}
		if got, want := st.Rebuilt.ByCatalog, 0; got != want {
			t.Fatalf("Run rebuilt %d segments by catalog, want %d: nothing is named, so every segment scans", got, want)
		}
		if !sameEntries(chained, entriesOf(st.Rebuilt.Index)) {
			t.Errorf("Run: the scan-only rebuild built a different index than the chained one that read its %d catalogues", len(segs)-1)
		}
	})
}
