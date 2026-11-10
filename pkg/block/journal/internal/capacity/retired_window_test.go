package capacity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestTake_RetiredRacingTheFirstSeed stages an unseeded tag whose first
// take races a retirement of the tag's own bytes: the retirement's own
// move of the reporting layer's figures lands first — the settled bytes
// down, the cumulative retired bytes unchanged, neither on disk yet — so
// the take's seed names the post-retirement settled figure, and the
// retirement's own late call arrives after the seed committed. Under the
// design that charged a Retired call's bytes unconditionally, the call
// subtracted its bytes from a figure the seed had already lowered them
// out of: the tally undercounted by the retired bytes and the share's
// limit failed open, admitting writes past it by up to one retirement per
// racing tag. The seed reads the figures as a pair and every Retired call
// measures its delta against the baseline the pair names, so the late call
// computes a delta of zero and the tally holds the truth. The staging is
// deterministic: the retirement's move is made inside the figures read
// itself — the seam the seed and the retirement share — and no timers run.
//
// The figures, in the admission's own arithmetic: a 2792-byte payload
// against the rig's generous room costs rb(2792) = 2896, whole ask 3000
// with the extra record's charge; a 2800-byte payload costs 2904, whole
// ask 3008. The layer holds 3000 record bytes of the tag; the retirement
// frees 2000 of them, so the post-retirement figures the seed reads are
// 1000 settled beside 2000 retired. The limit 4000 holds the first ask
// exactly beside the settled 1000, and the second ask misses by 8 — while
// a tally lowered below the truth by the late call carried it whole.
func TestTake_RetiredRacingTheFirstSeed(t *testing.T) {
	r := newRig(t, nil)
	r.f.setShare(tag1, 3000)
	ask := recordSplit(2792, roomBig, testCap) + slackOverhead()
	if ask != 3000 {
		t.Fatalf("the 2792-payload ask = %d, want 3000; the test's figures depend on it", ask)
	}

	seeded := atomic.Bool{}
	r.c.cfg.ShareFigures = func(tag core.ShareTag) (settled, retired int64) {
		if seeded.CompareAndSwap(false, true) {
			// The retirement's own move of the layer's figures lands
			// inside the read: the settled bytes fall to 1000 and the
			// cumulative retired bytes rise to 2000, while the
			// retirement's own Retired call is still to come.
			r.f.setShare(tag, 1000)
			r.f.setRetired(tag, 2000)
		}
		return r.f.shareFigures(tag)
	}

	// The first take seeds the tag on the post-retirement figures: one
	// whole ask against the settled 1000, admitted at the 4000 limit.
	q := req()
	q.Payload = 2792
	q.ShareLimit = 4000
	g, err := r.c.Take(context.Background(), q)
	if err != nil {
		t.Fatalf("the first take after the retirement's move: %v", err)
	}
	g.Release()

	// The retirement's own call arrives after the seed, with the move the
	// pair already took into account — the call whose bytes the old
	// design subtracted a second time.
	r.c.Retired(tag1)

	// The take the window must not admit: a whole ask of 3008 against the
	// settled 1000 on the 4000 limit is refused naming the share, eight
	// bytes short — a tally lowered below the truth by the late call
	// would have carried it whole.
	q2 := req()
	q2.Payload = 2800
	q2.ShareLimit = 4000
	_, terr := r.c.Take(context.Background(), q2)
	var ns *core.NoSpaceError
	if !errors.As(terr, &ns) || ns.Limit != core.LimitShare {
		t.Fatalf("the second take = %v, want the share's refusal", terr)
	}
	if want := 1000 + recordBytes(2800) + slackOverhead() - 4000; ns.Missing != want {
		t.Fatalf("Missing = %d, want %d: the settled figure plus the whole 3008 ask over the 4000 limit", ns.Missing, want)
	}
}
