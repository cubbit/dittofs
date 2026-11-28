package assembler_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// valid is a configuration New accepts; each case changes one field of it.
func valid() carver.AssemblerConfig {
	return carver.AssemblerConfig{
		BlockTarget: 4 << 20,
		MaxChunks:   512,
		KeyBudget:   10_000,
		BlockKeys:   3,
		FileKeys:    1,
		MaxAge:      time.Minute,
	}
}

// checkNew asserts New accepts c when field is empty, and otherwise refuses it
// with ErrInvalidSettings, a nil fold and an error naming field.
func checkNew(t *testing.T, c carver.AssemblerConfig, field string) {
	t.Helper()
	f, err := assembler.New(c)
	if field == "" {
		if err != nil || f == nil {
			t.Fatalf("New(%+v): got (%v, %v), want a fold and no error", c, f, err)
		}
		return
	}
	if !errors.Is(err, carver.ErrInvalidSettings) || f != nil {
		t.Fatalf("New(%+v): got (%v, %v), want (nil, ErrInvalidSettings)", c, f, err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("New(%+v): error %q does not name %s", c, err, field)
	}
}

func TestNewRefusesBlockTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int64
		field  string // "BlockTarget" when refused, empty when accepted
	}{
		{"0", 0, "BlockTarget"},
		{"512 KiB", 512 << 10, "BlockTarget"},
		{"128 MiB", 128 << 20, "BlockTarget"},
		{"1 MiB - 1", 1<<20 - 1, "BlockTarget"},
		{"64 MiB + 1", 64<<20 + 1, "BlockTarget"},
		{"1 MiB", 1 << 20, ""},
		{"64 MiB", 64 << 20, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			c.BlockTarget = tc.target
			checkNew(t, c, tc.field)
		})
	}
}

func TestNewRefusesConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string // the field a refusal names; empty for an accepted config
		edit  func(*carver.AssemblerConfig)
	}{
		{"valid", "", func(*carver.AssemblerConfig) {}},
		{"no age, no keys per block or file", "", func(c *carver.AssemblerConfig) { c.BlockKeys, c.FileKeys, c.MaxAge = 0, 0, 0 }},
		{"count cap 1", "", func(c *carver.AssemblerConfig) { c.MaxChunks = 1 }},
		{"count cap 1024", "", func(c *carver.AssemblerConfig) { c.MaxChunks = 1024 }},
		{"key budget 1", "", func(c *carver.AssemblerConfig) { c.KeyBudget = 1 }},
		{"count cap 0", "MaxChunks", func(c *carver.AssemblerConfig) { c.MaxChunks = 0 }},
		{"count cap 1025", "MaxChunks", func(c *carver.AssemblerConfig) { c.MaxChunks = 1025 }},
		{"key budget 0", "KeyBudget", func(c *carver.AssemblerConfig) { c.KeyBudget = 0 }},
		{"per-block keys -1", "BlockKeys", func(c *carver.AssemblerConfig) { c.BlockKeys = -1 }},
		{"per-file keys -1", "FileKeys", func(c *carver.AssemblerConfig) { c.FileKeys = -1 }},
		{"max age -1ns", "MaxAge", func(c *carver.AssemblerConfig) { c.MaxAge = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.edit(&c)
			checkNew(t, c, tc.field)
		})
	}
}

// cfg is a pass's limits with a key budget no test here reaches.
func cfg(target int64, n int) carver.AssemblerConfig {
	return carver.AssemblerConfig{BlockTarget: target, MaxChunks: n, KeyBudget: 1 << 30, MaxAge: time.Minute}
}

