package recovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// castagnoli is the table the trailer checksums are computed with — plain
// CRC32C, unseeded, as format/crc.go computes them — so a check that redoes
// a trailer's checksum after mutating its bytes mirrors the format's own.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

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
	raws := rawEntries(ents)
	// 1 is the catalog layout version this format writes. The entries come
	// first — raw.Trailer checksums them but appends only its own 32 bytes.
	var footer []byte
	for _, e := range raws {
		footer = append(footer, e[:]...)
	}
	return append(append(b, footer...), raw.Trailer(1, uint64(markerOff+raw.SealMarkerSize), raws...)...)
}

// rawEntries encodes catalogue entries the way the seal writer's footer
// holds them, so a check that rebuilds a trailer names the same bytes the
// plant wrote.
func rawEntries(ents []format.CatalogEntry) [][raw.CatalogEntrySize]byte {
	out := make([][raw.CatalogEntrySize]byte, 0, len(ents))
	for _, e := range ents {
		out = append(out, raw.Entry{
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
	return out
}

// sealSeg appends the seal a segment's tail lacks — the marker, its footer
// of ents and the trailer — writing the file back as a synced writer would
// have left it.
func (r *rig) sealSeg(id core.SegmentID, ents []format.CatalogEntry) {
	r.t.Helper()
	name := format.SegmentName(id)
	r.rewrite(name, appendSealed(r.fileBytes(name), id, ents))
}

// asideSummary holds the columns of an aside header two sources build
// alike: a catalogued aside's header comes either decoded from the record
// or, for a forget, from the entry alone — and an entry carries no
// synced-through column and no body checksum — so those two columns stay
// zero where the entry feeds and are compared by nobody here. Everything
// the phases will read off an aside is what the summary keeps.
type asideSummary struct {
	Kind    format.Kind
	File    core.FileID
	Extent  core.Extent
	Seq     core.Seq
	Version core.Version
	Tag     core.ShareTag
	Offset  int64
	Segment core.SegmentID
}

// summarizeAside projects one aside's header onto the columns both sources
// build alike.
func summarizeAside(h format.RecordHeader) asideSummary {
	return asideSummary{
		Kind:    h.Kind,
		File:    h.File,
		Extent:  h.Extent,
		Seq:     h.Seq,
		Version: h.Version,
		Tag:     h.Tag,
		Offset:  h.Offset,
		Segment: h.Segment,
	}
}

// asidesOf collects the rebuild's asides per segment, projected onto the
// columns both sources build alike, so a both-ways run compares them for
// every segment the rebuild walked.
func asidesOf(st State) map[core.SegmentID][]asideSummary {
	out := make(map[core.SegmentID][]asideSummary, len(st.Rebuilt.Scans))
	for _, sc := range st.Rebuilt.Scans {
		as := make([]asideSummary, len(sc.Aside))
		for i, a := range sc.Aside {
			as[i] = summarizeAside(a.Header)
		}
		out[sc.ID] = as
	}
	return out
}

// TestCatalogAndScanBuildTheSameIndex drives the rebuild's two sources over
// two identical directories and holds they give one index and one aside list
// per segment: run A reads the catalogue of every segment a verified
// successor names, run B scans every segment under the test-only force-scan
// switch, and the per-file entries and the per-segment asides must be equal
// — the catalogue is an accelerator, so whatever one source reads the other
// must answer with.
func TestCatalogAndScanBuildTheSameIndex(t *testing.T) {
	for _, row := range []struct {
		name  string
		steps []step
	}{
		{"all kinds", catalogSteps()},
		{"breadth", seededSteps(7)},
	} {
		t.Run(row.name, func(t *testing.T) {
			build := func(t *testing.T, forceScan bool) (map[string][]index.Entry, map[core.SegmentID][]asideSummary) {
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
				return entriesOf(st.Rebuilt.Index), asidesOf(st)
			}
			catalog, catalogAsides := build(t, false)
			scan, scanAsides := build(t, true)
			if !sameEntries(catalog, scan) {
				t.Errorf("Run: the catalog rebuild built a different index than the scan of the same directory")
			}
			if len(catalogAsides) != len(scanAsides) {
				t.Fatalf("Run: the two rebuilds scanned %d and %d segments, want the same set", len(catalogAsides), len(scanAsides))
			}
			for id, want := range catalogAsides {
				if got := scanAsides[id]; !slices.Equal(want, got) {
					t.Errorf("Run: segment %d's asides from the scan are %+v, want the catalog rebuild's %+v", id, got, want)
				}
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

// TestCatalogDemotesEveryNoFooterToAScan breaks one sealed, named segment's
// catalogue each way a writer or a crash can leave it unreadable — a file
// cut past its trailer, a footer entry whose bytes differ from what the
// trailer checksummed, a trailer of a layout this format does not read or
// carrying flags none define, the seal marker the footer needs wiped, a
// trailer naming more entries than fit before it, one naming fewer than the
// segment holds, one naming none over records the segment does hold, and a
// file too short to be a segment at all — and holds, per row, that the open
// still succeeds, the segment is built by no catalogue, and the index the
// demoted rebuild built is the one a forced scan of the same directory
// builds: a footer that does not verify loses the open speed and nothing
// else.
func TestCatalogDemotesEveryNoFooterToAScan(t *testing.T) {
	// footerOffset names where a sealed segment's entries start: the entries
	// run to the trailer, the file's last 32 bytes.
	footerOffset := func(b []byte, ents []format.CatalogEntry) int64 {
		return int64(len(b)) - raw.TrailerSize - raw.CatalogEntrySize*int64(len(ents))
	}
	// replaceTrailer overwrites the file's last 32 bytes with the trailer tr.
	replaceTrailer := func(b, tr []byte) {
		copy(b[len(b)-raw.TrailerSize:], tr)
	}

	rows := []struct {
		name   string
		mutate func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte
	}{
		{"truncated file", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// The cut takes the trailer and the last 8 bytes of the footer's
			// entries, leaving the seal marker standing.
			return b[:len(b)-40]
		}},
		{"flipped entry byte", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			b[footerOffset(b, ents)] ^= 0xff
			return b
		}},
		{"trailer layout 2", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			tr := raw.Trailer(2, uint64(footerOffset(b, ents)), rawEntries(ents)...)
			replaceTrailer(b, tr)
			return b
		}},
		{"trailer flags nonzero", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// Byte 6 opens the flags column, read as a version none define.
			// The trailer's own checksum is redone over the flipped bytes —
			// plain CRC32C, as format/crc.go computes it — so the flags
			// column itself is what refuses, not the checksum a bare flip
			// breaks first.
			b[len(b)-raw.TrailerSize+6] ^= 1
			binary.LittleEndian.PutUint32(b[len(b)-4:],
				crc32.Checksum(b[len(b)-raw.TrailerSize:len(b)-4], castagnoli))
			return b
		}},
		{"no seal marker before the footer", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			off := footerOffset(b, ents) - format.SealMarkerSize
			for i := range format.SealMarkerSize {
				b[off+int64(i)] = 0xee // a pattern no slot is told by
			}
			return b
		}},
		{"entryCount past the trailer", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			es := rawEntries(ents)
			tr := raw.Trailer(1, uint64(footerOffset(b, ents)), append(es, es[0])...)
			replaceTrailer(b, tr)
			return b
		}},
		{"footer names a subset", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// One entry fewer than the segment holds: the last named record
			// ends before the marker, so the footer is refused rather than
			// answered with a silently truncated index.
			es := rawEntries(ents)
			tr := raw.Trailer(1, uint64(footerOffset(b, ents)), es[:len(es)-1]...)
			replaceTrailer(b, tr)
			return b
		}},
		{"footer skips a middle entry", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// A middle entry dropped, its record left standing in the segment
			// and the last named record still ending at the marker, so both of
			// the footer's checksums recompute honestly over the entries it
			// does name: only the tiling of the entries against the records
			// can catch it, and a footer that slips it would answer the index
			// without the skipped record — an index the scan of the same
			// bytes never builds.
			mid := len(ents) / 2
			keep := slices.Concat(ents[:mid], ents[mid+1:])
			out := b[:footerOffset(b, ents)]
			for _, e := range rawEntries(keep) {
				out = append(out, e[:]...)
			}
			return append(out, raw.Trailer(1, uint64(footerOffset(b, ents)), rawEntries(keep)...)...)
		}},
		{"zero entries", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// The trailer honestly names no entries, but the segment holds
			// records: the first record slot is not where the marker sits,
			// so the footer is refused.
			replaceTrailer(b, raw.Trailer(1, uint64(footerOffset(b, ents))))
			return b
		}},
		{"short file", func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			// 20 bytes hold neither a whole segment header nor a trailer: the
			// attach pass classes the segment unverified and the rebuild never
			// reaches it, let alone its catalogue.
			return b[:20]
		}},
	}

	// open plants the chained workload on a fresh rig, rewrites the
	// first sealed segment's whole file with what mutate returns, and opens
	// the journal. It returns the state and the planted ids, so each row's
	// assertions name their own segment.
	open := func(t *testing.T, mutate func([]byte, core.SegmentID, []format.CatalogEntry) []byte, forceScan bool) (State, []core.SegmentID) {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		segs, ents := r.plantSealed(catalogSteps(), stepsPerSegment, true)
		if len(segs) < 3 {
			t.Fatalf("fixture: the workload planted %d segments, want a sealed one to break with a sealed neighbour beside the newest", len(segs))
		}
		name := format.SegmentName(segs[0])
		r.rewrite(name, mutate(r.fileBytes(name), segs[0], ents[segs[0]]))
		deps := r.runDeps()
		deps.rebuildForceScan = forceScan
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over the directory of a segment whose footer does not verify: %v", err)
		}
		return st, segs
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			st, segs := open(t, row.mutate, false)
			victim := segs[0]
			// Intact, the plant catalogues every sealed segment a successor
			// names: all but the newest. The broken one must not count —
			// most rows demote it to the scan; the short file is refused at
			// attach before the rebuild reaches it at all.
			if got, want := st.Rebuilt.ByCatalog, len(segs)-2; got != want {
				t.Fatalf("Run rebuilt %d segments by catalog, want %d: the broken footer's segment was not kept out of the catalogues — neither demoted to the scan nor refused at attach", got, want)
			}
			if sc, ok := scanOf(st, victim); ok && sc.SealedByCatalog {
				t.Errorf("Run: segment %d was rebuilt by catalog, want the scan: a footer that does not verify never feeds", victim)
			}
			scanSt, _ := open(t, row.mutate, true)
			if !sameEntries(entriesOf(st.Rebuilt.Index), entriesOf(scanSt.Rebuilt.Index)) {
				t.Errorf("Run: the rebuild that demoted the broken footer built a different index than the forced scan of the same directory")
			}
		})
	}

	// A sealed segment holding no record, whose trailer honestly names none,
	// has its marker at the first record slot: the footer verifies, so the
	// catalogue reads it as an empty scan — the same emptiness the walk of
	// the same bytes builds — and the segment is not demoted. It replaces a
	// records-holding one, whose records are lost to both sources alike, so
	// the equality of the two answers is what the row holds.
	t.Run("zero entries over a record-less segment", func(t *testing.T) {
		mutate := func(b []byte, id core.SegmentID, ents []format.CatalogEntry) []byte {
			out := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
			markerOff := len(out)
			out = append(out, raw.SealMarker{Identity: journalID, Segment: uint64(id), Offset: uint64(markerOff)}.Bytes()...)
			return append(out, raw.Trailer(1, uint64(markerOff+raw.SealMarkerSize))...)
		}
		st, segs := open(t, mutate, false)
		victim := segs[0]
		if got, want := st.Rebuilt.ByCatalog, len(segs)-1; got != want {
			t.Fatalf("Run rebuilt %d segments by catalog, want %d: an empty footer over an empty segment is a footer the catalogue reads", got, want)
		}
		sc, ok := scanOf(st, victim)
		if !ok || !sc.SealedByCatalog {
			t.Fatalf("Run: segment %d was not rebuilt by catalog, want its honestly empty footer read", victim)
		}
		if len(sc.Aside) != 0 {
			t.Errorf("Run: the catalogued scan of a record-less segment holds %d aside records, want none", len(sc.Aside))
		}
		scanSt, _ := open(t, mutate, true)
		if !sameEntries(entriesOf(st.Rebuilt.Index), entriesOf(scanSt.Rebuilt.Index)) {
			t.Errorf("Run: the catalogued empty rebuild built a different index than the forced scan of the same directory")
		}
	})
}

