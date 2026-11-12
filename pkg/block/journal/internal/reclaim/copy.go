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

// BadSource is one source record the pass read whole but could not verify,
// so its copies are dropped instead. Extent is the record header's own.
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
// name it, and its verified header and body. bad marks a record that reads
// whole but fails its checksum, whose pieces the pass drops instead.
type source struct {
	rec  index.RecordAt
	file core.FileID
	h    format.RecordHeader
	body format.RecordBody
	bad  bool
}

// read enumerates seg's live pieces from the in-memory state and then reads
// and verifies each distinct source record whole, holding no append domain.
// The closure Try runs per attempt acquires one segment ref, reads the
// record's header and body through it, and releases it before answering.
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
	// held: a read that never returns bytes is marked unread by Try and
	// abandons the unit below.
	out := make([]source, 0, len(recs))
	for _, rec := range recs {
		file := fileOf[rec]
		identity := c.streams.For(file).Identity()
		// The mark's length is the header's, the whole of the record the
		// pass can name before it reads; the set holds the entry by its
		// (segment, offset) identity.
		b, err := c.un.Try(ctx, unread.Ref{
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
				return nil, err
			}
			body := make([]byte, h.BodySize())
			if _, err := ref.ReadAt(body, int64(rec.Offset)+format.RecordHeaderSize); err != nil {
				return nil, err
			}
			return append(head, body...), nil
		})
		if err != nil {
			return nil, fmt.Errorf("reclaim: the source of segment %d: %w", seg, err)
		}
		h, err := format.DecodeRecordHeader(b[:format.RecordHeaderSize], identity, seg, int64(rec.Offset))
		if err != nil {
			return nil, fmt.Errorf("reclaim: the source of segment %d: %w", seg, err)
		}
		src := source{rec: rec, file: file, h: h}
		if body, err := format.VerifyRecordBody(b[format.RecordHeaderSize:], identity, h); err == nil {
			src.body = body
		} else {
			src.bad = true
		}
		out = append(out, src)
	}
	return out, nil
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
// which the caller holds. Each source's pieces are read from the index
// again, under the domain, so a piece released since the pass enumerated is
// not copied; a source that no piece names is skipped.
func (c *Copier) appendFile(ctx context.Context, file core.FileID, srcs []source, res *Result) error {
	tag, ok := c.ix.Owner(file)
	if !ok {
		return fmt.Errorf("reclaim: file %v holds no owner share", file)
	}
	retained := c.ix.Retained(file)

	// decision: a source that reads whole but fails its checksum is dropped
	// through corrupt with the file's append domain held, never copied: the
	// corruption is reported under the same domain the writer republishes
	// in, so the record's index state changes hands in one step. Withdrawn
	// by a corrupter that takes the domain itself.
	for i := range srcs {
		if !srcs[i].bad {
			continue
		}
		src := srcs[i]
		bad := BadSource{Tag: tag, File: file, Record: src.rec, Seq: src.h.Seq, Version: src.h.Version, Extent: src.h.Extent}
		if c.corrupt != nil {
			if err := c.corrupt(ctx, bad); err != nil {
				return fmt.Errorf("reclaim: dropping the corrupt source of file %v: %w", file, err)
			}
		}
		res.Corrupt = append(res.Corrupt, bad)
	}

	for i := range srcs {
		src := srcs[i]
		if src.bad {
			continue
		}
		pieces := c.ix.HeldAt(file, src.rec)
		for _, rt := range retained {
			if rt.Record == src.rec {
				pieces = append(pieces, rt.Piece)
			}
		}
		if len(pieces) == 0 {
			continue
		}
		slices.SortFunc(pieces, func(a, b index.Piece) int {
			if n := cmp.Compare(a.Extent.Off, b.Extent.Off); n != 0 {
				return n
			}
			return cmp.Compare(a.Skip, b.Skip)
		})
		for _, run := range runs(pieces) {
			if err := ctx.Err(); err != nil {
				return err
			}
			seq, err := c.counters.NextSeq()
			if err != nil {
				return err
			}
			h := format.RecordHeader{
				Kind:    src.h.Kind,
				File:    file,
				Extent:  run.ext,
				Seq:     seq,
				Version: src.h.Version,
				Tag:     tag,
			}
			// A fill stays a fill: its body carries no mtime, so the
			// verified body's zero MTime is the copy's too.
			body := format.RecordBody{MTime: src.body.MTime, Payload: src.body.Payload[run.skip : run.skip+uint32(run.ext.Len)]}
			pl, err := c.streams.For(file).AppendRoll(h, body, segment.Roll{Repack: true})
			if err != nil {
				return err
			}
			if pl.N != run.ext.Len {
				return fmt.Errorf("reclaim: the copy of file %v at offset %d placed %d of %d bytes", file, run.ext.Off, pl.N, run.ext.Len)
			}
			res.Copies = append(res.Copies, Copied{
				File:    file,
				Tag:     tag,
				Kind:    h.Kind,
				Version: src.h.Version,
				Seq:     seq,
				Extent:  run.ext,
				At:      c.clock.Now(),
				From:    src.rec,
				To:      index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
				Loc:     pl.Loc,
			})
		}
	}
	return nil
}

// copyRun is one span of a record's payload copied as one record: the file
// extent it covers and where in the payload its first byte sits.
type copyRun struct {
	ext  core.Extent
	skip uint32
}

// runs groups pieces, in offset order, into runs whose extents are
// contiguous in the file and whose payload slices are contiguous in the
// record: each run is copied whole, as one record. A gap or an overlap in
// either starts a new run.
func runs(pieces []index.Piece) []copyRun {
	var out []copyRun
	for i := 0; i < len(pieces); {
		j := i + 1
		for j < len(pieces) &&
			pieces[j].Extent.Off == pieces[j-1].Extent.Off+pieces[j-1].Extent.Len &&
			pieces[j].Skip == pieces[j-1].Skip+uint32(pieces[j-1].Extent.Len) {
			j++
		}
		out = append(out, copyRun{
			ext: core.Extent{
				Off: pieces[i].Extent.Off,
				Len: pieces[j-1].Extent.Off + pieces[j-1].Extent.Len - pieces[i].Extent.Off,
			},
			skip: pieces[i].Skip,
		})
		i = j
	}
	return out
}
