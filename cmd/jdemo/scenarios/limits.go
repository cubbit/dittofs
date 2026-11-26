package scenarios

import (
	"context"
	"strings"

	"github.com/cubbit/dittofs/pkg/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

// The budget pair is one derived constant: the pool permanently reserves
// its own quarter of the maximum (SpareTarget 2 + headroom + reserve ≈ 4
// caps of the 8 a 2 GiB world holds), so a 512 MiB share budget sits
// safely inside the ~1 GiB of rollable content — the story's writes meet
// the SHARE's limit, never the journal's or the spare's.
const (
	limitsMaxBytes  = 2 << 30   // the world: 8 caps
	limitsShareEach = 512 << 20 // the share's record-byte budget
	limitsFillEach  = 40 << 20  // one filler write; the reservation holds two of them
)

// LimitsStory is the space half of the paper's story: the share budget,
// the reserve that must be taken before the burst, and what truncate does
// and does not give back.
func LimitsStory() ([]jdemo.Beat, journal.Settings) {
	basic, _ := BasicStory()
	beats := []jdemo.Beat{
		basic[0], // journal online — the banner names the 2 GiB world
		{
			Title:   "attach the share — with a budget",
			Caption: "the share limit is the per-share record-byte budget: writes into this share answer to it before they answer to anything else — basic's story passed zero for the same call",
			Run: func(d *jdemo.Demo) {
				h, err := d.J.Share(d.Tag, limitsShareEach, journal.Version{})
				d.Codef("    h, err := j.Share(%d, %d, journal.Version{})", d.Tag, limitsShareEach)
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				d.H[d.Tag] = h
				d.Yellowf("    // → handle attached: the share's record bytes are capped at %s", jdemo.HumanSize(limitsShareEach))
			},
		},
		{
			Title:   "reserve before the burst",
			Caption: "the reservation is charged the budget it holds — take it while there is budget, or the escape hatch is refused with the rest; this is why the engine reserves before a burst, never after",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				// Reserve the double of one fill: a write costs a little
				// more than its payload — entries, a footer slot — so an
				// exactly-payload reservation cannot hold the very write
				// it was made for.
				res, err := h.Reserve(2*limitsFillEach, 1)
				d.Codef("    res, err := h.Reserve(%d, 1)", 2*limitsFillEach)
				if err != nil {
					d.Yellowf("    // → %v  // the reserve is charged the budget too", err)
					return
				}
				d.Yellowf("    // → reservation held: %s exempt from every limit while it lasts", jdemo.HumanSize(2*limitsFillEach))
				d.LimitsReservation = res
			},
		},
		{
			Title:   "fill the budget — until the share says no",
			Caption: "40 MiB writes, one at a time, until the share's budget refuses one: the journal's own words, counting what is missing",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				d.Codef("    for { v, err := h.WriteAt(ctx, FILE, off, fill40MiB, 0) }")
				off := int64(0)
				n := 0
				for {
					payload := make([]byte, limitsFillEach)
					pattern := []byte("0123456789abcdef")
					for i := range payload {
						payload[i] = pattern[i%len(pattern)]
					}
					werr := jdemo.AwaitSpare(context.Background(), func() error {
						_, err := h.WriteAt(context.Background(), jdemo.StoryFile, off, payload, 0)
						return err
					})
					n++
					if werr != nil {
						d.Yellowf("    // → write %d: %v", n, werr)
						d.LimitsRefused = true
						return
					}
					off += limitsFillEach
					if n >= 40 { // a bound the budget cannot outlast
						d.Yellowf("    // → %d writes, no refusal — the budget math is wrong", n)
						return
					}
				}
			},
		},
		{
			Title:   "the reserved write walks through the refusal",
			Caption: "the same share that just refused a write serves the reservation's: its bytes were held up front — this is the escape hatch, demonstrated in the order the engine must use it",
			Run: func(d *jdemo.Demo) {
				res := d.LimitsReservation
				if res == nil {
					d.Yellowf("    // → no reservation in hand; the reserve-first beat was refused")
					return
				}
				payload := make([]byte, limitsFillEach)
				pattern := []byte("0123456789abcdef")
				for i := range payload {
					payload[i] = pattern[i%len(pattern)]
				}
				v, err := res.WriteAt(context.Background(), jdemo.StoryFile, 512<<20, payload, 0)
				d.Codef("    v, err := res.WriteAt(ctx, FILE, %d, fill40MiB, 0)", 512<<20)
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				d.Yellowf("    // → v%d — served through the refusal the filler just met", v.Counter)
				_ = res.Close()
			},
		},
		{
			Title:   "the headroom math",
			Caption: "used counts every segment at its accounted size — spares and the active segment at the cap, a sealed one at what it holds: the pool's spares are the journal's own overhead, and the world has to hold them as well as the share's budget — read it off the stats look",
			Run: func(d *jdemo.Demo) {
				st := d.J.Stats()
				d.Printf("    max bytes:      %s\n", jdemo.HumanSize(limitsMaxBytes))
				d.Printf("    used:           %s  (%d segments written, %d spares; spares count at the segment cap)\n", jdemo.HumanSize(st.UsedBytes), st.Segments, st.Spares)
				d.Printf("    share budget:   %s  (held %s)\n", jdemo.HumanSize(limitsShareEach), jdemo.HumanSize(st.HeldBytes))
			},
		},
		{
			Title:   "truncate gives back nothing — yet",
			Caption: "the held bytes go, but used does not move: reclaimed space is repack's to free, and nothing is freed until then — the counterintuitive half of the space story",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchHeld, Hint: "held falls by everything past 32"},
				{Kind: jdemo.WatchMarkers, Hint: "a removal marker waits"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				before := d.J.Stats()
				v, terr := h.Truncate(context.Background(), jdemo.StoryFile, 32)
				d.Codef("    v, err := h.Truncate(ctx, FILE, 32)")
				if terr != nil {
					d.Yellowf("    // → %v", terr)
					return
				}
				after := d.J.Stats()
				d.Stash(v, nil)
				d.Yellowf("    // → used before %s, after %s — unchanged; a repack pass frees", jdemo.HumanSize(before.UsedBytes), jdemo.HumanSize(after.UsedBytes))
			},
		},
		{
			Title:   "repack gives the space back",
			Caption: "Repack copies the still-live records out of the best segments, repoints the index and retires the originals: only then does used fall — the space comes back on repack's pass, not on the truncate",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchHeld, Hint: "held does not move: repack discards nothing live"},
			},
			Run: func(d *jdemo.Demo) {
				before := d.J.Stats()
				freed, err := d.J.Repack(context.Background(), 1<<30, journal.RepackScope{})
				d.Codef("    freed, err := j.Repack(ctx, 1<<30, journal.RepackScope{})")
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				after := d.J.Stats()
				d.Yellowf("    // → freed %s; used before %s, after %s; unreclaimed before %s, after %s",
					jdemo.HumanSize(freed), jdemo.HumanSize(before.UsedBytes), jdemo.HumanSize(after.UsedBytes),
					jdemo.HumanSize(before.UnreclaimedBytes), jdemo.HumanSize(after.UnreclaimedBytes))
			},
		},
		{
			Title:   "the honesty ledger",
			Caption: "the share limit, the escape hatch and repack are live wiring; the offload drain to the remote tier is not built in this journal yet, and the journal says so in its own words",
			Run: func(d *jdemo.Demo) {
				d.Codef("    err := h.Offload(ctx, FILE, through, limit, widen, sink)")
				d.Yellowf("    // → journal: not implemented")
			},
		},
	}
	return beats, journal.Settings{
		Streams:         2,
		SpareTarget:     2,
		HeadroomRecords: 1024,
		MaxBytes:        limitsMaxBytes,
	}
}

var _ = strings.Contains