// scanOf finds one own segment's scan in the rebuild's answer, reporting
// whether the segment was attached at all.
func scanOf(st State, id core.SegmentID) (segmentScan, bool) {
	for _, sc := range st.Rebuilt.Scans {
		if sc.ID == id {
			return sc, true
		}
	}
	return segmentScan{}, false
}

// TestCatalogReadsOnlySegmentsASuccessorNames holds the successor set rule
// through the open: a segment is offered its catalogue only when an own
// header names it as predecessor. The newest segment — even one whose own
// trailer is a valid, verifying catalogue — is scanned, and a directory
// where no header names a predecessor anywhere reads no catalogue at all
// and still rebuilds the same index.
func TestCatalogReadsOnlySegmentsASuccessorNames(t *testing.T) {
	steps := catalogSteps()

	// chainedIndex builds the reference the unchained plant compares
	// against: the index of the same workload planted chained, where every
	// sealed segment is named and reads its catalogue — built per subtest,
	// so one subtest's failure takes no other's reference with it.
	chainedIndex := func(t *testing.T) map[string][]index.Entry {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		r.plantSealed(steps, stepsPerSegment, true)
		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over the chained directory: %v", err)
		}
		return entriesOf(st.Rebuilt.Index)
	}

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
	})

	t.Run("no header names a predecessor anywhere", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		segs, _ := r.plantSealed(steps, stepsPerSegment, false)
		if len(segs) < 2 {
			t.Fatalf("fixture: the workload planted %d segments, want a sealed one whose catalogue the chained plant reads", len(segs))
		}
		st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over a directory where no header names a predecessor: %v", err)
		}
		if got, want := st.Rebuilt.ByCatalog, 0; got != want {
			t.Fatalf("Run rebuilt %d segments by catalog, want %d: nothing is named, so every segment scans", got, want)
		}
		if !sameEntries(chainedIndex(t), entriesOf(st.Rebuilt.Index)) {
			t.Errorf("Run: the scan-only rebuild built a different index than the chained one that read its catalogues")
		}
	})
}

