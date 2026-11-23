package durability

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Loss is one extent the journal stopped holding without being asked.
//
// Extent is the piece lost, not necessarily the whole dropped record: a
// reconcile can drop one piece of a record and keep the rest under the same
// Seq. A caller hands Lose each piece it drops, and each piece is reported
// on its own and appends a loss record of its own over its own extent: that
// extent is what outranks the older records below it after a crash.
type Loss struct {
	Share   core.ShareTag
	File    core.FileID
	Extent  core.Extent
	Version core.Version

	// Seq is the dropped record's sequence number.
	Seq core.Seq

	Reason core.LossReason

	// Offloaded is the extent's offloaded bit as it was.
	Offloaded bool

	// Synced is whether the dropped record was synced when dropped.
	Synced bool

	// AtOpen marks an open's own loss.
	AtOpen bool

	// Raised says the generation was already raised for this loss — by the
	// caller, as the read path does before it hands the loss on, or by the
	// first take of the piece a takeover now republishes. Lose then counts
	// and publishes it but raises nothing.
	Raised bool

	// Marked says the caller already left the marker of a synced loss, so
	// Lose leaves none.
	Marked bool

	// Reported says the failure this loss names was already published to the
	// ring by the caller's own report — a record a read-only journal failed
	// reported it when its window failed, so the open that appends the
	// record the failed append owed publishes no new event for it. Lose
	// appends the record and keeps the causality registration its placement
	// files, but the ring holds nothing and the zero event answers. The
	// credit holds only while the report's own answer stands: a take that
	// overtakes a dead, unread claim clears it and publishes the loss here,
	// the Raise set the same take, for the piece's first take counted that
	// generation already. A reported loss
	// also takes back a piece a placed but unsynced record of an earlier
	// Lose holds, to append the record that one never synced.
	Reported bool

	// Deferred appends no record: the copy that retires the dropped
	// record's segment writes it.
	Deferred bool

	// Seg is the segment holding the dropped record; required when
	// Deferred, ignored otherwise.
	Seg core.SegmentID
}

// Lose records a loss: it appends and syncs the loss record, leaves the
// index's marker for a synced loss, raises the generation by the reason's
// rule, counts the loss, and publishes its event, in that order. The event
// is published last, so a reader that sees it is already owed the record.
// A piece (Seq and Extent) already taken by an earlier Lose or Place does
// nothing and returns the zero event with a nil error. That says only that
// the piece was taken by an earlier call: it says nothing about that call's
// record being on disk, which may still be in flight, and only the call that
// took the piece reports whether its record was appended and synced. Every
// other piece, of a Seq already reported or not, goes through all of it, so
// no event precedes its own record.
//
// StreamIndex returns the number of the stream a loss record of file appends
// on, the stream Lose draws through streams.For, so a caller counting the
// rolls a set of losses needs counts the streams the records land on. It
// answers -1 before Start, when the layer has no streams.
func (d *Durability) StreamIndex(file core.FileID) int {
	streams := d.streams.Load()
	if streams == nil {
		return -1
	}
	return streams.IndexFor(file)
}

// SetLossAdmission wires the record-count admission every loss record is
// drawn through, the headroom of the journal that owns this layer. It is not
// a Config field because the headroom's own config takes this layer's Admit,
// so the core builds each with the other built and wires the pair only then;
// it runs before Start, and a Lose before Start is refused before its piece
// is taken, so no loss record ever appends unwired. admit reserves one loss
// record's headroom entry — atOpen tells the class, an open's own loss
// drawing the open reserve where a loss found while the journal runs is
// refused — and the settle it returns is called once with placed, saying
// whether the record landed, to draw the entry or refund it by. A settle of
// nil comes only with an error. Nil, or never called, admits every loss: a
// layer whose journal budgets no record headroom.
func (d *Durability) SetLossAdmission(admit func(atOpen bool) (settle func(placed bool), err error)) {
	d.lossAdmit = admit
}

// Lose records a loss: it appends and syncs the loss record, leaves the
// index's marker for a synced loss, raises the generation by the reason's
// rule, counts the loss, and publishes its event, in that order. The record
// draws on the headroom through the wired admission, by the class of caller
// the loss serves: an open's own loss (AtOpen) may draw the open reserve, a
// loss found while the journal runs may not take the space the next open
// needs. A refusal appends nothing and hands the piece back, so the loss can
// be raised again. The event
// is published last, so a reader that sees it is already owed the record.
// A piece (Seq and Extent) already taken by an earlier Lose or Place does
// nothing and returns the zero event with a nil error. That says only that
// the piece was taken by an earlier call: it says nothing about that call's
// record being on disk, which may still be in flight, and only the call that
// took the piece reports whether its record was appended and synced. Every
// other piece, of a Seq already reported or not, goes through all of it, so
// no event precedes its own record.
func (d *Durability) Lose(ctx context.Context, l Loss) (core.LossEvent, error) {
	return d.loseMoved(ctx, l, nil)
}