// The P1, P2 and P4 check: every plan ends on a chunk, overshoots the target
// by less than its last chunk, and holds at most N.
func TestFoldPacksWholeChunksToTheTargetOrTheCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       carver.AssemblerConfig
		chunk     int32
		plans     int
		firstPlan int // carried chunks in the first plan
	}{
		{"1 KiB chunks, N 1024", cfg(1<<20, 1024), 1 << 10, 5, 1024},
		{"1 KiB chunks, N 100", cfg(1<<20, 100), 1 << 10, 50, 100},
		{"3 KiB chunks overshoot by 2 KiB", cfg(1<<20, 1024), 3 << 10, 15, 342},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stream(4096, lengths(5000, tc.chunk))
			now := t0.Add(2 * time.Minute)
			r := fold(t, tc.cfg, s, now)
			checkPlans(t, tc.cfg, s, r.plans, now, r.held)
			if len(r.plans) != tc.plans {
				t.Fatalf("got %d plans, want %d", len(r.plans), tc.plans)
			}
			if got := len(r.plans[0].Carried); got != tc.firstPlan {
				t.Errorf("first plan: got %d carried chunks, want %d", got, tc.firstPlan)
			}
		})
	}
}

// Blocks are cut across files by the target alone: a file's end never closes
// one, so no plan before the last is short.
func TestFoldPacksAcrossFiles(t *testing.T) {
	var small, ends [][]int32
	for range 1000 {
		small = append(small, lengths(4, 16<<10))
	}
	// 17 chunks a file at 16 a block: the files end at every chunk position
	// of a block in turn.
	for range 16 {
		ends = append(ends, lengths(17, 64<<10))
	}
	for _, tc := range []struct {
		name  string
		cfg   carver.AssemblerConfig
		files [][]int32
		plans int
	}{
		{"10^3 files of 64 KiB", cfg(4<<20, 1024), small, 16},
		{"file ends at every block position", cfg(1<<20, 1024), ends, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stream(0, tc.files...)
			r := fold(t, tc.cfg, s, t0)
			checkPlans(t, tc.cfg, s, r.plans, t0, r.held)
			if len(r.plans) != tc.plans {
				t.Errorf("got %d plans, want %d", len(r.plans), tc.plans)
			}
		})
	}
}

// A short last plan is held while its oldest chunk is younger than MaxAge and
// put from MaxAge on; a full one is never held.
func TestFinishHoldsAShortLastPlanUntilMaxAge(t *testing.T) {
	oldest := t0.Add(-time.Hour)
	short := stream(0, lengths(3, 64<<10))
	short[1].Oldest = oldest // the oldest ref is not the first
	unset := stream(0, lengths(3, 64<<10))
	unset[2].Oldest = time.Time{}
	for _, tc := range []struct {
		name string
		cfg  carver.AssemblerConfig
		s    []carver.ChunkInfo
		now  time.Time
		held bool
	}{
		{"short, 1ns under MaxAge", cfg(1<<20, 1024), short, oldest.Add(time.Minute - 1), true},
		{"short, at MaxAge", cfg(1<<20, 1024), short, oldest.Add(time.Minute), false},
		{"short, past MaxAge", cfg(1<<20, 1024), short, oldest.Add(time.Hour), false},
		{"short, an Oldest unset", cfg(1<<20, 1024), unset, t0, false},
		{"full at the target, young", cfg(1<<20, 1024), stream(0, lengths(16, 64<<10)), t0, false},
		{"full at the cap, young", cfg(1<<20, 3), short, oldest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fold(t, tc.cfg, tc.s, tc.now)
			checkPlans(t, tc.cfg, tc.s, r.plans, tc.now, r.held)
			if len(r.plans) != 1 || r.held != tc.held {
				t.Errorf("got %d plans, held %v; want 1 plan, held %v", len(r.plans), r.held, tc.held)
			}
		})
	}
}