// buildSealed returns the bytes of segment id holding recs, sealed the way
// plantSealed seals and as the journal's seal writer leaves them — one
// footer entry per record, ascending by RecordOffset, and the trailer —
// with the entries themselves, derived from the records' own columns, so a
// check can assert or mutate them.
func buildSealed(id core.SegmentID, recs ...raw.Record) ([]byte, []format.CatalogEntry) {
	rc := slices.Clone(recs)
	b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
	ents := make([]format.CatalogEntry, 0, len(rc))
	for i := range rc {
		rc[i].Identity = journalID
		rc[i].Segment = uint64(id)
		rc[i].RecordOffset = uint32(len(b))
		b = append(b, rc[i].Bytes()...)
		ents = append(ents, format.CatalogEntry{
			File:         core.NewFileID(rc[i].FileID),
			Extent:       core.Extent{Off: int64(rc[i].Offset), Len: int64(rc[i].Length)},
			RecordOffset: int64(rc[i].RecordOffset),
			Seq:          core.Seq(rc[i].Seq),
			Version:      core.Version{Epoch: rc[i].Version.Epoch, Counter: rc[i].Version.Counter},
			Kind:         format.Kind(rc[i].Kind),
			Tag:          core.ShareTag(rc[i].Tag),
		})
	}
	return appendSealed(b, id, ents), ents
}

// withoutTheCloneEntry drops the workload's one clone-target entry from a
// built index map: the confined record's own contribution, which the scan
// run the comparison reads attaches and the catalog run that confined it
// does not. In the read-out a clone target is the marker carrying its spec.
func withoutTheCloneEntry(ix *index.Index) map[string][]index.Entry {
	m := entriesOf(ix)
	for k, es := range m {
		m[k] = slices.DeleteFunc(slices.Clone(es), func(e index.Entry) bool {
			return e.Clone != nil
		})
	}
	return m
}

