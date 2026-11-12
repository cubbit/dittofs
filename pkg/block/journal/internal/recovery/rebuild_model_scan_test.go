package recovery_test

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/model"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// modelID is the journal identity the planted format file names.
var modelID = core.JournalID{'o', 'w', 'n', '-', 'i', 'd', 'e', 'n', 't', 'i', 't', 'y'}

// modelInstall is the installation ID the planted format file names.
var modelInstall = core.InstallationID{'k', 'i', 't'}

// cloneShare and cloneFile are a clone target's source ids, shared by every
// clone the workload plants.
var (
	cloneShare = core.ShareID{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0}
	cloneFile  = [16]byte{0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf}
)

// workload is the check's seeded workload: the files it writes, the records
// to plant in the order the check draws them, and the model operations of
// the same records, so the state the model derives is what a journal that
// wrote exactly these records would serve.
type workload struct {
	files []journal.FileID
	owned map[journal.FileID]core.ShareTag
	bytes map[journal.FileID]map[int64]byte
	plant []raw.Record
	apply []func(m *model.Model) error
	// mtimes names, at each record's location — its segment and its own
	// offset there — the mtime a write named and a fill's zero, which is
	// what the journal's own Since reads from the record's body. Filled at
	// plant time, when the placement decides each location.
	mtimes  map[core.SegmentID]map[uint32]int64
	high    core.Version // the highest version drawn
	nextSeq uint64
}

func newWorkload() *workload {
	return &workload{
		owned:  map[journal.FileID]core.ShareTag{},
		bytes:  map[journal.FileID]map[int64]byte{},
		mtimes: map[core.SegmentID]map[uint32]int64{},
	}
}

// file adds a file of share tag and returns it.
func (w *workload) file(i byte, tag core.ShareTag) journal.FileID {
	id := journal.NewFileID([16]byte{0x21, 15: i})
	w.files = append(w.files, id)
	w.owned[id] = tag
	w.bytes[id] = map[int64]byte{}
	return id
}

// numbers draws the pair the next record carries: a version above every one
// drawn before it, and a sequence number from one counter, as the journal's
// own writer issues them.
func (w *workload) numbers() (core.Version, uint64) {
	w.high.Counter++
	w.nextSeq++
	return w.high, w.nextSeq
}

// write records a write of p at off: the raw record to plant, the model
// operation of the same extent and bytes, and the bytes the view serves the
// model's reads from. Every write names mtime 1.
func (w *workload) write(id journal.FileID, off int64, payload []byte) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 1, FileID: id.Bytes(), Offset: uint64(off), Length: uint64(len(payload)),
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
		Write: &raw.WriteBody{MTime: 1, Payload: payload},
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.WriteAt(j_Share(w.owned[id]), id, off, payload, 1, j_Ver(v), nil)
	})
	w.note(id, off, payload)
}

// fill records a fill of p at off: filled content enters the journal already
// offloaded, and the model sees the same.
func (w *workload) fill(id journal.FileID, off int64, payload []byte) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 2, FileID: id.Bytes(), Offset: uint64(off), Length: uint64(len(payload)),
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
		Fill: &raw.FillBody{Payload: payload},
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Fill(j_Share(w.owned[id]), id, off, payload, j_Ver(v), j_Ver(v), nil)
	})
	w.note(id, off, payload)
}

// note remembers the payload bytes the view serves the model's reads from.
func (w *workload) note(id journal.FileID, off int64, payload []byte) {
	for i, b := range payload {
		w.bytes[id][off+int64(i)] = b
	}
}

// release records a release of the extent: the index replays it as an
// unconditional cut, and the model stops holding the same bytes.
func (w *workload) release(id journal.FileID, off, n int64) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 3, FileID: id.Bytes(), Offset: uint64(off), Length: uint64(n),
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Release(j_Share(w.owned[id]), id, []journal.Extent{{Off: off, Len: n}}, nil)
	})
}

// truncate records a truncate to size: Off is the new size, Len the removal
// to the end of the file.
func (w *workload) truncate(id journal.FileID, size int64) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 4, FileID: id.Bytes(), Offset: uint64(size), Length: 0,
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Truncate(j_Share(w.owned[id]), id, size, j_Ver(v), nil)
	})
}

