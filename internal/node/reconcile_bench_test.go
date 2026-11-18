package node_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// benchShares is the share count RFC 23 §8's bound on a live change is
// measured at.
const benchShares = 10_000

// benchChurn is how many other shares change in each pass beside the one
// timed.
//
// decision: steady churn is ten share state changes a second, each its own
// write and so its own move of the share list version. No rate is specified;
// raise it once an installation's audit shows a higher one.
const benchChurn = 10

// BenchmarkReconcile times the reconcile pass a live change waits on, over
// benchShares shares on a node whose store is on disk: "cold", one pass
// binding every share with none bound, and "churn", one share's state change
// applied in a pass beside benchChurn others, with every share bound. Each
// is timed from the commit that moves the share list version to the end of
// the pass that binds the change. The pass is woken as a write on this node
// wakes it, so the figure leaves out the wait for the next turn: RFC 23 §8's
// 5 s holds while the figure plus reconcileInterval stays under it.
//
// Run it as its suite does, with -benchtime=1x: the cold case builds a node
// and writes its shares outside the timed region of every iteration.
func BenchmarkReconcile(b *testing.B) {
	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			h := nodetest.New(b)
			if err := h.Node().Start(b.Context()); err != nil {
				b.Fatalf("Start: %v", err)
			}
			configure(b, h, storeA)
			shares := benchCreate(b, h)
			want := counters(b, h).ShareList
			b.StartTimer()
			benchPass(b, h, want)
			b.StopTimer()
			for _, s := range shares {
				benchBound(b, h, s.ID, entity.ShareEnabled)
			}
			if err := h.Node().Stop(b.Context()); err != nil {
				b.Fatalf("Stop: %v", err)
			}
		}
	})

	b.Run("churn", func(b *testing.B) {
		h := nodetest.New(b)
		started(b, h)
		configure(b, h, storeA)
		shares := benchCreate(b, h)
		benchPass(b, h, counters(b, h).ShareList)
		state := make([]entity.ShareState, len(shares))
		for i := range state {
			state[i] = entity.ShareEnabled
		}
		next := 0
		// flip moves the next share in turn to the other state, and returns
		// it in the state it moved to.
		flip := func() (entity.ShareID, entity.ShareState) {
			i := next % len(shares)
			next++
			to := entity.ShareQuiesced
			if state[i] == entity.ShareQuiesced {
				to = entity.ShareEnabled
			}
			if err := h.Store().ControlPlane().SetShareState(b.Context(), shares[i].ID, state[i], to); err != nil {
				b.Fatalf("share %s state %v to %v: %v", shares[i].Name, state[i], to, err)
			}
			state[i] = to
			return shares[i].ID, to
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			for range benchChurn {
				flip()
			}
			id, to := flip()
			want := counters(b, h).ShareList
			b.StartTimer()
			benchPass(b, h, want)
			b.StopTimer()
			benchBound(b, h, id, to)
		}
	})
}

// benchCreate creates benchShares shares, each in a namespace of its own on
// storeA, a thousand to a write.
func benchCreate(b *testing.B, h *nodetest.Harness) []entity.Share {
	b.Helper()
	shares := make([]entity.Share, 0, benchShares)
	for len(shares) < benchShares {
		names := make([]string, min(1000, benchShares-len(shares)))
		for i := range names {
			names[i] = fmt.Sprintf("s%d", len(shares)+i)
		}
		shares = append(shares, createShares(b, h, storeA, names...)...)
	}
	return shares
}

// benchPass wakes reconcile, as a write on this node does, and waits for the
// pass to end, failing unless it reconciled the share list through want.
func benchPass(b *testing.B, h *nodetest.Harness, want uint64) {
	b.Helper()
	moved := h.Node().Moved()
	h.Node().Wake()
	select {
	case <-moved:
	case <-time.After(time.Hour): // a backstop for a pass that never ends; a slow one is the figure
		b.Fatalf("reconcile pass: not ended after an hour")
	}
	if got := h.Node().Positions().ShareList; got < want {
		b.Fatalf("share list position after the pass: got %d, want at least %d", got, want)
	}
}

// benchBound fails unless reconcile bound share id in state.
func benchBound(b *testing.B, h *nodetest.Harness, id entity.ShareID, state entity.ShareState) {
	b.Helper()
	if got, ok := h.Node().Bound(id); !ok || got != state {
		b.Fatalf("share %s after the pass: bound %v in %v, want bound in %v", id, ok, got, state)
	}
}