// loseMoved is Lose with moved: the segments whose held bytes the caller's
// move already took out of the index without a marker of this layer's own —
// a failed window's cut of its lost records, which frees the segments before
// the loss record that names the loss exists. Once this call has placed that
// record, the move is reported through Config.Emptied gated on its place; a
// caller whose move lowered no segment of its own, or whose marker does the
// accounting (a synced loss), passes nil.
func (d *Durability) loseMoved(ctx context.Context, l Loss, moved []core.SegmentID) (core.LossEvent, error) {
	if err := ctx.Err(); err != nil {
		return core.LossEvent{}, err
	}

	// A Lose before Start has no stream to append through. It is refused
	// before the piece is taken, so the loss can be raised again once started.
	if d.streams.Load() == nil {
		return core.LossEvent{}, fmt.Errorf("durability: the layer is not started: %w", core.ErrClosed)
	}

	// A reason that names none is a caller bug refused here, before the seq
	// is named, so every path refuses it alike.
	format.LossReasonOf(l.Reason)

	// A deferred loss names the segment holding its dropped record, the
	// segment whose copy writes the record this one defers; a zero Seg is a
	// caller bug refused here too, before the piece is taken, so the loss
	// can be raised again with one, like the not-started refusal.
	if l.Deferred && l.Seg == 0 {
		return core.LossEvent{}, errors.New("durability: a deferred loss names no segment holding its dropped record")
	}

	// The piece is taken in the directory's state, shared with every other
	// Durability on it: one already named by a Place, or by the Lose of this
	// instance, of one still live, or of one whose loss's event a reader
	// took away, is reported by whichever call took it, never again. The
	// Lose of an instance stopped before any reader reached its loss's
	// event retired with it, and this take takes the piece over, so the
	// loss is reported here as this instance's.
	//
	// A reported loss takes back a piece a placed but unsynced record holds:
	// the take stopped its second event, not the record it is still owed.
	k := piece{l.Seq, l.Extent}
	// retaken says the piece stays held whatever this call's append does:
	// the unsynced record whose debt this append pays held it, and a
	// failure of the append hands that debt back, never releasing the
	// piece as though nothing named the loss.
	retaken := false
	if took, takeover, debt := d.dir.take(k, d); !took {
		if !l.Reported || !d.dir.retakeUnsynced(k) {
			return core.LossEvent{}, nil
		}
		retaken = true
	} else {
		retaken = debt
		if takeover {
			// The claim this take overtook belonged to an instance that
			// stopped before any reader reached the event its loss
			// published: the report Reported credits answered nothing, so
			// this call publishes the loss itself. Raised is set whether
			// or not the caller brought Reported, for the piece's own
			// first take already raised the generation when the retired
			// claim's loss took it — the reason and the offloaded bit the
			// rule raises by are the same drop's — and a second raise
			// would count again a loss no reader was told of.
			l.Reported = false
			l.Raised = true
		}
	}

	var at segment.Location
	var recErr error
	refused := false
	if !l.Deferred {
		if recErr = d.refuse(); recErr != nil {
			refused = true
		} else {
			// The loss record's admission, by the class of caller the loss
			// serves: a refusal returns before the append, like the
			// read-only one, and the piece is handed back below, so the
			// loss can be raised again.
			var settle func(placed bool)
			if d.lossAdmit != nil {
				if settle, recErr = d.lossAdmit(l.AtOpen); recErr != nil {
					refused = true
				}
			}
			if !refused {
				at, recErr = d.appendRecord(l)
				if settle != nil {
					settle(at != (segment.Location{}))
				}
			}
		}
		if d.afterAppend != nil {
			d.afterAppend()
		}
	} else {
		// The registry entry the copy that retires l.Seg builds its loss
		// record from, stored once the piece is taken: the deferral ends on
		// EndDeferral alone.
		d.dir.deferAgainst(l.Seg, l)
	}

	firstErr := recErr
	// A window's own cut of its lost records named no causality record of
	// theirs: the loss record this call has just placed is the one a
	// retirement of their segment waits on. reportEmptied drops the report
	// when nothing was appended, so a refusal names nothing. A record placed
	// but unsyncable — its sync failing, its segment ending — is still
	// reported, deliberately: the retirement waiting on that record then
	// waits on a durability that never comes and keeps the segment, on the
	// safe side; nothing is lost.
	d.reportEmptied(moved, at)
	// decision: a superseded loss leaves no marker even when synced. It
	// reports no write lost, so Since does not yield it and it needs no
	// Settle; a marker would make a later DropOlder inherit one that nothing
	// settles. Withdraw this if a superseded loss ever has to be settled.
	if l.Synced && !l.Marked && l.Reason != core.LossSuperseded {
		delta, touched, err := d.markLoss(l.File, l.Extent, l.Version)
		if err != nil {
			d.log.Error("a synced loss left no marker", "file", l.File, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			if d.account != nil {
				d.account(delta)
			}
			// The marker's cut and the record the marker rode on name one
			// crossing, so the report is gated on the record's place.
			d.reportEmptied(touched, at)
		}
	}

	// decision: the generation is raised by the reason and the offloaded bit
	// alone, never by what the caller asks, bar a raise it already made and
	// says so with Raised: a corrupt or acknowledged extent
	// whose bytes are not offloaded is data a client may have been told was
	// stored and is now gone, and anything else can be fetched again. A
	// failed window raises it once, for the window, through RaiseWindow.
	//
	// The raise, the counters and the event are one critical section, so a
	// Stats call never sees the raised generation without the event that
	// raised it. The raise stays before the publish: a reader that sees the
	// event sees its raise.
	d.lmu.Lock()
	if (l.Reason == core.LossCorrupt || l.Reason == core.LossAcknowledged) && !l.Offloaded && !l.Raised {
		d.dir.raise()
	}
	switch {
	case l.Reason == core.LossCorrupt && !l.Offloaded:
		d.corruptDataLoss++
	case l.Reason == core.LossCorrupt:
		d.corruptRepairable++
	case l.Reason == core.LossStale:
		d.stale++
	case l.Reason == core.LossSuperseded:
		d.superseded++
	}
	ev := core.LossEvent{}
	if l.Reported {
		// The failure was already published when the caller first told it:
		// the open's own leg for a record a read-only journal failed, whose
		// window reported it then and whose record this call now appends.
		// The counters and the raise above are the one rule's still, and
		// the causality registration the append filed answers a retirement
		// of the failed segment's own; no event exists to answer with.
	} else {
		ev = d.pushRingLocked(core.LossEvent{
			Share:     l.Share,
			File:      l.File,
			Extent:    l.Extent,
			Version:   l.Version,
			Reason:    l.Reason,
			Offloaded: l.Offloaded,
			Synced:    l.Synced,
			AtOpen:    l.AtOpen,
			Deferred:  l.Deferred,
		})
	}
	d.lmu.Unlock()
	if !l.Reported {
		// The claim this call took or kept records the event its loss
		// published — the number the push assigned it — so the reader's
		// reach retires the claim on its own loss alone. A reported loss
		// publishes no event of its own, and its claim keeps the zero.
		d.dir.setClaimEvent(k, d, ev.SeqNum)
	}

	// decision: a drop whose record could not be appended is never undone.
	// The journal stops holding what it cannot produce, and a loss only errs
	// toward fetching the extent again, so the caller's drop stands, the
	// event is published — unless the caller already reported the failure
	// and the take took no dead claim over, in which case the suppression
	// above holds no event for the ring and the zero event answers — and
	// the sequence number joins the directory's failed list for the next
	// open to name. A deferred loss is not on that
	// list: its record is owed to the copy that retires the segment.
	//
	// decision: a superseded loss whose record is refused stays off the
	// failed list. That list drops the whole dropped record at the next open
	// as one with an unsynced loss record, but a superseded piece is only
	// part of it and what DropOlder kept can still be current content; the
	// unrecorded piece is the reconcile's to write. Withdraw this if a
	// superseded loss ever covers a whole dropped record.
	if recErr != nil {
		if l.Reason != core.LossSuperseded {
			// A failure the read-only refusal turned back — like a
			// reported loss re-appending a record its window failed —
			// reached its writer by that refusal rather than by any
			// ring, so the entry names no reporter: the report stands
			// however every ring ended, none was its medium. Any other
			// failure published its own event on the way, and its word
			// is the failing instance and that event: honoured while
			// either lives, re-published once neither does.
			if refused || l.Reported {
				d.dir.addFailed(l.Seq, nil, 0)
			} else {
				d.dir.addFailed(l.Seq, d, ev.SeqNum)
			}
			// decision: the piece a failure took is handed back with it
			// when the record never placed and the instance still lives,
			// for the failed list the same failure appended carries the
			// word alongside the number — the reporter itself, and the
			// event its failing Lose published — so the planner that owes
			// the record honours the report only while the reporter lives
			// or a reader reached that event, and a loss a closed unread
			// ring never answered is re-published at the next open. A
			// stopped instance keeps its claim instead, marked unplaced:
			// it owns no record, so it names nothing and bars no other
			// taker, and the open that takes it over learns — the takeover
			// is the word — how far a reader reached the report the dead
			// ring held, so the record is appended again either way, and
			// the loss republished when nothing at all answered for it.
			// Overturned by a caller that appends the record itself
			// instead of retrying the loss, which would make the taken
			// piece the dedup that stops a second attempt. A kept piece is
			// marked unsynced, so the open's pass can tell it from one a
			// durable record holds and append the record it is still owed.
			// A piece retaken from an earlier unsynced record stays held
			// too: that record is still on disk, so a failed retry frees
			// nothing.
			if at == (segment.Location{}) && !retaken {
				if !d.claimsDead() {
					d.dir.release(k)
				} else {
					d.dir.markUnplaced(k, d)
				}
			} else {
				d.dir.holdUnsynced(k)
			}
		}
		if refused || errors.Is(recErr, core.ErrNoSpace) || errors.Is(recErr, core.ErrClosed) ||
			errors.Is(recErr, segment.ErrStreamStopped) || errors.Is(recErr, segment.ErrStreamClosed) {
			d.log.Debug("a loss record was not appended", "file", l.File, "seq", l.Seq, "err", recErr)
		} else {
			d.log.Error("a loss record was not appended", "file", l.File, "seq", l.Seq, "err", recErr)
		}
	}
	return ev, firstErr
}

// appendRecord appends the loss record to the file's stream and syncs it. It
// returns the record's place, zero when it appended nothing, and the sync's
// error when it placed the record but could not make it durable.
//
// ponytail: the sync is the segment's own, one per loss, issued outside the
// group commit. A failed window of N content records therefore costs N serial
// append-and-fsync rounds on the stream's worker, and no other file's Sync on
// the stream returns meanwhile. Upgrade to appending the window's loss records
// and one SyncAt of the last when a failed window's stall shows up in a
// sync-latency reading. A failure here ends the segment itself, as the stream's
// worker does for a sync of its own: no later sync would claim the records of
// a segment left open, and the failed window is taken from the end this
// requests. For the same reason that failure is not counted in sync_failures,
// which only the worker's syncs and a seal's feed. Upgrade by waiting on
// WaitDurable or Durable for the record's location instead, when a loss
// record's sync failure shows up in a sync_failures reading or the layer's own
// accounting needs it.
func (d *Durability) appendRecord(l Loss) (segment.Location, error) {
	seq, err := d.counters.NextSeq()
	if err != nil {
		return segment.Location{}, err
	}
	h := format.RecordHeader{
		Kind:    format.KindLoss,
		File:    l.File,
		Extent:  l.Extent,
		Seq:     seq,
		Version: l.Version,
		Tag:     l.Share,
	}
	body := format.RecordBody{Loss: format.Loss{
		Dropped:   l.Seq,
		Synced:    l.Synced,
		Offloaded: l.Offloaded,
		Reason:    format.LossReasonOf(l.Reason),
	}}
	streams := d.streams.Load()
	if streams == nil {
		return segment.Location{}, fmt.Errorf("durability: the layer is not started: %w", core.ErrClosed)
	}
	s := streams.For(l.File)
	placed, err := s.Append(h, body)
	if err != nil {
		return segment.Location{}, err
	}
	if d.landed != nil {
		d.landed(h.Tag, placed.Loc.Len)
	}
	// Registered between the append and the sync, so the record is named
	// before the segment carrying it can seal.
	if d.lossRegistered != nil {
		d.lossRegistered(l, seq, placed.Loc)
	}
	if d.beforeSync != nil {
		d.beforeSync()
	}
	// The record's own segment is synced, not whatever the stream is on by
	// now: a roll between the two would otherwise let the sync cover a
	// successor and report the record durable.
	err = s.SyncAt(placed.Loc)
	if err != nil && !errors.Is(err, segment.ErrSegmentEnded) && !errors.Is(err, segment.ErrStreamClosed) {
		// The segment is already ended or the stream closed for those two
		// answers; any other failure left the segment open, so it is ended
		// here for the failed window to be taken from the end event.
		if eerr := s.End(placed.Loc.Segment, err); eerr != nil && !errors.Is(eerr, segment.ErrStreamClosed) {
			d.log.Debug("could not end the segment a loss record's sync failed", "segment", placed.Loc.Segment, "err", eerr)
		}
	}
	return placed.Loc, err
}

// piece is one extent of one dropped record.
type piece struct {
	seq core.Seq
	ext core.Extent
}

// Generation returns the directory's loss generation.
func (d *Durability) Generation() uint64 { return d.dir.load() }

// RaiseWindow raises the generation once for a failed sync window, however
// many records the window held, and returns the new generation.
func (d *Durability) RaiseWindow() uint64 { return d.dir.raise() }

// Failed returns the sequence numbers, sorted, whose loss record could not be
// appended, in the directory's life in this process. A later Durability on
// the directory sees them too.
func (d *Durability) Failed() []core.Seq { return d.dir.failedSeqs() }

// Named reports whether a Lose or a Place already took the piece of the
// dropped record seq over ext: a loss record that landed, one that deferred,
// or one an earlier open found on disk. The open's loss pass asks it before
// its own takes, so it draws no open reserve and waits on no spare pool for
// a record the disk already names.
func (d *Durability) Named(seq core.Seq, ext core.Extent) bool {
	return d.dir.named(piece{seq, ext})
}

// PlacedUnsynced reports whether the piece of the dropped record seq over ext
// is held by a loss record that placed but never synced: Named answers it
// true, yet nothing durable names the loss, so the open's pass appends the
// record again, reported, as a Lose that takes the piece back.
func (d *Durability) PlacedUnsynced(seq core.Seq, ext core.Extent) bool {
	return d.dir.isUnsynced(piece{seq, ext})
}

// DeferredAgainst returns the losses whose record the copy that retires seg
// owes, the entries Lose deferred against it, in the order they deferred.
// Each entry names its Seg; the caller hands the entry back on EndDeferral
// once the copy has the record durable. The entries are the directory's own,
// so they reach this instance through whichever one deferred them.
func (d *Durability) DeferredAgainst(seg core.SegmentID) []Loss {
	return d.dir.deferredAgainst(seg)
}

// DeferredOfTag answers every deferred loss whose dropped content belonged
// to share tag, keyed by the segment each is deferred against, the registry's
// own tag-keyed census a forget of the tag runs: both at its emptiness check
// — a pending loss of the tag is something it holds, whatever the set and
// the index answer — and in its sweep, which the set's per-tag figures
// cannot guide, a deferral sitting against a segment that carries none of
// the tag's bytes escaping them otherwise.
func (d *Durability) DeferredOfTag(tag core.ShareTag) map[core.SegmentID][]Loss {
	return d.dir.deferredOfTag(tag)
}

// EndDeferral ends one deferred loss of seg: the copy that retires it has
// the loss record durable, so the registry gives the entry up. l names the
// entry as DeferredAgainst returned it. An entry leaves on this call alone,
// so the call is idempotent: a second one, or a call whose entry already
// left, answers false and changes nothing; true says an entry left.
func (d *Durability) EndDeferral(seg core.SegmentID, l Loss) bool {
	return d.dir.endDeferral(seg, l)
}

// Place publishes ev, a loss record found at open, for the dropped record
// seq, to this instance's ring. It appends nothing, raises nothing and
// counts nothing: the counters are of drops this run made, and the record
// is already on disk. A piece (seq and ev.Extent) already taken — by a
// Place of whichever open, by the Lose of this instance, of a still live
// one, or of one whose loss's event a reader took away, all of which
// answer for the piece — publishes nothing, so a later open does not
// report the same loss record as new, while another piece of the same
// record is placed on its own. The Lose of an instance stopped before any
// reader reached its loss's event retired with it, and the Place takes
// the piece over: the record the disk names is
// published here, into this instance's own fresh ring, as found at open —
// the ruling that makes the found-at-open ring serve a reopen in one
// process — and the placed claim answers every later open as far as this
// placement's own event was read: a reader that reached it answers for
// the loss for good, while an instance that stopped before any reader
// did leaves the piece to a later open, which reports the loss found at
// open again. The event's Synced is kept as the caller set it.
func (d *Durability) Place(ev core.LossEvent, dropped core.Seq) {
	if !d.dir.takePiece(d, dropped, ev.Extent) {
		return
	}
	ev.AtOpen = true
	d.lmu.Lock()
	published := d.pushRingLocked(ev)
	d.lmu.Unlock()
	// The placed claim carries the event this placement published into
	// this instance's own ring, so it retires on the same terms as a
	// Lose's: past this instance's stop it holds only as far as a reader
	// reached that event.
	d.dir.setClaimEvent(piece{dropped, ev.Extent}, d, published.SeqNum)
}