// deallocate records a deallocate of the extent.
func (w *workload) deallocate(id journal.FileID, off, n int64) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 5, FileID: id.Bytes(), Offset: uint64(off), Length: uint64(n),
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Deallocate(j_Share(w.owned[id]), id, journal.Extent{Off: off, Len: n}, j_Ver(v), nil)
	})
}

// delete records a delete of the whole file.
func (w *workload) delete(id journal.FileID) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 6, FileID: id.Bytes(), Offset: 0, Length: 0,
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Delete(j_Share(w.owned[id]), id, j_Ver(v), nil)
	})
}

// clone records a clone target and returns the spec its marker must show.
// The model has no clone operation of its own — its extent removes like a
// deallocate — so the check drives the model with a deallocate and holds
// the clone target's own marker against the read-out directly.
func (w *workload) clone(id journal.FileID, dst, n int64) (core.CloneSpec, core.Version) {
	v, seq := w.numbers()
	w.plant = append(w.plant, raw.Record{
		Kind: 7, FileID: id.Bytes(), Offset: uint64(dst), Length: uint64(n),
		Seq: seq, Version: raw.Version{Counter: v.Counter}, Tag: uint32(w.owned[id]),
		Clone: &raw.CloneBody{Share: cloneShare, File: cloneFile, Off: 0x101, AsOf: raw.Version{Counter: 9}},
	})
	w.apply = append(w.apply, func(m *model.Model) error {
		return m.Deallocate(j_Share(w.owned[id]), id, journal.Extent{Off: dst, Len: n}, j_Ver(v), nil)
	})
	return core.CloneSpec{
		SrcShare: cloneShare,
		Src:      core.NewFileID(cloneFile),
		SrcOff:   0x101, DstOff: dst, Len: n,
		AsOf: core.Version{Counter: 9},
	}, v
}

// j_Share and j_Ver convert between the core names the record builders take
// and the public ones the model takes, which are one value under two.
func j_Share(t core.ShareTag) journal.ShareTag { return journal.ShareTag(t) }
func j_Ver(v core.Version) journal.Version     { return journal.Version(v) }

// modelRig is the external check's own rig: a seam over a temporary
// directory, the clock the bound reads, and the fixture's format file. The
// in-package rig is unreachable from here, so the check drives the same
// fixture shape through the seam directly.
type modelRig struct {
	t    *testing.T
	seam *kit.Seam
	clk  kit.DrivenClock
}

func newModelRig(t *testing.T) *modelRig {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("fixture: new seam: %v", err)
	}
	t.Cleanup(seam.Abandon)
	return &modelRig{t: t, seam: seam, clk: kit.NewStopwatchClock()}
}

// write makes the file name from whole bytes, as a synced writer leaves it.
func (r *modelRig) write(name string, b []byte) {
	r.t.Helper()
	f, err := r.seam.Create(name)
	if err != nil {
		r.t.Fatalf("fixture: create %s: %v", name, err)
	}
	if len(b) > 0 {
		if _, err := f.WriteAt(b, 0); err != nil {
			r.t.Fatalf("fixture: write %s: %v", name, err)
		}
		if err := f.Sync(); err != nil {
			r.t.Fatalf("fixture: sync %s: %v", name, err)
		}
	}
	if err := f.Close(); err != nil {
		r.t.Fatalf("fixture: close %s: %v", name, err)
	}
	if err := r.seam.SyncDir(); err != nil {
		r.t.Fatalf("fixture: sync dir: %v", err)
	}
}

// writeFormat writes the fixture's format file, version 1.
func (r *modelRig) writeFormat() {
	r.t.Helper()
	r.write(format.FormatFileName, format.AppendFormatFile(nil, format.FormatFile{
		Identity: modelID, Installation: modelInstall,
	}))
}

