package recovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
)

// asideRecord is the header of one record the rebuild does not feed — a
// loss, hold, unhold, stamp or forget record — kept for the phases that
// apply those kinds' own rules and read their bodies there. The header
// verified; its body was not read here.
type asideRecord struct {
	Header format.RecordHeader
}

// segmentScan is what one own segment's walk found: the index records its
// content kinds map to, the aside kinds' headers, the tail facts the later
// phases judge by — whether the walk resynchronised past a bad range, the
// highest synced-through offset any verifying record names, the seal
// marker — and where it ended. Size is the segment file's own size.
type segmentScan struct {
	ID      core.SegmentID
	Records []index.Record
	Aside   []asideRecord

	// Resynced is the walk's resync count contribution: a RangeBad range
	// directly followed by an attached record counts the scan once, however
	// many ranges it crossed.
	Resynced bool

	// SyncedThrough is the highest SyncedThrough any verifying record named.
	SyncedThrough int64

	Sealed bool
	// SealedByCatalog is set only when the scan was built from the
	// segment's verified footer rather than the header walk: the later
	// phases read it to know which source a segment's facts came from, and
	// the rebuild counts it.
	SealedByCatalog bool
	// End is where the walk stopped: the seal marker's offset, the failing
	// slot's offset, or the file's size.
	End int64
	// RecordsEnd is where the last verifying record ends:
	// format.SegmentHeaderSize when the segment holds none, End for a sealed
	// or catalogued scan, whose last record ends at the seal marker.
	RecordsEnd int64
	// Ranges holds the byte ranges the walk attached nothing from, as the
	// walk reported them, ascending; a catalogued scan has none.
	Ranges []format.ScanRange
	// Size is the segment file's own size, as its storage reported.
	Size int64

	// VerifiedEnd is where the last record whose header verified ends, a clone
	// target whose body failed excluded: format.SegmentHeaderSize when none
	// did. The walk never reads a content payload, so a record whose payload
	// is torn still counts here; it is not proof of a payload. It is the line
	// the classing draws between a torn drop at the tail and one a later
	// record follows.
	VerifiedEnd int64

	// Dropped holds the entries the tail's classing will judge: the catalogue
	// read fills it for each record it confines, with Synced set, and the walk
	// fills it for each clone target whose body did not verify, with Entry
	// zero and Synced unset. What it carries is the tail's to own.
	Dropped []droppedRecord
}

// readerAt adapts the open's reader to io.ReaderAt for one file name, so
// every byte the scan walks goes through the retry bound the open's other
// reads take. A read the bound did not clear comes back as itself: the
// range the reader collected answers the open, never a verdict these bytes
// never earned.
type readerAt struct {
	rd   *reader
	name string
}

// ReadAt reads len(p) bytes at off through the bound, as io.ReaderAt.
func (a readerAt) ReadAt(p []byte, off int64) (int, error) {
	return a.rd.ReadAt(a.name, p, off)
}

