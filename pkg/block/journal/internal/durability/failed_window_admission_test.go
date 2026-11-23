package durability

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// lossRefusals is a slog.Handler that keeps the error of every "a loss
// record was not appended" record, so a test can read why a loss path that
// drops its error refused the record.
type lossRefusals struct {
	mu   sync.Mutex
	errs []error
}

func (h *lossRefusals) Enabled(context.Context, slog.Level) bool { return true }
func (h *lossRefusals) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *lossRefusals) WithGroup(string) slog.Handler            { return h }

func (h *lossRefusals) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "a loss record was not appended" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && a.Key == "err" {
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
		}
		return true
	})
	return nil
}

func (h *lossRefusals) taken() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.errs)
}

// TestAFailedWindowsLossRefusedAtTheHeadroomIsLeftForTheNextOpen covers a
// failed window's loss at a spent headroom: the loss runs on the stream's
// worker, through loseMoved, and its record's admission is refused for the
// headroom, so no loss record is appended and nothing is drawn. The drop
// stands and its event is published, the sequence number joins the failed
// list the next open names, and the piece is handed back, so the open's own
// raise of the same loss draws the open reserve and appends its record.
func TestAFailedWindowsLossRefusedAtTheHeadroomIsLeftForTheNextOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})
	logs := &lossRefusals{}
	fr := &failRig{resolved: make(chan core.SegmentID, 64)}
	fr.rig = newRigConfigured(t, 1, func(c *Config) {
		c.Resolved = func(_ uint32, seg core.SegmentID) {
			fr.atResolved.Store(fr.passes.Load())
			fr.resolved <- seg
		}
		c.Log = slog.New(logs)
	})
	fr.d.afterPass = func() { fr.passes.Add(1) }

	// A headroom of two records at the journal's limit, whose drawing side is
	// already spent: only the open reserve is left, which a loss found while
	// the journal runs may not draw.
	h := capacity.NewHeadroom(capacity.HeadroomConfig{
		Records:    2,
		Footprint:  func() int64 { return 1 << 20 },
		MaxBytes:   1,
		SegmentCap: 1,
		Admit:      func() error { return nil },
	})
	spent, err := h.Admit(capacity.Loss)
	if err != nil {
		t.Fatalf("drawing the headroom's drawing side: %v", err)
	}
	h.Drawn(spent, 1)
	fr.d.SetLossAdmission(func(atOpen bool) (func(placed core.SegmentID), error) {
		class := capacity.Loss
		if atOpen {
			class = capacity.Open
		}
		draw, err := h.Admit(class)
		if err != nil {
			return nil, err
		}
		return func(placed core.SegmentID) {
			if placed != 0 {
				h.Drawn(draw, placed)
				return
			}
			h.Refund(draw)
		}, nil
	})
	fr.d.Start(fr.streams)
	draws := h.Draws()

	f := file(1)
	loc, v := fr.publish(t, f, 0, "aaaa")
	seq := core.Seq(fr.lastSeq.Load())
	fr.failSegmentOf(t, loc.Segment, f)
	fr.awaitResolved(t)
	fr.awaitPassEnd(t, fr.atResolved.Load())

	refusals := logs.taken()
	var ns *core.NoSpaceError
	if len(refusals) != 1 || !errors.As(refusals[0], &ns) || ns.Limit != core.LimitHeadroom {
		t.Fatalf("the failed window's loss record was refused with %v, want one NoSpaceError naming the headroom", refusals)
	}
	if recs := fr.lossRecordsOnDisk(t); len(recs) != 0 {
		t.Fatalf("%d loss records on disk after the refusal, want none", len(recs))
	}
	if got := h.Draws(); got != draws {
		t.Fatalf("headroom_draws = %d after the refused loss, want %d: the open reserve was drawn", got, draws)
	}
	evs := fr.events()
	if len(evs) != 1 || evs[0].Reason != core.LossFailedWindow || evs[0].Version != v {
		t.Fatalf("loss events %+v after the refusal, want the failed window's one: the drop stands", evs)
	}
	if got := fr.d.Failed(); !slices.Contains(got, seq) {
		t.Fatalf("the failed list is %v, want it to hold seq %d for the next open", got, seq)
	}

	// The same loss raised again as an open's own: the piece was handed
	// back, so it is taken again, draws the open reserve and appends, where
	// a piece the refusal held would answer the zero event and append
	// nothing.
	ev, err := fr.d.Lose(context.Background(), Loss{
		Share: 1, File: f, Extent: core.Extent{Off: 0, Len: 4}, Version: v, Seq: seq,
		Reason: core.LossFailedWindow, Raised: true, AtOpen: true,
	})
	if err != nil {
		t.Fatalf("the open's raise of the refused loss: %v", err)
	}
	if ev == (core.LossEvent{}) {
		t.Fatal("the open's raise of the refused loss returned the zero event: its piece stayed taken")
	}
	if recs := fr.lossRecordsOnDisk(t); len(recs) != 1 || recs[0].Dropped != seq {
		t.Fatalf("loss records on disk after the open's raise = %+v, want one naming seq %d: the piece stayed taken", recs, seq)
	}
	if got := h.Draws(); got != draws+1 {
		t.Fatalf("headroom_draws = %d after the open's raise, want %d", got, draws+1)
	}
}