// plantWorkload writes the workload's records into segment files, six
// records to a segment, and gives each record the identity, segment and own
// offset its header verifies by.
func (r *modelRig) plantWorkload(w *workload) {
	r.t.Helper()
	const perSeg = 6
	id := core.SegmentID(1)
	b := raw.SegmentHeader{Identity: modelID, ID: uint64(id)}.Bytes()
	inSeg := 0
	for i := range w.plant {
		w.plant[i].Identity = modelID
		w.plant[i].Segment = uint64(id)
		w.plant[i].RecordOffset = uint32(len(b))
		if w.plant[i].Kind == 1 || w.plant[i].Kind == 2 {
			mt := int64(0)
			if w.plant[i].Kind == 1 {
				mt = w.plant[i].Write.MTime
			}
			if w.mtimes[id] == nil {
				w.mtimes[id] = map[uint32]int64{}
			}
			w.mtimes[id][w.plant[i].RecordOffset] = mt
		}
		b = append(b, w.plant[i].Bytes()...)
		inSeg++
		if inSeg < perSeg && i != len(w.plant)-1 {
			continue
		}
		r.write(format.SegmentName(id), b)
		if i == len(w.plant)-1 {
			return
		}
		id++
		b = raw.SegmentHeader{Identity: modelID, ID: uint64(id)}.Bytes()
		inSeg = 0
	}
}

// TestRunRebuildByScanMatchesTheModel drives a fixed workload — writes,
// fills, releases, truncates, deallocates, deletes and a clone target, over
// three files of two shares, across several segments — through the open's
// rebuild, and every file the model holds must read back through the
// rebuilt index exactly as the model derives it: entries, versions, record
// locations and markers (R12). The clone target the model has no operation
// for is driven as the deallocate its extent is, and its marker and spec
// are held against the read-out directly.
//
// The check lives in the external test package because the model imports
// the journal's public package, which imports recovery, so an in-package
// test could not import both.
func TestRunRebuildByScanMatchesTheModel(t *testing.T) {
	w := newWorkload()
	a := w.file(1, 1)
	b := w.file(2, 1)
	c := w.file(3, 2)

	w.write(a, 0, pattern(16, 1))
	w.fill(a, 64, pattern(16, 2))
	spec, specV := w.clone(a, 8, 16)
	w.write(b, 32, pattern(24, 3))
	w.truncate(a, 80)
	w.deallocate(b, 32, 8)
	w.fill(c, 0, pattern(12, 4))
	w.release(c, 0, 8)
	w.write(a, 100, pattern(8, 5)) // past the truncate: the file holds bytes again
	w.delete(b)
	w.fill(c, 32, pattern(16, 6))
	w.truncate(b, 0) // a truncate of a deleted file: the marker stays

	st := rebuildAndCompare(t, w)

	// The clone target's marker, held against the read-out directly: its
	// kind, extent, version and spec are what the rebuild must show.
	entries := st.Rebuilt.Index.FileEntries(cFile(a))
	var marker *index.Entry
	for i := range entries {
		e := &entries[i]
		if e.Kind == index.EntryMarker && e.Removal == core.ChangeCloneTarget &&
			e.Extent.Off == spec.DstOff && e.Extent.Len == spec.Len {
			marker = e
		}
	}
	if marker == nil {
		t.Fatalf("Run: the rebuilt index holds no clone-target marker at [%d,+%d), want one", spec.DstOff, spec.Len)
	}
	if marker.Clone == nil || *marker.Clone != spec {
		t.Errorf("Run: the clone-target marker's spec got %+v, want %+v", marker.Clone, spec)
	}
	if marker.Version != specV {
		t.Errorf("Run: the clone-target marker's version got %v, want %v", marker.Version, specV)
	}
}

