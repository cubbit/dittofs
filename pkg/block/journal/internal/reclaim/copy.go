package reclaim

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
)

// Domains is the file append domains the copy takes per file — one at a
// time — around its liveness check and append (RFC 1 §8.3). The datapath's
// own table satisfies it.
type Domains interface {
	Lock(id core.FileID) (unlock func())
}

// Corrupter reports one corrupt source record to the durability layer. It
// runs with the record's file append domain already held.
type Corrupter func(ctx context.Context, bad BadSource) error

// BadSource is one source record whose bytes the pass read but could not
// verify, so its copies are dropped instead. Extent is the record header's
// own where the header decoded; where the header's bytes were read but did
// not decode, it is the hull of the pieces the index holds at the record.
type BadSource struct {
	Tag     core.ShareTag
	File    core.FileID
	Record  index.RecordAt
	Seq     core.Seq
	Version core.Version
	Extent  core.Extent
}

// Copied is one copy the pass appended: what it holds of which file, the
// fresh placement it landed at, and the record it was copied from.
type Copied struct {
	File     core.FileID
	Tag      core.ShareTag
	Kind     format.Kind
	Version  core.Version
	Seq      core.Seq
	Extent   core.Extent
	At       time.Time
	From, To index.RecordAt
	Loc      segment.Location
}

// Result is what one Copy appended, and the sources it dropped as corrupt.
type Result struct {
	Copies  []Copied
	Corrupt []BadSource
}

// Copier copies the live content of a claimed segment into fresh records.
type Copier struct {
	ix       *index.Index
	counters *index.Counters
	streams  *segment.Streams
	un       *unread.Set
	tracker  *Tracker
	dom      Domains
	corrupt  Corrupter
	clock    core.Clock
	log      *slog.Logger
}

// NewCopier builds the pass over one journal's pieces. dom is the append
// domain table; corrupt is called per corrupt source with the file's domain
// held; a nil corrupt drops corrupt sources without reporting them.
func NewCopier(ix *index.Index, c *index.Counters, st *segment.Streams, un *unread.Set, tr *Tracker, dom Domains, corrupt Corrupter, clk core.Clock, log *slog.Logger) *Copier {
	return &Copier{ix: ix, counters: c, streams: st, un: un, tracker: tr, dom: dom, corrupt: corrupt, clock: clk, log: log}
}

// Copy copies the live content of seg, whose claim the caller holds. The
// copies are appended but published nowhere: repoint and retirement are the
// pass's. The whole source is read and checksum-verified before any part of
// it is copied. A source unread after its reads abandons the unit with
// nothing appended; a corrupt source is dropped through corrupt with the
// file's append domain held.
func (c *Copier) Copy(ctx context.Context, seg core.SegmentID) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// Every stream reaches the one ledger the appended copies draw spares
	// from, so the first names it.
	sources, err := c.read(ctx, c.streams.Stream(0).Set(), seg)
	if err != nil {
		return Result{}, err
	}
	return c.append(ctx, sources)
}

// source is one record the pass holds: where it sits, the file whose pieces
// name it, and its verified header and body. bad marks a record whose bytes
// were read but did not verify — a body that fails its checksum, or a header
// that does not decode at its place, which leaves every header field zero —
// whose pieces the pass drops instead.
type source struct {
	rec  index.RecordAt
	file core.FileID
	h    format.RecordHeader
	body format.RecordBody
	bad  bool
}

