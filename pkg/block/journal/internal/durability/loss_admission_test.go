package durability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// atLimitHeadroom wires the env's loss records through a real headroom of two
// records whose journal is at its limit: the first record is the drawing
// side, the second the open reserve, which a loss found while the journal
// runs may not draw and an open's own loss may.
func atLimitHeadroom(e *env) *capacity.Headroom {
	h := capacity.NewHeadroom(capacity.HeadroomConfig{
		Records:    2,
		Footprint:  func() int64 { return 1 << 20 },
		MaxBytes:   1,
		SegmentCap: 1,
		Admit:      func() error { return nil },
	})
	// The wiring the core makes: the open class for a loss an open's own leg
	// raises, the loss class for every other, and the settle drawn only when
	// the record placed and refunded when it did not.
	e.d.SetLossAdmission(func(atOpen bool) (settle func(placed core.SegmentID), err error) {
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
	return h
}

// TestLoseDrawsItsRecordThroughTheHeadroomByTheClassOfCaller checks the loss
// record's admission at the headroom's limit: a loss raised while the journal
// runs takes the headroom's drawing side, a second one is refused once only
// the open reserve is left — a loss found on a read must not take the space
// the next open needs — and the refusal hands the piece back, so the same
// loss raised as an open's own draws the reserve, appends and is counted in
// headroom_draws, where a piece the refusal held would have answered the
// zero event.
func TestLoseDrawsItsRecordThroughTheHeadroomByTheClassOfCaller(t *testing.T) {
	e := newEnv(t)
	h := atLimitHeadroom(e)
	ctx := context.Background()

	// The drawing side: the first runtime loss is admitted and appends.
	before := h.Draws()
	if _, err := e.d.Lose(ctx, loss(1)); err != nil {
		t.Fatalf("the first runtime loss at the limit: %v", err)
	}
	if got := h.Draws(); got != before+1 {
		t.Fatalf("headroom_draws = %d after the admitted runtime loss, want %d", got, before+1)
	}

	// Only the open reserve is left, which a runtime loss may not draw.
	l := loss(2)
	_, err := e.d.Lose(ctx, l)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitHeadroom || ns.Unit != core.UnitEntries || ns.Missing <= 0 {
		t.Fatalf("a runtime loss at the open reserve = %v, want a NoSpaceError naming the headroom", err)
	}
	if got := h.Draws(); got != before+1 {
		t.Fatalf("headroom_draws = %d after the refused runtime loss, want %d: the reserve was drawn", got, before+1)
	}
	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Fatalf("%d loss records after the refused loss, want the admitted one alone", len(lh))
	}

	// The piece the refusal handed back: the same loss raised as an open's
	// own draws the reserve and appends, counted by headroom_draws.
	l.AtOpen = true
	ev, err := e.d.Lose(ctx, l)
	if err != nil {
		t.Fatalf("raising the refused loss as an open's own: %v", err)
	}
	if ev == (core.LossEvent{}) {
		t.Fatal("raising the refused loss returned the zero event: its piece stayed taken")
	}
	if got := h.Draws(); got != before+2 {
		t.Fatalf("headroom_draws = %d after the open's own loss, want %d: its draw went unrecorded", got, before+2)
	}
	if lh, _ := e.lossRecords(); len(lh) != 2 {
		t.Fatalf("%d loss records, want the runtime one and the open one", len(lh))
	}
}