// rebuildAndCompare plants the workload, runs the open's rebuild over it and
// holds every file of both shares against the model's derivation of the same
// operations, returning the state the rebuild gave.
func rebuildAndCompare(t *testing.T, w *workload) recovery.State {
	t.Helper()
	r := newModelRig(t)
	r.writeFormat()
	r.plantWorkload(w)

	st, err := recovery.Run(context.Background(), recovery.Deps{FS: r.seam.FS(), Clock: r.clk}, core.Version{}, modelID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := model.New()
	for _, apply := range w.apply {
		if err := apply(m); err != nil {
			t.Fatalf("fixture: the model of the workload: %v", err)
		}
	}

	// The opened journal is a reopen of these records: its floor buckets
	// start at the highest version on disk, so a file's read reports its
	// bucket wherever its own change was older — the reopen allowance the
	// model takes from Reopen.
	if err := m.Reopen(j_Ver(w.high)); err != nil {
		t.Fatalf("fixture: the model's reopen: %v", err)
	}
	for _, tag := range []core.ShareTag{1, 2} {
		if err := m.Compare(context.Background(), j_Share(tag), shareView{w: w, ix: st.Rebuilt.Index, tag: tag}); err != nil {
			t.Errorf("Run: the rebuild through share %d differs from the model: %v", tag, err)
		}
	}
	return st
}

// TestRunRebuildByScanMatchesTheModelOverSeededWorkloads draws a workload
// from each of a few fixed seeds — writes, fills, releases, truncates,
// deallocates, deletes and clone targets over three files of two shares —
// and holds the rebuild against the model for each.
func TestRunRebuildByScanMatchesTheModelOverSeededWorkloads(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 5, 8} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed*2+1))
			w := newWorkload()
			files := []journal.FileID{w.file(1, 1), w.file(2, 1), w.file(3, 2)}
			// Fills land in a region of their own, never over a write: the
			// journal fills only what it does not hold, and the model refuses
			// the rest.
			const fillBase = 1 << 12
			for i := range 60 {
				f := files[rng.IntN(len(files))]
				off := int64(rng.IntN(32)) * 8
				n := 8 + int64(rng.IntN(8))*8
				switch rng.IntN(7) {
				case 0:
					w.write(f, off, pattern(int(n), byte(i)))
				case 1:
					w.fill(f, fillBase+int64(i)*64, pattern(int(n), byte(i)))
				case 2:
					// The model releases only offloaded bytes, so the draw
					// fills the extent and releases a part of it at once.
					w.fill(f, fillBase+int64(i)*64, pattern(int(n), byte(i)))
					w.release(f, fillBase+int64(i)*64, 8)
				case 3:
					w.truncate(f, off)
				case 4:
					w.deallocate(f, off, n)
				case 5:
					w.clone(f, off, n)
				case 6:
					if rng.IntN(4) == 0 { // a delete is rarer: it empties the file
						w.delete(f)
					} else {
						w.write(f, off, pattern(int(n), byte(i)))
					}
				}
			}
			rebuildAndCompare(t, w)
		})
	}
}

// cFile converts the public FileID to the core one the index takes, one
// value under two names.
func cFile(id journal.FileID) core.FileID { return core.NewFileID(id.Bytes()) }

// pattern returns n payload bytes no other call of the workload writes,
// salted by s.
func pattern(n int, s byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = s<<4 | byte(i)
	}
	return p
}

// shareView serves the model's reads of one share from the rebuilt index
// and the workload's planted bytes: what the wired journal's inspection and
// handles would answer over the same rebuilt state.
type shareView struct {
	w   *workload
	ix  *index.Index
	tag core.ShareTag
}

// mtime names the mtime of the record at the location the read-out entry
// names, the write's own and a fill's zero.
func (v shareView) mtime(e index.Entry) int64 {
	return v.w.mtimes[e.Record.Segment][e.Record.Offset]
}

// ReadAt serves p from the pieces the index holds, reading each piece's
// bytes from the payload the workload planted for them, and reports the
// exact missing.
func (v shareView) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	e := core.Extent{Off: off, Len: int64(len(p))}
	placed, missing := v.ix.Span(cFile(id), e)
	held := make([]journal.Held, 0, len(placed))
	for _, pl := range placed {
		held = append(held, journal.Held{Extent: journal.Extent(pl.Extent), Version: j_Ver(pl.Version)})
		for i := pl.Extent.Off; i < pl.Extent.Off+pl.Extent.Len; i++ {
			p[i-off] = v.w.bytes[id][i]
		}
	}
	miss := make([]journal.Extent, len(missing))
	for i, me := range missing {
		miss[i] = journal.Extent(me)
	}
	return len(p), miss, held, j_Ver(v.ix.AsOf(cFile(id))), nil
}

