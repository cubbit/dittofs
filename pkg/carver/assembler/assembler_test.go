package assembler_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
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
	short := stream(0, lengths(3, 64<<10))
	short[1].Oldest = t0.Add(-time.Hour) // the oldest ref is not the first
	oldest := t0.Add(-time.Hour)
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
		{"Add", func(f *assembler.Fold) { f.Add(stream(0, lengths(1, 1))[0]) }},
		{"Finish", func(f *assembler.Fold) { f.Finish(t0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := assembler.New(cfg(1<<20, 1024))
			if err != nil {
				t.Fatal(err)
			}
			f.Finish(t0)
			defer func() {
				if recover() == nil {
					t.Errorf("%s after Finish: no panic, want one", tc.name)
				}
			}()
			tc.use(f)
		})
	}
}