// TestCatalogReadsTheHeaderOnlyKindsAtTheirEntry holds the read-back kinds'
// arm: a sealed, catalogued segment's loss, hold, unhold and stamp records
// are read at their entries' offsets and set aside as the headers their own
// bytes verified — named fields and payload checksum alike — never as the
// headers the entries claim, and exactly what a forced scan of the same
// directory sets aside.
func TestCatalogReadsTheHeaderOnlyKindsAtTheirEntry(t *testing.T) {
	aside := []raw.Record{
		rec(10, func(r *raw.Record) { r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8} }),
		rec(11, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 3}; r.Offset, r.Length = 0, 0 }), // hold
		rec(12, func(r *raw.Record) { // unhold: no file, no version, no extent
			r.Cut = &raw.CutBody{Cut: 4}
			r.FileID = [16]byte{}
			r.Version = raw.Version{}
			r.Offset, r.Length = 0, 0
		}),
		rec(13, func(r *raw.Record) { r.Cut = &raw.CutBody{Cut: 5}; r.Offset, r.Length = 0, 0 }), // stamp
	}

	// build plants the sealed segment and a successor whose header names
	// it — the successor is the stream's newest, so it scans while the
	// sealed one is offered its catalogue — and opens the journal with the
	// test-only switch set.
	build := func(t *testing.T, forceScan bool) (State, []format.CatalogEntry) {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		seg, ents := buildSealed(2, aside...)
		r.plantSeg(2, seg)
		r.plantSeg(3, raw.SegmentHeader{Identity: journalID, ID: 3, Predecessor: 2}.Bytes())
		deps := r.runDeps()
		deps.rebuildForceScan = forceScan
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over the directory holding the read-back kinds: %v", err)
		}
		return st, ents
	}

	st, ents := build(t, false)
	if got, want := st.Rebuilt.ByCatalog, 1; got != want {
		t.Fatalf("Run rebuilt %d segments by catalog, want %d: the sealed one was not offered its catalogue", got, want)
	}
	cat, ok := scanOf(st, 2)
	if !ok {
		t.Fatalf("Run: the scans name no segment 2, want the sealed one")
	}
	if got, want := len(cat.Aside), len(aside); got != want {
		t.Fatalf("the catalogued scan sets %d records aside, want the %d planted", got, want)
	}
	wantKinds := []format.Kind{format.KindLoss, format.KindHold, format.KindUnhold, format.KindStamp}
	for i, a := range cat.Aside {
		if a.Header.Kind != wantKinds[i] {
			t.Errorf("aside record %d's kind got %v, want %v", i, a.Header.Kind, wantKinds[i])
		}
		// The entry honestly mirrors the record, so the named columns agree —
		// an arm feeding from the entry's claim passes these too:
		want := headerFromEntry(2, ents[i])
		if a.Header.File != want.File || a.Header.Extent != want.Extent ||
			a.Header.Seq != want.Seq || a.Header.Version != want.Version ||
			a.Header.Kind != want.Kind || a.Header.Offset != want.Offset {
			t.Errorf("aside record %d's header got file %v extent %+v seq %d version %+v kind %v offset %d, want the record's %v %+v %d %+v %v %d",
				i, a.Header.File, a.Header.Extent, a.Header.Seq, a.Header.Version, a.Header.Kind, a.Header.Offset,
				want.File, want.Extent, want.Seq, want.Version, want.Kind, want.Offset)
		}
		// The payload checksum no entry carries: the verified header holds
		// the record body's own, an entry's claim always zero.
		if a.Header.PayloadCRC == 0 {
			t.Errorf("aside record %d carries payload checksum 0, want the verified header's: the aside came from the entry's claim, not the record", i)
		}
	}

	// The aside a forced scan of the same directory collects — headers read
	// by the walk, verified to their own place — is the aside the catalogued
	// scan holds, whole struct equal.
	scanSt, _ := build(t, true)
	if got, want := scanSt.Rebuilt.ByCatalog, 0; got != want {
		t.Fatalf("the forced-scan run rebuilt %d segments by catalog, want %d", got, want)
	}
	scan, scOK := scanOf(scanSt, 2)
	if !scOK {
		t.Fatalf("Run: the forced-scan run's scans name no segment 2")
	}
	if !slices.EqualFunc(scan.Aside, cat.Aside, func(x, y asideRecord) bool { return x.Header == y.Header }) {
		t.Errorf("the catalogued scan's asides differ from the scan's, want the verified headers of one and the same records")
	}
}