// Extents returns the file's held extents, contiguous ones merged.
func (v shareView) Extents(id journal.FileID) ([]journal.Extent, error) {
	var out []journal.Extent
	for _, e := range v.ix.FileEntries(cFile(id)) {
		if e.Kind != index.EntryHeld {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Off+out[n-1].Len == e.Extent.Off {
			out[n-1].Len += e.Extent.Len
			continue
		}
		out = append(out, journal.Extent(e.Extent))
	}
	return out, nil
}

// Since yields the file's changes above from, derived from the read-out the
// index exports — pieces the version filter keeps are writes, and markers
// above from are removals — ordered as the journal's own Since is: by
// version, markers before held extents at one version, then by offset. The
// mtime of a write the index does not keep; every write of this workload
// named 1, so that is what is yielded.
func (v shareView) Since(ctx context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error] {
	return func(yield func(journal.Change, error) bool) {
		type item struct {
			c      journal.Change
			marker bool
		}
		var items []item
		for _, e := range v.ix.FileEntries(cFile(id)) {
			if e.Version.Compare(core.Version(from)) <= 0 {
				continue
			}
			switch e.Kind {
			case index.EntryHeld:
				items = append(items, item{c: journal.Change{Kind: journal.ChangeWrite, Extent: journal.Extent(e.Extent), Version: j_Ver(e.Version), Mtime: v.mtime(e)}})
			case index.EntryMarker:
				kind := journal.ChangeKind(e.Removal)
				if e.Removal == core.ChangeCloneTarget {
					// The model drives a clone target as the deallocate its
					// extent is; the marker yields under the same name.
					kind = journal.ChangeDeallocate
				}
				items = append(items, item{c: journal.Change{Kind: kind, Extent: journal.Extent(e.Extent), Version: j_Ver(e.Version)}, marker: true})
			}
		}
		slices.SortStableFunc(items, func(a, b item) int {
			if c := a.c.Version.Compare(b.c.Version); c != 0 {
				return c
			}
			if a.marker != b.marker {
				if b.marker {
					return -1
				}
				return 1
			}
			return cmp.Compare(a.c.Extent.Off, b.c.Extent.Off)
		})
		for _, it := range items {
			if !yield(it.c, nil) {
				return
			}
		}
	}
}

// Files lists the share's files: those the read-out sees with a held entry
// or a marker under this share's tag.
func (v shareView) Files() iter.Seq[journal.FileID] {
	return func(yield func(journal.FileID) bool) {
		for _, f := range v.w.files {
			if v.w.owned[f] != v.tag {
				continue
			}
			visible := false
			for _, e := range v.ix.FileEntries(cFile(f)) {
				if e.Kind == index.EntryHeld || e.Kind == index.EntryMarker {
					visible = true
					break
				}
			}
			if visible && !yield(f) {
				return
			}
		}
	}
}

// DirtyFiles lists the share's files holding content whose offloaded bit is
// unset; the times are zero, the workload having no clock of its own.
func (v shareView) DirtyFiles() iter.Seq2[journal.FileID, time.Time] {
	return func(yield func(journal.FileID, time.Time) bool) {
		for _, f := range v.w.files {
			if v.w.owned[f] != v.tag {
				continue
			}
			dirty := false
			for _, e := range v.ix.FileEntries(cFile(f)) {
				if e.Kind == index.EntryHeld && !e.Offloaded {
					dirty = true
					break
				}
			}
			if dirty && !yield(f, time.Time{}) {
				return
			}
		}
	}
}

// Index maps the read-out onto the journal's inspection entries, dropping
// the fields the public entry does not carry: Skip, By, Removal and the
// entry-order Clone, for only the public Kind, Extent, Version, Tag,
// Location and Offloaded reach the model's comparisons.
func (v shareView) Index(id journal.FileID) ([]journal.IndexEntry, error) {
	var out []journal.IndexEntry
	for _, e := range v.ix.FileEntries(cFile(id)) {
		out = append(out, journal.IndexEntry{
			Kind:      journal.EntryKind(e.Kind),
			Extent:    journal.Extent(e.Extent),
			Version:   j_Ver(e.Version),
			Tag:       journal.ShareTag(e.Tag),
			Location:  journal.RecordRef{Segment: e.Record.Segment, Offset: int64(e.Record.Offset)},
			Offloaded: e.Offloaded,
		})
	}
	return out, nil
}