// scanSegment walks one attached own segment by its record headers alone
// and turns each verifying record into what the rebuild consumes: content
// kinds into index records, loss, hold, unhold, stamp and forget headers
// into the aside list, and the tail facts — resync, synced-through, seal —
// into the scan. identity is the journal the segment headers must name;
// info is the segment as the attach pass classed it.
//
// Every read the walk makes goes through the open's bound, so an error the
// bound did not clear returns as itself and the open refuses on the range
// the reader collected; a cancellation the open itself caused returns as
// the context's error, collected nowhere. The walk's own stops — a header
// of an unknown kind, a record whose version carries a non-zero epoch —
// fail the scan naming the segment, as does a record whose version counter
// or sequence number is the largest 64-bit value, which this journal never
// issues: each comes from damage or another writer, and none may be
// stepped over or served.
//
// A segment too short to hold the first record slot scans empty: its bytes
// are the tail phases' to judge, and nothing here fails the open.
//
// decision: a reconcile or reconciled record fails the scan with
// ErrNotImplemented rather than being fed, skipped or set aside: the
// metadata reconcile that record names is not built, and an open that
// dropped it would serve without knowing a reconcile was owed. The
// ceiling is an open that cannot serve such a segment at all — no
// reconcile record may be read past until the reconcile work lands, which
// is what withdraws this marker.
func scanSegment(rd *reader, identity core.JournalID, info SegmentInfo) (segmentScan, error) {
	// The size goes through the same bound every byte read takes: an open
	// or a Size the device does not answer is retried on the bound's own
	// gates, and one no bounded attempt completes is collected as the
	// segment's range rather than answered with a scan over bytes nobody
	// measured.
	size, err := segSize(rd, info.ID)
	if err != nil {
		return segmentScan{ID: info.ID}, err
	}

	sc := segmentScan{ID: info.ID, Size: size, End: size, RecordsEnd: format.SegmentHeaderSize,
		VerifiedEnd: format.SegmentHeaderSize}
	if size < format.SegmentHeaderSize {
		// Only an unverified-header segment can be this short: no record
		// slot exists, so the scan is empty and the tail phases judge it.
		return sc, nil
	}
	walk, werr := format.ScanSegmentHeaders(readerAt{rd: rd, name: format.SegmentName(info.ID)},
		size, identity, info.ID)
	sc.End = walk.End
	sc.Sealed = walk.Sealed
	sc.Resynced = walk.Resynced
	sc.Ranges = walk.Ranges
	if walk.Sealed {
		sc.RecordsEnd = walk.End
	} else if n := len(walk.Records); n > 0 {
		last := walk.Records[n-1].Header
		sc.RecordsEnd = last.Offset + last.Size()
	}

	for _, rec := range walk.Records {
		h := rec.Header
		// Every kind meets the largest-value rule, content or aside: the
		// numbers a record carries are refused whatever its kind's own
		// rules do with them.
		if err := checkNumbers(h); err != nil {
			return sc, err
		}
		if h.SyncedThrough > sc.SyncedThrough {
			sc.SyncedThrough = h.SyncedThrough
		}
		switch {
		case h.Kind <= format.KindUnmark: // the content kinds, 1–9
			r, cerr := contentRecord(rd, identity, h)
			if errors.Is(cerr, core.ErrCorrupt) {
				// Only a clone target's body can fail this way: the
				// record's header names it, so the tail's classing
				// decides it, torn or corrupt.
				sc.Dropped = append(sc.Dropped, droppedRecord{Header: h, Err: cerr})
				continue
			}
			if cerr != nil {
				return sc, cerr
			}
			sc.Records = append(sc.Records, r)
		case h.Kind <= format.KindForget: // the aside kinds, 10–14
			sc.Aside = append(sc.Aside, asideRecord{Header: h})
		default: // the reconcile kinds, 15 and 16
			return sc, fmt.Errorf("%w: a %s record in segment %d", core.ErrNotImplemented, h.Kind, info.ID)
		}
		sc.VerifiedEnd = h.Offset + h.Size()
	}
	if werr != nil {
		// The walk's stop: an unknown kind or a non-zero epoch — the
		// format's own errors, wrapped here with the segment the open must
		// refuse. A read error the bound did not clear never takes this
		// branch's verdicts: it is returned as itself below, and the open
		// refuses on the collected range.
		if errors.Is(werr, format.ErrUnknownKind) || errors.Is(werr, format.ErrNonZeroEpoch) {
			return sc, fmt.Errorf("%w: segment %d: %v", core.ErrUnknownFormat, info.ID, werr)
		}
		return sc, werr
	}
	return sc, nil
}

// largestCounter is the largest 64-bit value, the number this journal never
// issues: its version limit is exclusive above one below the top, so a
// record carrying it — as a version counter or a sequence number — comes
// from damage or another writer and fails the open.
const largestCounter = math.MaxUint64

// checkNumbers refuses a record whose version counter or sequence number is
// the largest 64-bit value, or whose sequence number is zero — a number the
// journal's writer never issues, whatever the record's kind — naming the
// segment and the record's offset. A non-zero epoch needs no check here: the
// walk's header decode stops on it.
func checkNumbers(h format.RecordHeader) error {
	if h.Version.Counter == largestCounter || h.Seq == 0 || h.Seq == core.Seq(largestCounter) {
		return fmt.Errorf("%w: segment %d: the record at %d carries version %v, seq %d",
			core.ErrUnknownFormat, h.Segment, h.Offset, h.Version, h.Seq)
	}
	return nil
}