// TestCatalogConfinesARecordThatDoesNotVerify holds the confinement: a
// record the catalogue's own read cannot verify — a body whose checksum the
// damage broke, an entry whose summary its record contradicts — drops into
// its segment's Dropped, carried for the tail's classing, while the open
// succeeds, every neighbour still attaches and the segment is not demoted
// from its catalogue. The dropped record's header is the record's own, not
// the entry's claim of it: the segments win.
func TestCatalogConfinesARecordThatDoesNotVerify(t *testing.T) {
	rows := []struct {
		name string
		idx  int // the victim's entry whose record the row confines
		// mutateEntry rewrites the confined record's entry before the seal
		// is rebuilt over it; the record's own bytes are left honest.
		mutateEntry func(*format.CatalogEntry)
		// flipBody flips one byte of the confined record's body instead;
		// the entries are left honest.
		flipBody bool
		// scanRefuses holds that the header walk over the broken bytes
		// refuses the open itself, so the index leg compares against an
		// honest directory's scan rather than the broken one's.
		scanRefuses bool
	}{
		{"a loss whose body bytes are flipped", 3, nil, true, false},
		{"a clone target whose spec flips", 0, nil, true, true},
		{"an entry whose seq disagrees with its record header", 0, func(e *format.CatalogEntry) { e.Seq += 1 }, false, false},
		{"an entry whose extent disagrees with its record header", 0, func(e *format.CatalogEntry) { e.Extent.Off += 64 }, false, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// plant writes the chained workload and damages one record of
			// the second sealed segment — the one holding the clone target
			// at its first record and the loss at its fourth — returning the
			// rig, the planted ids, the confined record's entry as the
			// footer now names it and the same entry as it was planted.
			// damage false replants the honest directory.
			plant := func(damage bool) (*rig, []core.SegmentID, format.CatalogEntry, format.CatalogEntry) {
				r := newRig(t)
				r.writeFormat()
				segs, ents := r.plantSealed(catalogSteps(), stepsPerSegment, true)
				victim := segs[1]
				name := format.SegmentName(victim)
				es := slices.Clone(ents[victim])
				b := r.fileBytes(name)
				if damage && row.mutateEntry != nil {
					row.mutateEntry(&es[row.idx])
					// The seal is rebuilt over the lying entry — the
					// trailer's checksums recomputed — so the footer is one
					// a writer could have bungled, not one the damage itself
					// already refuses.
					markerOff := len(b) - raw.TrailerSize - raw.CatalogEntrySize*len(es) - format.SealMarkerSize
					b = appendSealed(b[:markerOff], victim, es)
				}
				if damage && row.flipBody {
					b[es[row.idx].RecordOffset+format.RecordHeaderSize] ^= 0xff
				}
				r.rewrite(name, b)
				return r, segs, es[row.idx], ents[victim][row.idx]
			}
			open := func(r *rig, forceScan bool) (State, error) {
				deps := r.runDeps()
				deps.rebuildForceScan = forceScan
				return Run(context.Background(), deps, core.Version{}, journalID, nil)
			}

			r, segs, confined, honest := plant(true)
			victim := segs[1]
			st, err := open(r, false)
			if err != nil {
				t.Fatalf("Run over the damaged directory: %v", err)
			}
			// The confinement is per record: the segment keeps its catalogue
			// and its neighbours their entries.
			if got, want := st.Rebuilt.ByCatalog, len(segs)-1; got != want {
				t.Fatalf("Run rebuilt %d segments by catalog, want %d: a confined record demotes nothing", got, want)
			}
			sc, ok := scanOf(st, victim)
			if !ok {
				t.Fatalf("Run: the scans name no segment %d, want the damaged one", victim)
			}
			if len(sc.Dropped) != 1 {
				t.Fatalf("the scan of segment %d holds %d dropped records, want the one the damage confined", victim, len(sc.Dropped))
			}
			d := sc.Dropped[0]
			if d.Entry != confined {
				t.Errorf("the dropped entry got %+v, want the damaged one %+v", d.Entry, confined)
			}
			// The record outranks its summary: the dropped header is what
			// the record's own bytes decoded as, whatever the entry claims.
			if d.Header.Seq != honest.Seq || d.Header.Extent != honest.Extent {
				t.Errorf("the dropped record's header carries seq %d extent %+v, want the record's %d %+v: the segments win",
					d.Header.Seq, d.Header.Extent, honest.Seq, honest.Extent)
			}
			if !d.Synced {
				t.Errorf("the dropped record is not marked synced, want it: it sat below a seal that verifies")
			}
			if !errors.Is(d.Err, core.ErrCorrupt) {
				t.Errorf("the dropped record's error got %v, want one wrapping ErrCorrupt", d.Err)
			}
			for _, a := range sc.Aside {
				if a.Header.Kind == confined.Kind && a.Header.Seq == honest.Seq {
					t.Errorf("the confined record was also set aside, want it dropped and carried, feeding nothing")
				}
			}

			// Every neighbour attaches: the index is the scan's, minus the
			// confined record's own contribution.
			sr, _, _, _ := plant(true)
			if row.scanRefuses {
				scanSt, scanErr := open(sr, true)
				if scanErr != nil {
					t.Fatalf("the forced scan over the damaged directory: %v, want it to drop the record as the catalogue does", scanErr)
				}
				if len(scanSt.Tails.Corrupt) != 1 || !scanSt.Tails.Corrupt[0].Drop.Synced {
					t.Errorf("the forced scan's Corrupt got %+v, want the one clone target, synced", scanSt.Tails.Corrupt)
				}
				// The walk drops the broken bytes itself, so the
				// neighbours' reference is the honest directory's scan.
				cr, _, _, _ := plant(false)
				cleanSt, cerr := open(cr, true)
				if cerr != nil {
					t.Fatalf("Run over the honest directory: %v", cerr)
				}
				got := entriesOf(st.Rebuilt.Index)
				if row.idx == 0 { // the confined clone target's entry removed from the reference
					if !sameEntries(withoutTheCloneEntry(cleanSt.Rebuilt.Index), got) {
						t.Errorf("Run: the confined rebuild differs from the honest scan beyond the one confined record")
					}
				} else if !sameEntries(entriesOf(cleanSt.Rebuilt.Index), got) {
					t.Errorf("Run: a confined aside record changed an index neither source feeds it into")
				}
				return
			}
			scanSt, scanErr := open(sr, true)
			if scanErr != nil {
				t.Fatalf("the forced scan over the damaged directory: %v", scanErr)
			}
			got := entriesOf(st.Rebuilt.Index)
			if row.idx == 0 {
				if !sameEntries(withoutTheCloneEntry(scanSt.Rebuilt.Index), got) {
					t.Errorf("Run: the confined rebuild differs from the scan beyond the one confined record")
				}
			} else if !sameEntries(entriesOf(scanSt.Rebuilt.Index), got) {
				t.Errorf("Run: a confined aside record changed an index neither source feeds it into")
			}
		})
	}
}

// TestCatalogRefusesTheLargestNumbersAnEntryCarries holds the largest-value
// rule on the entry path: an entry whose record is never read back — a
// release, whose kind an entry stands for — carries a sequence number no
// writer issues, and the open refuses naming the segment, confining nothing
// and demoting nothing.
func TestCatalogRefusesTheLargestNumbersAnEntryCarries(t *testing.T) {
	for _, row := range []struct {
		name string
		seq  uint64
	}{
		{"the largest sequence number", math.MaxUint64},
		{"sequence number zero", 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			segs, ents := r.plantSealed(catalogSteps(), stepsPerSegment, true)
			victim := segs[0]
			name := format.SegmentName(victim)
			es := slices.Clone(ents[victim])
			es[2].Seq = core.Seq(row.seq) // the release entry, kind 3: its record is never read back
			b := r.fileBytes(name)
			markerOff := len(b) - raw.TrailerSize - raw.CatalogEntrySize*len(es) - format.SealMarkerSize
			r.rewrite(name, appendSealed(b[:markerOff], victim, es))

			_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			if err == nil {
				t.Fatalf("Run over an entry carrying %s returned no error, want the open to refuse", row.name)
			}
			if !errors.Is(err, core.ErrUnknownFormat) {
				t.Errorf("Run: an entry carrying %s got err %v, want ErrUnknownFormat, the largest-value rule's own refusal", row.name, err)
			}
			if got := err.Error(); !contains(got, fmt.Sprint(victim)) {
				t.Errorf("Run: the refusal %q does not name the segment %d it refused", got, victim)
			}
		})
	}
}