// read enumerates seg's live pieces from the in-memory state and then reads
// and verifies each distinct source record whole, holding no append domain.
// Each record's bytes come through readSource, the same read a later access
// of an unread record repeats.
func (c *Copier) read(ctx context.Context, set *segment.Set, seg core.SegmentID) ([]source, error) {
	// Phase one over the in-memory pieces alone, holding no domain: the
	// files whose entries name a record of seg, and the distinct records
	// behind them.
	fileOf := make(map[index.RecordAt]core.FileID)
	var found bool
	for _, row := range set.Snapshot() {
		if row.ID != seg {
			continue
		}
		found = true
		for tag, n := range row.Tags {
			if n == 0 {
				continue
			}
			for _, file := range c.ix.Files(tag) {
				for _, e := range c.ix.FileEntries(file) {
					if e.Record.Segment == seg {
						if _, ok := fileOf[e.Record]; !ok {
							fileOf[e.Record] = file
						}
					}
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("reclaim: segment %d is not in the set", seg)
	}
	recs := make([]index.RecordAt, 0, len(fileOf))
	for r := range fileOf {
		recs = append(recs, r)
	}
	slices.SortFunc(recs, func(a, b index.RecordAt) int { return cmp.Compare(a.Offset, b.Offset) })

	// Each source read whole, with no index call in flight and no domain
	// held: a read that returns no bytes is marked unread by Try and
	// abandons the unit below, while bytes that were read and did not verify
	// take the corrupt path there, never the unread one.
	out := make([]source, 0, len(recs))
	for _, rec := range recs {
		file := fileOf[rec]
		identity := c.streams.For(file).Identity()
		b, err := c.readSource(ctx, seg, file, rec)
		if err != nil {
			return nil, fmt.Errorf("reclaim: the source of segment %d: %w", seg, err)
		}
		// decision: an I/O error never reaches the corruption handler —
		// only bytes that were read and did not verify do (RFC 28 §3.3). A
		// read that returns no bytes is marked unread by Try and abandons
		// the unit; a body whose checksum fails, and equally a header whose
		// bytes were read and do not decode at their place — the unread
		// column is only for a read that returns no bytes at all — makes
		// the source corrupt instead, named by its header where the header
		// decodes and by the index identity where it cannot, whose extent is
		// the hull of the record's pieces and can cover less than the
		// record held. Overturned only if the RFC comes to drop an extent on
		// an I/O error, or to mark a read that returned bytes unread.
		src := source{rec: rec, file: file}
		if h, derr := format.DecodeRecordHeader(b[:format.RecordHeaderSize], identity, seg, int64(rec.Offset)); derr == nil {
			src.h = h
			if body, verr := format.VerifyRecordBody(b[format.RecordHeaderSize:], identity, h); verr == nil {
				src.body = body
			} else {
				src.bad = true
			}
		} else {
			src.bad = true
		}
		out = append(out, src)
	}
	return out, nil
}

// readSource reads one record of seg whole through the unread set, the same
// read the pass's first phase makes of every source: the closure Try runs
// per attempt acquires one segment ref, reads one whole record through it,
// and releases it before answering. The mark's length is the header's, the
// whole of the record the pass can name before it reads; the set holds the
// entry by its (segment, offset) identity.
func (c *Copier) readSource(ctx context.Context, seg core.SegmentID, file core.FileID, rec index.RecordAt) ([]byte, error) {
	identity := c.streams.For(file).Identity()
	return c.un.Try(ctx, unread.Ref{
		SegmentRange: core.SegmentRange{Segment: seg, Off: int64(rec.Offset), Len: format.RecordHeaderSize},
		Kind:         unread.Content,
		File:         file,
	}, func() ([]byte, error) {
		ref, err := c.streams.For(file).AcquireRead(seg)
		if err != nil {
			return nil, err
		}
		defer ref.Release()
		head := make([]byte, format.RecordHeaderSize)
		if _, err := ref.ReadAt(head, int64(rec.Offset)); err != nil {
			return nil, err
		}
		h, err := format.DecodeRecordHeader(head, identity, seg, int64(rec.Offset))
		if err != nil {
			// The header's bytes were read and do not decode: the read
			// itself failed nowhere, so Try marks nothing; the truncated
			// answer names the source corrupt.
			return head, nil
		}
		body := make([]byte, h.BodySize())
		if _, err := ref.ReadAt(body, int64(rec.Offset)+format.RecordHeaderSize); err != nil {
			return nil, err
		}
		return append(head, body...), nil
	})
}

// append is the pass's second phase: one file at a time, at most one append
// domain held at any instant, each source's live pieces read from the index
// and copy records appended for them.
func (c *Copier) append(ctx context.Context, sources []source) (Result, error) {
	var res Result
	files := make([]core.FileID, 0, len(sources))
	seen := make(map[core.FileID]struct{}, len(sources))
	for _, s := range sources {
		if _, ok := seen[s.file]; !ok {
			seen[s.file] = struct{}{}
			files = append(files, s.file)
		}
	}
	slices.SortFunc(files, func(a, b core.FileID) int {
		aI, bI := a.Bytes(), b.Bytes()
		return bytes.Compare(aI[:], bI[:])
	})

	for _, file := range files {
		unlock := c.dom.Lock(file)
		srcs := make([]source, 0, len(sources))
		for _, s := range sources {
			if s.file == file {
				srcs = append(srcs, s)
			}
		}
		if err := c.appendFile(ctx, file, srcs, &res); err != nil {
			unlock()
			return res, err
		}
		unlock()
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// appendFile copies one file's live pieces under the file's append domain,
// which the caller holds. The pieces of every good source are read from the
// index again, under the domain, so a piece released since the pass
// enumerated is not copied; a source no piece names is skipped. The pieces
// plan into runs, and each run copies as one record whose payload is
// stitched piece by piece, whatever record or skip each piece came from.
func (c *Copier) appendFile(ctx context.Context, file core.FileID, srcs []source, res *Result) error {
	tag, ok := c.ix.Owner(file)
	if !ok {
		return fmt.Errorf("reclaim: file %v holds no owner share", file)
	}
	retained := c.ix.Retained(file)

	// A corrupt source is dropped before anything copies, with the file's
	// append domain held, never copied. Its identity is its header's own
	// where the header decoded; a journal issues every record a sequence
	// number from 1, so a zero one means the header's bytes were read but
	// did not decode, and the pieces the index holds at the record name it
	// instead — taken here, before corrupt drops them, and their extent can
	// span less than the record held.
	for i := range srcs {
		if !srcs[i].bad {
			continue
		}
		src := srcs[i]
		bad := BadSource{Tag: tag, File: file, Record: src.rec, Seq: src.h.Seq, Version: src.h.Version, Extent: src.h.Extent}
		if bad.Seq == 0 {
			pieces := c.ix.HeldAt(file, src.rec)
			for _, rt := range retained {
				if rt.Record == src.rec {
					pieces = append(pieces, rt.Piece)
				}
			}
			if len(pieces) == 0 {
				// decision: a record the index holds no piece of any more
				// is reported nowhere, corrupt and the result both silent:
				// every piece was released or superseded, so the record's
				// unverifiable bytes back nothing and an empty extent names
				// no loss to record (RFC 28 §3.3). Overturned only if a
				// caller is ruled to need the record acknowledged while
				// nothing holds it.
				continue
			}
			bad.Seq, bad.Version = pieces[0].Seq, pieces[0].Version
			lo, hi := pieces[0].Extent.Off, pieces[0].Extent.Off+pieces[0].Extent.Len
			for _, p := range pieces[1:] {
				lo, hi = min(lo, p.Extent.Off), max(hi, p.Extent.Off+p.Extent.Len)
			}
			bad.Extent = core.Extent{Off: lo, Len: hi - lo}
		}
		if c.corrupt != nil {
			if err := c.corrupt(ctx, bad); err != nil {
				return fmt.Errorf("reclaim: dropping the corrupt source of file %v: %w", file, err)
			}
		}
		res.Corrupt = append(res.Corrupt, bad)
	}

	var live []pieceInfo
	for i := range srcs {
		src := &srcs[i]
		if src.bad {
			continue
		}
		pieces := c.ix.HeldAt(file, src.rec)
		for _, rt := range retained {
			if rt.Record == src.rec {
				pieces = append(pieces, rt.Piece)
			}
		}
		for _, p := range pieces {
			live = append(live, pieceInfo{Piece: p, kind: src.h.Kind, mtime: src.body.MTime, src: src})
		}
	}
	slices.SortFunc(live, func(a, b pieceInfo) int {
		if n := cmp.Compare(a.Extent.Off, b.Extent.Off); n != 0 {
			return n
		}
		return cmp.Compare(a.Skip, b.Skip)
	})
	for _, r := range planRuns(live) {
		if err := ctx.Err(); err != nil {
			return err
		}
		seq, err := c.counters.NextSeq()
		if err != nil {
			return err
		}
		// A run shares kind, version and mtime across its pieces; a fill
		// stays a fill, its body carrying no mtime of its own.
		h := format.RecordHeader{
			Kind:    r.pieces[0].kind,
			File:    file,
			Extent:  r.extent,
			Seq:     seq,
			Version: r.pieces[0].Version,
			Tag:     tag,
		}
		payload := make([]byte, r.extent.Len)
		var n int
		for _, p := range r.pieces {
			n += copy(payload[n:], p.src.body.Payload[p.Skip:p.Skip+uint32(p.Extent.Len)])
		}
		body := format.RecordBody{MTime: r.pieces[0].mtime, Payload: payload}
		pl, err := c.streams.For(file).AppendRoll(h, body, segment.Roll{Repack: true})
		if err != nil {
			return err
		}
		if pl.N != r.extent.Len {
			return fmt.Errorf("reclaim: the copy of file %v at offset %d placed %d of %d bytes", file, r.extent.Off, pl.N, r.extent.Len)
		}
		res.Copies = append(res.Copies, Copied{
			File:    file,
			Tag:     tag,
			Kind:    h.Kind,
			Version: r.pieces[0].Version,
			Seq:     seq,
			Extent:  r.extent,
			At:      c.clock.Now(),
			From:    r.pieces[0].src.rec,
			To:      index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
			Loc:     pl.Loc,
		})
	}
	return nil
}

// pieceInfo is one live piece beside the two facts of its verified source the
// grouping reads: the record's content kind and the mtime its verified body
// carried. The source itself sits in src for the append step's stitch; the
// grouping never reads it.
type pieceInfo struct {
	index.Piece
	kind  format.Kind
	mtime int64
	src   *source
}

// run is one copy the plan makes: the file extent it covers, and the pieces
// it stitches in offset order, each carrying the record its bytes read from.
type run struct {
	extent core.Extent
	pieces []pieceInfo
}

// merges reports whether prev and next — neighbours in file order — copy as
// one record: next's extent begins where prev's ends, and the two share the
// keys one record's header can carry. The offloaded bit is a key, but two
// adjacent same-kind pieces with differing bits have no row: no writer marks
// the bit today.
func merges(prev, next pieceInfo) bool {
	return prev.Extent.Off+prev.Extent.Len == next.Extent.Off &&
		prev.Version == next.Version &&
		prev.kind == next.kind &&
		prev.Offloaded == next.Offloaded &&
		// Writes merge only at one mtime: a record's header spells the one
		// mtime its whole payload changed at, so two mtimes cannot ride in
		// one copy. A fill carries no mtime, so nothing separates two fills.
		(prev.kind != format.KindWrite || prev.mtime == next.mtime)
}

// planRuns groups file-sorted live pieces into copy runs: adjacent pieces
// (prev.Extent.End == next.Extent.Off) merge when they share version, kind,
// offloaded bit and — for writes — mtime. Kinds never mix in a merge, and a
// gap starts a new run (RFC 1 §4.2).
//
// decision: kinds never mix, so a fill beside a write at one version stays
// two copies. A merged fill would lose its bit at the next crash: one record
// carries one kind and one offloaded mark (RFC 28 §3.2), so the reopen could
// not set the bit the fill's half needs. Ceiling: files whose writes and
// fills alternate keep one index entry per run after a pass; revisit when
// such files reach the extent bound through repack copies, which RFC 28
// §2.3's record format cannot express a fix for today.
func planRuns(pieces []pieceInfo) []run {
	var out []run
	for i := 0; i < len(pieces); {
		j := i + 1
		for j < len(pieces) && merges(pieces[j-1], pieces[j]) {
			j++
		}
		out = append(out, run{
			extent: core.Extent{
				Off: pieces[i].Extent.Off,
				Len: pieces[j-1].Extent.Off + pieces[j-1].Extent.Len - pieces[i].Extent.Off,
			},
			pieces: pieces[i:j],
		})
		i = j
	}
	return out
}