// A fold serves one pass: Add or Finish after Finish panics.
func TestFoldPanicsAfterFinish(t *testing.T) {
	for _, tc := range []struct {
		name string
		use  func(*assembler.Fold)
	}{
		{"Add", func(f *assembler.Fold) { f.Add(carver.ChunkInfo{}) }},
		{"Finish", func(f *assembler.Fold) { f.Finish(t0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := assembler.New(cfg(1<<20, 1024))
			if err != nil {
				t.Fatal(err)
			}
			if last, held := f.Finish(t0); last != nil || held {
				t.Fatalf("Finish on an empty fold: got (%v, %v), want (nil, false)", last, held)
			}
			defer func() {
				if recover() == nil {
					t.Errorf("%s after Finish: no panic, want one", tc.name)
				}
			}()
			tc.use(f)
		})
	}
}

// The P3 check: no plan carries an all-zero chunk, and each is a ZeroRef.
func TestFoldNeverCarriesAZeroChunk(t *testing.T) {
	s := stream(4096, lengths(3000, 1<<10))
	for i := 0; i < len(s); i += 3 {
		zero(s, i, i+1)
	}
	c := cfg(1<<20, 1024)
	r := fold(t, c, s, t0)
	checkPlans(t, c, s, r.plans, t0, r.held)
	// 2,000 carried chunks of 1 KiB at a 1 MiB target.
	if len(r.plans) != 2 {
		t.Fatalf("got %d plans, want 2", len(r.plans))
	}
	if got := len(r.plans[0].Carried); got != 1024 {
		t.Errorf("first plan: got %d carried chunks, want 1024", got)
	}
}

// The P5 check: the target counts carried bytes alone, so runs of zero refs
// between carried chunks close no block short.
func TestFoldCountsOnlyCarriedBytesTowardTheTarget(t *testing.T) {
	var files [][]int32
	for range 8 {
		files = append(files, append(lengths(64, 1<<20), lengths(24, 64<<10)...))
	}
	s := stream(0, files...)
	for i := 0; i < len(s); i += 88 {
		zero(s, i, i+64)
	}
	c := cfg(4<<20, 1024)
	r := fold(t, c, s, t0)
	checkPlans(t, c, s, r.plans, t0, r.held)
	// 8 runs of 1.5 MiB carried at a 4 MiB target.
	if len(r.plans) != 3 {
		t.Errorf("got %d plans, want 3", len(r.plans))
	}
}

// Zero refs ride in the nearest block's commit: those before the first
// carried chunk in the first plan, those between two full blocks in the
// earlier one, those after the last in the last. A full block stays open for
// the zero refs after it.
func TestFoldPlacesZeroRefsInTheNearestPlan(t *testing.T) {
	// 3 zeros, 16 carried (a full block), 3 zeros, 16 carried, 3 zeros.
	s := stream(0, lengths(41, 64<<10))
	zero(s, 0, 3)
	zero(s, 19, 22)
	zero(s, 38, 41)
	c := cfg(1<<20, 1024)
	r := fold(t, c, s, t0)
	checkPlans(t, c, s, r.plans, t0, r.held)
	want := [][2]int{{0, 22}, {22, 41}} // each plan's span of the input
	if len(r.plans) != len(want) {
		t.Fatalf("got %d plans, want %d", len(r.plans), len(want))
	}
	for i, p := range r.plans {
		if len(p.Refs) == 0 {
			t.Fatalf("plan %d: no refs", i)
		}
		if first, n := p.Refs[0], len(p.Refs); first != s[want[i][0]] || n != want[i][1]-want[i][0] {
			t.Errorf("plan %d: got %d refs from %+v, want input chunks %d to %d", i, n, first, want[i][0], want[i][1])
		}
	}
}

// A pass of zero chunks alone gives one plan with no carried chunk, the
// zero-ref commit, which is never held: holding it would leave the pass's
// zeros dirty until the age ceiling.
func TestFinishPutsAPassOfZerosAlone(t *testing.T) {
	s := stream(0, lengths(10, 64<<10))
	zero(s, 0, len(s))
	c := cfg(1<<20, 1024)
	r := fold(t, c, s, t0)
	checkPlans(t, c, s, r.plans, t0, r.held)
	if len(r.plans) != 1 || r.held {
		t.Errorf("got %d plans, held %v; want 1 plan, not held", len(r.plans), r.held)
	}
}

// A short last plan's age counts its zero refs: one at MaxAge before young
// carried chunks puts it.
func TestFinishAgesAShortLastPlanByItsZeroRefs(t *testing.T) {
	s := stream(0, lengths(4, 64<<10))
	zero(s, 0, 1)
	s[0].Oldest = t0.Add(-time.Minute)
	c := cfg(1<<20, 1024)
	r := fold(t, c, s, t0)
	checkPlans(t, c, s, r.plans, t0, r.held)
	if len(r.plans) != 1 || r.held {
		t.Errorf("got %d plans, held %v; want 1 plan, put", len(r.plans), r.held)
	}
}

// The P6 check: a chunk repeated within one block's span is carried once and
// referenced each time, and carried again in a later block; a repeat adds no
// carried bytes toward the target.
func TestFoldCarriesARepeatOncePerBlock(t *testing.T) {
	// The first chunk three times in the first block's span, which holds 16
	// distinct chunks of 64 KiB in 18 refs, and once as the next chunk after
	// it fills, which closes it though the plan carries that hash.
	three := stream(0, lengths(21, 64<<10))
	three[5].Hash, three[10].Hash, three[18].Hash = three[0].Hash, three[0].Hash, three[0].Hash
	// One chunk, 100 repeats of it, 15 more distinct chunks and one past them.
	many := stream(0, lengths(117, 64<<10))
	for i := 1; i <= 100; i++ {
		many[i].Hash = many[0].Hash
	}
	for _, tc := range []struct {
		name string
		s    []carver.ChunkInfo
		refs []int // refs in each plan
		reps []int // refs to the repeated hash in each plan
	}{
		{"three in one block, once past it", three, []int{18, 3}, []int{3, 1}},
		{"a block of repeats", many, []int{116, 1}, []int{101, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(1<<20, 1024)
			r := fold(t, c, tc.s, t0)
			checkPlans(t, c, tc.s, r.plans, t0, r.held)
			if len(r.plans) != len(tc.refs) {
				t.Fatalf("got %d plans, want %d", len(r.plans), len(tc.refs))
			}
			h := tc.s[0].Hash
			for i, p := range r.plans {
				if len(p.Refs) != tc.refs[i] || countHash(p.Refs, h) != tc.reps[i] {
					t.Errorf("plan %d: got %d refs, %d to the repeated hash; want %d, %d to it",
						i, len(p.Refs), countHash(p.Refs, h), tc.refs[i], tc.reps[i])
				}
			}
		})
	}
}

// countHash counts the chunks of cs whose hash is h.
func countHash(cs []carver.ChunkInfo, h entity.ChunkHash) int {
	n := 0
	for _, c := range cs {
		if c.Hash == h {
			n++
		}
	}
	return n
}

// budget is a pass's limits at a 1 MiB target and N 1,024, which no stream of
// the key-budget check fills, so only the key budget closes its plans.
func budget(k, blockKeys, fileKeys int) carver.AssemblerConfig {
	return carver.AssemblerConfig{BlockTarget: 1 << 20, MaxChunks: 1024, KeyBudget: k, BlockKeys: blockKeys, FileKeys: fileKeys, MaxAge: time.Minute}
}

// keyed sets the Keys of s's chunks from i on, in order, and returns s.
func keyed(s []carver.ChunkInfo, i int, keys ...int) []carver.ChunkInfo {
	for j, k := range keys {
		s[i+j].Keys = k
	}
	return s
}

// The P2 key-budget check: a plan closes at the last ref within K, zero refs
// counted, and the next starts with the ref that did not fit; zero refs that
// fit no block's commit commit alone, each within K; the per-block keys are
// charged only to a plan that carries a chunk, the per-file keys once per file
// in each plan; a chunk alone over K sits alone.
func TestFoldKeepsEveryPlanWithinTheKeyBudget(t *testing.T) {
	chunk := int32(64 << 10)
	for _, tc := range []struct {
		name string
		cfg  carver.AssemblerConfig
		s    func() []carver.ChunkInfo
		refs []int // refs in each plan
		keys []int // each plan's keys
	}{
		{"carried chunks reach K before the target", budget(100, 5, 1), func() []carver.ChunkInfo {
			return keyed(stream(0, lengths(10, chunk)), 0, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30)
		}, []int{3, 3, 3, 1}, []int{96, 96, 96, 36}},
		{"zero runs of 10^4 replacing refs", budget(5000, 3, 1), func() []carver.ChunkInfo {
			s := stream(0, lengths(20009, chunk))
			for _, from := range []int{3, 10006} {
				zero(s, from, from+10_000)
				for i := from; i < from+10_000; i++ {
					s[i].Keys = 2
				}
			}
			return s
		}, []int{2499, 2499, 2499, 2499, 2499, 2499, 2499, 2499, 17}, []int{4999, 4999, 4999, 4999, 4999, 4999, 4999, 4999, 35}},
		{"zero refs that fit no block commit alone", budget(100, 1, 1), func() []carver.ChunkInfo {
			s := stream(0, lengths(22, chunk))
			zero(s, 1, 21)
			s[0].Keys, s[21].Keys = 90, 90
			return s
		}, []int{9, 12, 1}, []int{100, 13, 92}},
		{"zero refs within K need no block keys", budget(20, 5, 1), func() []carver.ChunkInfo {
			s := stream(0, lengths(18, chunk))
			zero(s, 0, 18)
			return s
		}, []int{18}, []int{19}},
		{"a zero run past K", budget(20, 5, 1), func() []carver.ChunkInfo {
			s := stream(0, lengths(45, chunk))
			zero(s, 0, 45)
			return s
		}, []int{19, 19, 7}, []int{20, 20, 8}},
		{"a chunk at exactly K", budget(20, 5, 1), func() []carver.ChunkInfo {
			return keyed(stream(0, lengths(4, chunk)), 0, 14, 1, 13, 1)
		}, []int{1, 2, 1}, []int{20, 20, 7}},
		{"zero refs beside carried chunks at a small K", budget(10, 2, 1), func() []carver.ChunkInfo {
			s := stream(0, lengths(13, chunk), lengths(12, chunk), lengths(5, chunk))
			zero(s, 0, 5)
			zero(s, 8, 25)
			zero(s, 26, 30)
			return s
		}, []int{7, 6, 9, 6, 2}, []int{10, 9, 10, 10, 3}},
		{"files of three chunks", budget(10, 0, 1), func() []carver.ChunkInfo {
			var files [][]int32
			for range 6 {
				files = append(files, lengths(3, chunk))
			}
			return stream(0, files...)
		}, []int{7, 7, 4}, []int{10, 10, 6}},
		{"a chunk alone over K", budget(20, 5, 1), func() []carver.ChunkInfo {
			return keyed(stream(0, lengths(3, chunk)), 1, 30)
		}, []int{1, 1, 1}, []int{7, 36, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s()
			r := fold(t, tc.cfg, s, t0)
			checkPlans(t, tc.cfg, s, r.plans, t0, r.held)
			if len(r.plans) != len(tc.refs) {
				t.Fatalf("got %d plans, want %d", len(r.plans), len(tc.refs))
			}
			for i, p := range r.plans {
				if k := planKeys(tc.cfg, p.Refs); len(p.Refs) != tc.refs[i] || k != tc.keys[i] {
					t.Errorf("plan %d: got %d refs, %d keys; want %d refs, %d keys", i, len(p.Refs), k, tc.refs[i], tc.keys[i])
				}
			}
		})
	}
}