// TestCatalogRefusesARecordHeaderOfUnknownKind holds the triage the two
// sources share: a record whose header verifies at the place its entry
// names but names a kind this format version does not define is a format
// this binary does not read, refused by the open itself, never confined
// as one record's own damage — and the header walk refuses the same bytes
// the same way, so neither source steps over the record or serves it.
func TestCatalogRefusesARecordHeaderOfUnknownKind(t *testing.T) {
	// plant grows the chained workload and rebuilds the loss record of its
	// second sealed segment through the raw encoder with kind 0 — its two
	// checksums recomputed over the mutated bytes — so the header verifies
	// at its place and only its kind refuses. It returns the rig and the
	// planted ids.
	plant := func(t *testing.T) (*rig, []core.SegmentID) {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		segs, ents := r.plantSealed(catalogSteps(), stepsPerSegment, true)
		if len(segs) < 3 {
			t.Fatalf("fixture: the workload planted %d segments, want a sealed one beside the newest", len(segs))
		}
		victim := segs[1] // holds the loss at its fourth entry
		name := format.SegmentName(victim)
		b := r.fileBytes(name)
		e := ents[victim][3]
		body := b[e.RecordOffset+format.RecordHeaderSize : e.RecordOffset+format.RecordHeaderSize+16]
		unk := raw.Record{
			Identity:     journalID,
			Kind:         0, // a kind this format version does not define
			Segment:      uint64(victim),
			FileID:       e.File.Bytes(),
			Offset:       uint64(e.Extent.Off),
			Length:       uint64(e.Extent.Len),
			Seq:          uint64(e.Seq),
			Version:      raw.Version{Epoch: e.Version.Epoch, Counter: e.Version.Counter},
			Tag:          uint32(e.Tag),
			RecordOffset: uint32(e.RecordOffset),
			Loss: &raw.LossBody{
				Dropped: binary.LittleEndian.Uint64(body[0:8]),
				Flags:   binary.LittleEndian.Uint64(body[8:16]),
			},
		}
		got := unk.Bytes()
		if next := ents[victim][4].RecordOffset; e.RecordOffset+int64(len(got)) > next {
			t.Fatalf("fixture: the rebuilt record runs to %d, past its slot's end %d", e.RecordOffset+int64(len(got)), next)
		}
		copy(b[e.RecordOffset:], got)
		r.rewrite(name, b)
		return r, segs
	}

	r, segs := plant(t)
	victim := segs[1]
	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err == nil {
		t.Fatalf("Run over a catalogued segment whose record names an unknown kind returned no error, want the open to refuse")
	}
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("Run: a record header of an unknown kind got err %v, want ErrUnknownFormat, the open's own refusal", err)
	}
	if got := err.Error(); !contains(got, fmt.Sprint(victim)) {
		t.Errorf("Run: the refusal %q does not name the segment %d it refused", got, victim)
	}

	// The same bytes through the walk: the sources give one answer.
	sr, _ := plant(t)
	deps := sr.runDeps()
	deps.rebuildForceScan = true
	_, err = Run(context.Background(), deps, core.Version{}, journalID, nil)
	if err == nil {
		t.Fatalf("the forced scan over a record of an unknown kind returned no error, want the open to refuse")
	}
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("the forced scan: a record header of an unknown kind got err %v, want ErrUnknownFormat as the catalogued read's", err)
	}
}

// TestCatalogScansARecycledSegmentsStaleTrailer holds the successor-set rule
// through the open. The take here returns its segment to the same id, a
// deliberately impossible configuration — a recycle draws a fresh id above
// every id the directory names, so the old life's marker, footer and
// trailer name an id the take never holds, and the trailer's own seal
// marker check refuses them there, as the fresh-id sibling test plants. The
// same-id take is kept nonetheless, for it isolates the successor-set rule
// on its own: the take is the stream's newest, named by no successor, so
// the open never offers its trailer a read at all, and the stale catalogue
// — the old life's marker, footer and trailer bytes left past the new
// records by the crash that dropped the recycle's trailer zeroing, still
// verifying for this id, identity and offset — is unread on that rule
// alone. The open succeeds, no entry of the old life's footer lands in the
// index — the honest plant, the take a completed recycle leaves, builds the
// same index — ByCatalog counts the recycled segment nowhere, and a forced
// scan of the same directory is identical, so the stale trailer fed nothing
// the walk does not.
func TestCatalogScansARecycledSegmentsStaleTrailer(t *testing.T) {
	steps := catalogSteps()

	// The new life: one write, of a file and extent the old life never
	// touches, drawn from a fresh notebook — the recycle keeps the segment's
	// id, so the records are all that is new about the take.
	notebook := &stepNotebook{}
	take := step{kind: 1, file: workFiles[0], off: 256, n: 16, tag: tagOf(workFiles[0])}
	take.seq = notebook.nextSeq()
	take.version = notebook.nextVersion()
	take.payload = make([]byte, take.n)
	for i := range take.payload {
		take.payload[i] = byte(i + 1)
	}
	take.mtime = 1

	// plant grows the chained workload and rewrites its newest segment — the
	// one in line to be recycled — as the crash left it. recycle true seals
	// its old life first, then rewrites the header as the new take, appends
	// the new life's records before the old marker and keeps the old life's
	// marker, footer and trailer bytes at their own offsets past them:
	// the recycle's trailer zeroing is the one write the crash dropped.
	// recycle false plants the honest directory — the same take over a
	// completed recycle, none of the old bytes in the file.
	plant := func(t *testing.T, recycle bool) (*rig, []core.SegmentID) {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		segs, ents := r.plantSealed(steps, stepsPerSegment, true)
		if len(segs) < 3 {
			t.Fatalf("fixture: the workload planted %d segments, want the recycled one beside named neighbours", len(segs))
		}
		victim := segs[len(segs)-1]
		name := format.SegmentName(victim)
		// The take's header: the same id, identity and predecessor, so the
		// stream stays chained — and no successor names it, for the take is
		// the stream's newest.
		b := raw.SegmentHeader{Identity: journalID, ID: uint64(victim), Predecessor: uint64(victim - 1)}.Bytes()
		b = appendStep(b, victim, take)
		if recycle {
			r.sealSeg(victim, ents[victim]) // the old life, sealed as the recycle found it
			old := r.fileBytes(name)
			// The old marker sits just below the footer's entries, its own
			// offset past the new records.
			markerOff := len(old) - raw.TrailerSize - raw.CatalogEntrySize*len(ents[victim]) - format.SealMarkerSize
			if len(b) > markerOff {
				t.Fatalf("fixture: the take's records run to %d, past the old marker at %d", len(b), markerOff)
			}
			b = append(b, make([]byte, markerOff-len(b))...)
			b = append(b, old[markerOff:]...) // the old marker, footer and trailer, untouched
			r.rewrite(name, b)
			// The point of the fixture: the stale trailer is a live catalogue —
			// marker, entries and checksums verify for this id, identity and
			// offset, naming the old life's records — so only the
			// successor-set rule keeps it unread.
			rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
			footer, ferr := format.ReadFooter(readerAt{rd: rd, name: name}, int64(len(b)), journalID, victim)
			if ferr != nil {
				t.Fatalf("fixture: the recycled take's stale trailer does not verify: %v", ferr)
			}
			if !slices.Equal(footer.Entries, ents[victim]) {
				t.Fatalf("fixture: the stale trailer names the entries %+v, want the old life's %+v", footer.Entries, ents[victim])
			}
		} else {
			r.rewrite(name, b)
		}
		return r, segs
	}

	// open plants and opens, forceScan turning every catalogue off so the
	// sources' answers can be compared over one and the same directory.
	open := func(t *testing.T, what string, recycle, forceScan bool) (State, []core.SegmentID) {
		t.Helper()
		r, segs := plant(t, recycle)
		deps := r.runDeps()
		deps.rebuildForceScan = forceScan
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over %s: %v", what, err)
		}
		return st, segs
	}

	st, segs := open(t, "the recycled directory", true, false)
	victim := segs[len(segs)-1]

	// No entry of the old life's footer lands in the index: the recycled
	// take's entries are exactly the honest plant's — a stale trailer fed
	// through a catalogue would reattach the old life's fill of
	// workFiles[3] and differ here.
	honest, _ := open(t, "the honest plant without the old bytes", false, false)
	if !sameEntries(entriesOf(honest.Rebuilt.Index), entriesOf(st.Rebuilt.Index)) {
		t.Errorf("Run: the recycled take's rebuild differs from the plant without the old bytes: an entry of the old life's footer landed in the index")
	}

	// ByCatalog excludes the recycled segment: its named neighbours kept
	// their catalogues while the take, the stream's newest, scanned.
	if got, want := st.Rebuilt.ByCatalog, len(segs)-1; got != want {
		t.Fatalf("Run rebuilt %d segments by catalog, want %d: the recycled take's stale trailer was read as a catalogue", got, want)
	}
	sc, ok := scanOf(st, victim)
	if !ok {
		t.Fatalf("Run: the scans name no segment %d, want the recycled take", victim)
	}
	if sc.SealedByCatalog {
		t.Errorf("Run: segment %d was rebuilt by catalog, want the scan: a recycle's take is the stream's newest, which no successor names", victim)
	}

	// The stale trailer was never read as a catalogue: with every catalogue
	// turned off the same directory builds the identical index.
	forced, _ := open(t, "the forced scan of the recycled directory", true, true)
	if !sameEntries(entriesOf(st.Rebuilt.Index), entriesOf(forced.Rebuilt.Index)) {
		t.Errorf("Run: the forced scan of the recycled directory built a different index than the open over it, want one and the same")
	}
}