// contentRecord converts one content-kind record header into the index
// record the rebuild feeds, reading a clone target's 56-byte spec at the
// record's own offset through the bound — the one body the walk left
// unread that the index record needs, verified whole before it is used.
func contentRecord(rd *reader, identity core.JournalID, h format.RecordHeader) (index.Record, error) {

	r := index.Record{
		File:    h.File,
		Tag:     h.Tag,
		Numbers: index.Numbers{Version: h.Version, Seq: h.Seq},
	}
	switch h.Kind {
	case format.KindWrite:
		r.Kind = index.RecWrite
	case format.KindFill:
		r.Kind = index.RecFill
	case format.KindRelease:
		r.Kind = index.RecRelease
	case format.KindTruncate:
		// Off is the new size and Len is 0: the removal runs to the file's
		// end.
		r.Kind = index.RecTruncate
		r.Off, r.Len = h.Extent.Off, 0
	case format.KindDeallocate:
		r.Kind = index.RecDeallocate
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
	case format.KindDelete:
		// Both 0: the removal runs to the file's end.
		r.Kind = index.RecDelete
	case format.KindCloneTarget:
		// The extent is the spec's DstOff and Len; the spec itself is the
		// body the walk did not read.
		r.Kind = index.RecCloneTarget
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
		spec, err := cloneSpec(rd, identity, h)
		if err != nil {
			return index.Record{}, err
		}
		r.Clone = spec
	case format.KindOffloaded:
		r.Kind = index.RecOffloaded
	case format.KindUnmark:
		r.Kind = index.RecUnmark
	}
	if r.Kind != index.RecTruncate && r.Kind != index.RecDelete && r.Kind != index.RecCloneTarget {
		r.Off, r.Len = h.Extent.Off, h.Extent.Len
	}
	r.Segment = h.Segment
	r.At = uint32(h.Offset)
	return r, nil
}

// cloneSpec reads a clone target record's 56-byte body at the record's own
// offset through the bound and verifies it whole, so no part of a record
// that does not verify is used. A body that does not verify returns an error
// wrapping core.ErrCorrupt naming the segment and the record's offset.
//
// A clone target whose body does not verify is confined to that record:
// the caller records it as dropped and the tail's classing decides whether
// it is a crash's tear or damage.
func cloneSpec(rd *reader, identity core.JournalID, h format.RecordHeader) (*core.CloneSpec, error) {
	body := make([]byte, h.BodySize())
	n, rerr := rd.ReadAt(format.SegmentName(h.Segment), body, h.Offset+format.RecordHeaderSize)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A read the bound did not clear returns as itself — a
		// cancellation or a device fault, either answered by the caller:
		// the open refuses on the collected range for the fault, or with
		// the context's own error for the cancellation.
		return nil, rerr
	}
	if int64(n) < h.BodySize() {
		return nil, fmt.Errorf("%w: segment %d: the clone target at %d holds %d of its %d body bytes",
			core.ErrCorrupt, h.Segment, h.Offset, n, h.BodySize())
	}
	verified, verr := format.VerifyRecordBody(body, identity, h)
	if errors.Is(verr, format.ErrNonZeroEpoch) {
		// A version of a later epoch is a format this binary does not read,
		// as a header carrying one is, not damage.
		return nil, fmt.Errorf("%w: segment %d: the clone target at %d: %v",
			core.ErrUnknownFormat, h.Segment, h.Offset, verr)
	}
	if verr != nil {
		return nil, fmt.Errorf("%w: segment %d: the clone target at %d: %v",
			core.ErrCorrupt, h.Segment, h.Offset, verr)
	}
	return &core.CloneSpec{
		SrcShare: verified.Clone.Share,
		Src:      verified.Clone.File,
		SrcOff:   verified.Clone.Off,
		DstOff:   h.Extent.Off,
		Len:      h.Extent.Len,
		AsOf:     verified.Clone.AsOf,
	}, nil
}

// segSize returns the segment file's size, its open and its Size taken
// through the same bound every byte read of the open takes: the bound's
// gates decide whether the storage is asked again, and an open or a Size
// the device does not answer comes back as the error the open refuses on.
// A size names no byte range of its own — what the bound did not learn is
// the file's whole shape — so the refusal collects the file whole: every
// byte of a segment whose length no bounded attempt answered, the smallest
// honest name for "a size that could not be read".
func segSize(rd *reader, id core.SegmentID) (int64, error) {
	name := format.SegmentName(id)
	var size int64
	_, err := readretry.Do(rd.ctx, rd.clk, readretry.Attempts, func() error {
		f, ferr := rd.fs.Open(name)
		if ferr != nil {
			return ferr
		}
		// The close's own error is dropped: the size the attempt existed
		// for has already been asked for, and a close that failed releases
		// nothing the caller still needs.
		var serr error
		size, serr = f.Size()
		_ = f.Close()
		return serr
	})
	switch {
	case err == nil:
		return size, nil
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return 0, err
	}
	rd.warnUnreadable(name, err)
	rd.unread = append(rd.unread, core.SegmentRange{Segment: id, Off: 0, Len: math.MaxInt64})
	return 0, err
}