// TestCatalogDemotesAFreshIdTakeWithAStaleTail holds the trailer's seal
// marker name-check through the open, in the shape the recycle rule leaves:
// the take draws a fresh id above every id the directory names, and the old
// life it reclaims — its marker, footer and trailer — names the old id, so
// none of it verifies for the fresh one. The take is sealed at a crash (a
// marker of its own, no footer), the crash that dropped the recycle's
// trailer zeroing keeps the old life's tail in place, and a successor names
// the fresh id, so the open offers the take its catalogue: its trailer is
// the old life's, honest as a trailer and naming the old life's footer, and
// the one thing that keeps that footer from feeding as the take's
// catalogue is the seal marker at the footer's own offset, which names the
// old id. The open succeeds, the take is demoted to the scan, no entry of
// the old life lands in the index — the honest plant, the take a completed
// recycle leaves, builds the same index — and a forced scan of the same
// directory is identical.
func TestCatalogDemotesAFreshIdTakeWithAStaleTail(t *testing.T) {
	steps := catalogSteps()

	// The take's record: a file and extent the old life never touches, of a
	// fresh notebook — under a fresh id, nothing of the old life carries
	// over but the bytes.
	notebook := &stepNotebook{}
	take := step{kind: 1, file: workFiles[0], off: 256, n: 16, tag: tagOf(workFiles[0])}
	take.seq = notebook.nextSeq()
	take.version = notebook.nextVersion()
	take.payload = make([]byte, take.n)
	for i := range take.payload {
		take.payload[i] = byte(i + 1)
	}
	take.mtime = 1

	// plant grows the chained workload, seals its newest segment — the old
	// life the recycle reclaims — and hands it to the take under a fresh id
	// above every id the directory names, as the recycle rule draws it. The
	// take's header and record overwrite the old life's head, and its own
	// seal marker sits just past its record — sealed at a crash, so no
	// footer of its own was written. stale true keeps the old life's marker,
	// footer and trailer at their own offsets past the take: the recycle's
	// trailer zeroing is the one write the crash dropped, and the old tail's
	// marker, footer and trailer all name the old id. stale false plants the
	// honest directory — the same take over a completed recycle, none of
	// the old bytes in the file. A successor names the fresh id, so the open
	// offers the take its catalogue.
	plant := func(t *testing.T, stale bool) (*rig, []core.SegmentID, core.SegmentID) {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		// Five steps a segment, so the recycled one holds three records:
		// room for the take and its own marker below the old life's tail.
		segs, ents := r.plantSealed(steps, 5, true)
		if len(segs) < 3 {
			t.Fatalf("fixture: the workload planted %d segments, want the recycled one beside named neighbours", len(segs))
		}
		victim := segs[len(segs)-1] // the old life
		r.sealSeg(victim, ents[victim])
		old := r.fileBytes(format.SegmentName(victim))
		oldMarkerOff := len(old) - raw.TrailerSize - raw.CatalogEntrySize*len(ents[victim]) - format.SealMarkerSize

		fresh := victim + 1
		b := raw.SegmentHeader{Identity: journalID, ID: uint64(fresh), Predecessor: uint64(victim - 1)}.Bytes()
		b = appendStep(b, fresh, take)
		markerOff := len(b)
		b = append(b, raw.SealMarker{Identity: journalID, Segment: uint64(fresh), Offset: uint64(markerOff)}.Bytes()...)
		if stale {
			// The take's own marker, then the old life's tail at its own
			// offsets: the old marker, footer and trailer name the old id.
			if len(b) > oldMarkerOff {
				t.Fatalf("fixture: the take runs to %d, past the old marker at %d", len(b), oldMarkerOff)
			}
			b = append(b, make([]byte, oldMarkerOff-len(b))...)
			b = append(b, old[oldMarkerOff:]...)
		}
		// The recycle's rename, done the other way round so the seam never
		// sees both names: the old name is gone, the fresh one holds the take.
		if err := r.seam.Remove(format.SegmentName(victim)); err != nil {
			t.Fatalf("fixture: remove %s: %v", format.SegmentName(victim), err)
		}
		r.plantSeg(fresh, b)
		r.plantSeg(fresh+1, raw.SegmentHeader{Identity: journalID, ID: uint64(fresh + 1), Predecessor: uint64(fresh)}.Bytes())
		return r, segs, fresh
	}

	open := func(t *testing.T, what string, stale, forceScan bool) (State, []core.SegmentID, core.SegmentID) {
		t.Helper()
		r, segs, fresh := plant(t, stale)
		deps := r.runDeps()
		deps.rebuildForceScan = forceScan
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run over %s: %v", what, err)
		}
		return st, segs, fresh
	}

	st, segs, fresh := open(t, "the recycled directory", true, false)

	// The marker check demotes the take: its named neighbours kept their
	// catalogues while its stale trailer, whose seal marker names the old
	// id, fed nothing.
	if got, want := st.Rebuilt.ByCatalog, len(segs)-1; got != want {
		t.Fatalf("Run rebuilt %d segments by catalog, want %d: the fresh take's stale trailer was read as a catalogue", got, want)
	}
	sc, ok := scanOf(st, fresh)
	if !ok {
		t.Fatalf("Run: the scans name no segment %d, want the fresh take", fresh)
	}
	if sc.SealedByCatalog {
		t.Errorf("Run: segment %d was rebuilt by catalog, want the scan: a trailer whose seal marker does not verify for this segment never feeds", fresh)
	}

	// No entry of the old life lands in the index: the take's rebuild is
	// exactly the honest plant's — the old life's footer fed through a
	// catalogue would attach its records under the fresh id and differ here.
	honest, _, _ := open(t, "the honest plant without the old bytes", false, false)
	if !sameEntries(entriesOf(honest.Rebuilt.Index), entriesOf(st.Rebuilt.Index)) {
		t.Errorf("Run: the fresh take's rebuild differs from the plant without the old bytes: an entry of the old life's footer landed in the index")
	}

	// The stale tail fed nothing the walk does not: with every catalogue
	// turned off the same directory builds the identical index.
	forced, _, _ := open(t, "the forced scan of the recycled directory", true, true)
	if !sameEntries(entriesOf(st.Rebuilt.Index), entriesOf(forced.Rebuilt.Index)) {
		t.Errorf("Run: the forced scan of the recycled directory built a different index than the open over it, want one and the same")
	}
}

// TestCatalogRefusesAnUnreadableFooterRead holds the fault rule through the
// catalogue: a read of the trailer, the seal marker or a record the footer
// names that no bounded attempt completes refuses the open on the collected
// range — never demoted to a scan of bytes nobody read, never counted by
// catalogue — and the bound's attempts are exactly its three, after the
// healthy reads the file had before the faulted one.
func TestCatalogRefusesAnUnreadableFooterRead(t *testing.T) {
	for _, row := range []struct {
		name  string
		from  int    // the first call of the file the fault fails
		prior int    // the healthy reads the file had before the faulted one
		where string // which read the fault lands on, for the range it names
	}{
		{"the trailer read", 2, 1, "trailer"},
		{"the seal marker read", 3, 2, "marker"},
		{"a record read the footer names", 5, 4, "record"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			segs, ents := r.plantSealed(catalogSteps(), stepsPerSegment, true)
			// The victim holds the clone target at its first record, so the
			// record the footer names and the rebuild reads back is the
			// first read past the footer's own.
			victim := segs[1]
			name := format.SegmentName(victim)
			b := r.fileBytes(name)
			size := int64(len(b))
			footerOff := size - raw.TrailerSize - raw.CatalogEntrySize*int64(len(ents[victim]))
			var want core.SegmentRange
			switch row.where {
			case "trailer":
				want = core.SegmentRange{Segment: victim, Off: size - raw.TrailerSize, Len: int64(raw.TrailerSize)}
			case "marker":
				want = core.SegmentRange{Segment: victim, Off: footerOff - format.SealMarkerSize, Len: format.SealMarkerSize}
			case "record":
				want = core.SegmentRange{Segment: victim, Off: ents[victim][0].RecordOffset, Len: format.RecordHeaderSize}
			}
			if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, name, row.from, 0, syscall.EIO)); err != nil {
				t.Fatalf("fixture: schedule: %v", err)
			}
			before := r.fileBytes(name)

			st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			var ure *core.UnreadableRangesError
			if !errors.As(err, &ure) {
				t.Fatalf("Run with %s faulting on every attempt got err %v, want an UnreadableRangesError", row.name, err)
			}
			if !slices.Contains(ure.Ranges, want) {
				t.Errorf("Run: the refusal names ranges %v, want one to be %+v", ure.Ranges, want)
			}
			if got := st.Rebuilt.ByCatalog; got != 0 {
				t.Errorf("Run: the refused open counted %d segments by catalog, want none", got)
			}
			if got, wantCalls := r.seam.CallsFor(kit.OpRead, name), row.prior+readretry.Attempts; got != wantCalls {
				t.Errorf("Run: the faulted read made %d attempts, want the %d healthy reads before it and the bound's %d",
					got, row.prior, readretry.Attempts)
			}
			if after := r.fileBytes(name); !slices.Equal(before, after) {
				t.Errorf("Run: the refused read changed the segment file, want the bytes it started with")
			}
		})
	}
}