// rebuilt is the rebuild phase's answer: the index the records on disk
// rebuilt into with the counters they raised, the scan of every own
// segment — aside kinds carried for the phases that apply their rules, the
// tail facts for the phases that judge them — and the number of scans that
// resynchronised past a bad range.
type rebuilt struct {
	Index       *index.Index
	Counters    *index.Counters
	Scans       []segmentScan
	ScanResyncs int

	// ByCatalog counts the scans a segment's verified footer built: one per
	// own segment a verified successor named and whose trailer held a
	// verifying catalogue.
	ByCatalog int
}

// rebuildIndex rebuilds the index over every own segment the attach pass
// found, floor the version floor the journal was opened with — Finish takes
// it, refusing one with a non-zero epoch the same error a record of that
// shape takes. The workers only scan: the records they convert stay buffered
// on their scans, and once the last one has returned the rest of the phase
// runs serially in one fixed order — the aside kinds' bodies are decoded and
// decided against their own rules (losses reaching the sink given), the
// tails are classed, then the buffered records are fed into a fresh index,
// segments in ascending-id order and each scan's records in their walk
// order, and Finish closes the builder. The kinds pass's collected answer
// and the classing's come back with the rebuilt state, for the State fields
// the phase owns: the maps the kinds pass builds are the feed's to filter
// by, and no aside is ever fed — the aside kinds are carried for the phases
// that own their rules, the decode's confinements landing on the same scans
// the classing reads.
//
// Each segment is read from one of two sources: the catalogue its trailer
// names, offered to a segment only when another own segment's verified
// header names it as predecessor, or the header walk, which every other
// segment takes. A named segment whose trailer holds no verifying footer is
// demoted to the walk, and the two sources must give the same answer — the
// catalogue is an accelerator, not a second opinion. forceScan turns the
// catalogue off, the test-only switch that makes every segment take the
// walk so a check can compare the sources' answers.
//
// order reorders the segments before the dispatch — nil keeps the listing
// order — and is the one seam the order check drives: the workers' answer
// is scanned in whatever order it lands, but the feed is the one order
// above, so the index must come out identical whatever order the segments
// are walked in. extentLimit bounds the index the way the journal's
// settings do.
//
// Every segment is scanned by its own reader, at most readers at a time
// (the open passes GOMAXPROCS; a value below one runs one):
// the work per segment is one file's walk, and the bound costs nothing a
// file read costs. The first error stops the dispatch — the workers that
// are running answer their current read at the context's error and their
// segments are no longer started — and the whole phase returns it, the
// index discarded unbuilt, so a refused open returns no half-built state.
// A read no bounded attempt completed is collected by its worker's reader
// and merged into rd here, so the open refuses on every range the phase
// could not read, as the attach pass's own are. losses is the loss sink
// the kinds pass delivers to, nil when the open runs without one; att is
// the attach pass's answer for the segments in own, which the classing
// reads for what a segment's fate depends on.
//
// Nothing is written: the phase reads, feeds and finishes; the phases after
// it own every change the open makes.
//
// ponytail: one goroutine per segment, so a lone huge segment is walked by
// one reader while the others idle behind the bound. Split a scan at
// catalogued offsets — walking the two halves of one segment on two
// readers, merging the scans — when a start-up profile of a journal with
// huge segments shows the rebuild bound by one.
func rebuildIndex(ctx context.Context, rd *reader, identity core.JournalID, own []SegmentInfo, floor core.Version, extentLimit int64, readers int, order func([]SegmentInfo), onScanStart func(core.SegmentID), forceScan bool, losses LossSink, att Attached) (rebuilt, kindResults, tails, error) {
	ix := index.New(extentLimit)
	ctr := &index.Counters{}
	b := ix.Rebuilder(ctr)

	if order != nil && len(own) > 0 {
		// The seam works on a copy, so the listing the caller holds is not
		// reordered under it: the listing is State's, and a check that
		// reshuffles it would change what later phases see.
		shuffled := slices.Clone(own)
		order(shuffled)
		own = shuffled
	}

	// named is the successor set: every own segment another own segment
	// names as its predecessor. The catalogue a named segment's trailer may
	// hold is the only thing this buys — a segment nobody names takes the
	// walk, its trailer never read.
	//
	// decision: only a verified own header's claim counts. The attach pass
	// read and verified every own header before this dispatch, so the
	// predecessor column read here was itself covered by a checksum; a
	// successor the attach pass rejected — a foreign or damaged one —
	// claims nothing, for its predecessor column is not trusted. The
	// newest segment of a stream, which no own header names, is therefore
	// scanned whatever its trailer says, so a recycled segment's old
	// trailer is never read as its catalogue; the trailer's own defence
	// stays beside it — the footer's seal marker is seeded with the
	// journal's identity and the segment's own id, so no other segment's
	// footer verifies as a named segment's.
	named := make(map[core.SegmentID]bool, len(own))
	for _, seg := range own {
		// A segment naming itself as its own predecessor is damaged, and
		// nothing is offered a catalogue off its own claim.
		if seg.Header.Predecessor != 0 && seg.Header.Predecessor != seg.ID {
			named[seg.Header.Predecessor] = true
		}
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		slot = make(chan struct{}, max(readers, 1))
		// Each worker writes only its own index, and every read of these
		// follows wg.Wait, so no lock guards them.
		scans   = make([]segmentScan, len(own))
		workers = make([]*reader, len(own))
		errs    = make([]error, len(own))
	)
	for i := range own {
		slot <- struct{}{} // a reader slot, held until the segment is done
		if wctx.Err() != nil {
			// An error or a cancellation stops the dispatch, checked once the
			// slot is held — the slot a failure frees is the one a waiting
			// dispatch takes: the workers running answer at their next read,
			// and none after this is started.
			<-slot
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slot }()

			if onScanStart != nil {
				onScanStart(own[i].ID)
			}
			srd := newReader(wctx, rd.fs, rd.clk, rd.log)
			workers[i] = srd
			switch {
			case forceScan || own[i].Unverified || !named[own[i].ID]:
				scans[i], errs[i] = scanSegment(srd, identity, own[i])
			default:
				got, byCatalog, cerr := catalogSegment(srd, identity, own[i])
				switch {
				case cerr != nil:
					scans[i], errs[i] = got, cerr
				case byCatalog:
					scans[i] = got
				default:
					// The demotion: a named segment whose trailer holds no
					// verifying footer — a segment sealed at a crash holds
					// a marker but no trailer — falls back to the walk,
					// whose answer the catalogued one must match.
					scans[i], errs[i] = scanSegment(srd, identity, own[i])
				}
			}
			if errs[i] != nil {
				cancel() // the running workers answer their next read at the context's error
			}
		}()
	}
	wg.Wait()

	// Every worker's unreadable ranges merge into rd, in the dispatch's
	// order, so the open's end check refuses on the whole phase's, the
	// attach pass's among them.
	for _, srd := range workers {
		if srd == nil {
			// A segment the dispatch never started: its reader read
			// nothing, so it has nothing to merge.
			continue
		}
		rd.unread = append(rd.unread, srd.Unreadable()...)
	}

	// A segment's refusal or the context's error answers the phase: nothing
	// is fed and no index is returned, every worker having returned by the
	// wait above. The worker errors reach Run raw — wrapped only where
	// scanSegment named the segment — so errors.Is answers the caller's own
	// sentinel. A refusal that is not a read's own is answered as the
	// unreadable ranges when any range was collected, the attach pass's
	// included, because the ranges are the unknown the operator must clear
	// first; a cancellation is always answered as itself.
	if first := firstFailure(ctx, own, errs); first != nil {
		if len(rd.unread) > 0 && !isContextErr(first) {
			return rebuilt{}, kindResults{}, tails{}, &core.UnreadableRangesError{Ranges: slices.Clone(rd.unread)}
		}
		return rebuilt{}, kindResults{}, tails{}, first
	}

	// The kinds pass reads the bodies the workers set aside — every loss,
	// hold, unhold, stamp and forget record the scans carry — and decides
	// them against their own rules, before the tail's classing judges the
	// corrupt drops the decode confines onto the same scans.
	ks, kerr := decodeAsides(rd, identity, losses, scans)
	if kerr != nil {
		return rebuilt{}, kindResults{}, tails{}, kerr
	}
	// The classing reads the scans only after the kinds pass has confined
	// every unread aside body onto them, so a lost loss record reaches the
	// classing as the corrupt drop it is.
	tl, terr := classifyTails(rd, identity, att, scans)
	if terr != nil {
		return rebuilt{}, kindResults{}, tails{}, terr
	}

	// The workers did not feed — the scans hold every converted record —
	// because the feed must wait for the kinds pass and the classing over
	// all of them, so this is where the buffered records go.
	//
	// ponytail: records of every segment stay in memory until the kinds pass
	// has decided what the loss and forget records withhold, so the rebuild
	// peaks at about twice the builder's own buffer, which index/rebuild.go's
	// note already prices; upgrade to a two-pass header walk (asides first,
	// then a filtered scan) when J6's time-to-first-read profile shows the
	// peak or the open span mattering.
	//
	// decision: the feed is serial because the withhold set is global — a loss
	// record anywhere names a record anywhere — overturnable by a rebuilder
	// that can un-feed a sequence number. This amends the builder's own
	// concurrent-feeder note (index/rebuild.go): the builder still takes
	// concurrent feeders, and what it builds is order-independent, but this
	// caller cannot feed a segment's records before every aside has been
	// decided, whatever order the dispatch happened to scan in.
	byID := make([]int, len(scans))
	for i := range byID {
		byID[i] = i
	}
	slices.SortFunc(byID, func(a, c int) int { return cmp.Compare(scans[a].ID, scans[c].ID) })
	filter := ks.newFeedFilter()
	for _, i := range byID {
		sc := &scans[i]
		for _, rec := range sc.Records {
			if ferr := filter.feed(rec, b.Feed); ferr != nil {
				// The refused record's error answers as the phase's other
				// refusals do: as the unreadable ranges when any range was
				// collected, otherwise as itself, the index discarded unbuilt.
				if len(rd.unread) > 0 && !isContextErr(ferr) {
					return rebuilt{}, kindResults{}, tails{}, &core.UnreadableRangesError{Ranges: slices.Clone(rd.unread)}
				}
				return rebuilt{}, kindResults{}, tails{}, ferr
			}
		}
		// The index buffer holds what the record said; the scan keeps only
		// what the later phases read, so a journal of millions of records
		// does not hold every converted record twice.
		sc.Records = nil
	}

	// Finish runs with every scan's records fed. The floor's epoch half is
	// refused before the claim takes it, so a refused floor builds nothing
	// and changes nothing, and the counters' error answers as the record's
	// own epoch refusal does.
	if err := b.Finish(floor); err != nil {
		return rebuilt{}, kindResults{}, tails{}, err
	}
	out := rebuilt{Index: ix, Counters: ctr, Scans: scans}
	resyncs := 0
	byCatalog := 0
	for _, sc := range out.Scans {
		if sc.Resynced {
			resyncs++
		}
		if sc.SealedByCatalog {
			byCatalog++
		}
	}
	out.ScanResyncs = resyncs
	out.ByCatalog = byCatalog
	return out, ks.kindResults, tl, nil
}

// isContextErr reports a cancellation or a deadline, the errors a worker
// answers with when the dispatch was stopped under it.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// firstFailure picks the error that answers a phase in which segments may
// have failed together: a real failure outranks a cancellation — the
// cancellation a worker saw is usually the fallout of another's failure —
// and among failures of one class the lowest segment id wins, whatever
// order the segments were dispatched in or finished in. With no worker
// failure, a cancelled ctx answers as itself, for a dispatch it stopped
// before any worker saw it.
func firstFailure(ctx context.Context, own []SegmentInfo, errs []error) error {
	var real, cancelled error
	realID, cancelledID := core.SegmentID(0), core.SegmentID(0)
	for i, err := range errs {
		switch {
		case err == nil:
		case isContextErr(err):
			if cancelled == nil || own[i].ID < cancelledID {
				cancelled, cancelledID = err, own[i].ID
			}
		default:
			if real == nil || own[i].ID < realID {
				real, realID = err, own[i].ID
			}
		}
	}
	switch {
	case real != nil:
		return real
	case cancelled != nil:
		return cancelled
	}
	return ctx.Err()
}
