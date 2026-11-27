package carve

import (
	"errors"
	"runtime"
	"runtime/metrics"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// targets is every supported Target, 64 KiB to 4 MiB.
var targets = []int{64 << 10, 128 << 10, 256 << 10, 512 << 10, 1 << 20, 2 << 20, 4 << 20}

var (
	idKey  = [32]byte{0: 1}
	cutKey = [32]byte{0: 2}
)

// Every supported Target is accepted with the sizes and bit counts it
// derives; a Target at, under or over each limit, or not a power of two, is
// refused by CheckTarget and NewSettings alike, with nothing usable.
func TestTargets(t *testing.T) {
	seen := map[[2]int]int{}
	for i, target := range targets {
		if err := CheckTarget(target); err != nil {
			t.Errorf("CheckTarget(%d): got %v, want nil", target, err)
		}

		s, err := NewSettings(target, false, [32]byte{}, idKey)
		if err != nil {
			t.Fatalf("NewSettings(%d): got %v, want nil", target, err)
		}

		if s.Target() != target || s.Min() != target/4 || s.Max() != 4*target {
			t.Errorf("Target %d: got Target %d, Min %d, Max %d, want %d, %d, %d",
				target, s.Target(), s.Min(), s.Max(), target, target/4, 4*target)
		}

		// 64 KiB is 2^16, each profile one bit more.
		before, after := params.Bits(s)
		if tt := 16 + i; before != tt+1 || after != tt-3 {
			t.Errorf("Target %d = 2^%d: got Bits %d, %d, want %d, %d", target, tt, before, after, tt+1, tt-3)
		}

		if prev, ok := seen[[2]int{before, after}]; ok {
			t.Errorf("Targets %d and %d share the bit counts %d, %d", prev, target, before, after)
		}
		seen[[2]int{before, after}] = target
	}

	for _, target := range []int{48 << 10, 96 << 10, 32 << 10, 8 << 20, 0, -1, 64<<10 + 1, 3 << 20, 1 << 30} {
		if err := CheckTarget(target); !errors.Is(err, carver.ErrInvalidSettings) {
			t.Errorf("CheckTarget(%d): got %v, want ErrInvalidSettings", target, err)
		}

		s, err := NewSettings(target, false, [32]byte{}, idKey)
		if !errors.Is(err, carver.ErrInvalidSettings) || s != (carver.Settings{}) {
			t.Errorf("NewSettings(%d): got %+v, %v, want the zero value and ErrInvalidSettings", target, s, err)
		}
	}
}

// The key refusals of RFC 2 §3.7 hold at every Target, and an accepted value
// cuts under the table its encryption flag picks: the unkeyed one of the
// committed vectors without encryption, the keyed one under the vectors'
// chunking key with it. The vectors were computed by BLAKE3's reference
// binding from RFC 2 §3.2's text, so a wrong context string, mode or byte
// order fails here.
func TestKeys(t *testing.T) {
	v := carvetest.LoadVectors(t)

	for _, target := range targets {
		for _, tc := range []struct {
			name         string
			encrypting   bool
			chunking, id [32]byte
			refused      bool
			want         *[256]uint64
		}{
			{name: "a zero chunk-ID key", refused: true},
			{name: "a zero chunk-ID key, encrypting", encrypting: true, chunking: v.ChunkingKey, refused: true},
			{name: "a chunking key without encryption", chunking: v.ChunkingKey, id: idKey, refused: true},
			{name: "encryption with a zero chunking key", encrypting: true, id: idKey, refused: true},
			{name: "no encryption, a zero chunking key", id: idKey, want: &v.GearUnkeyed},
			{name: "encryption with a chunking key", encrypting: true, chunking: v.ChunkingKey, id: idKey, want: &v.GearKeyed},
		} {
			s, err := NewSettings(target, tc.encrypting, tc.chunking, tc.id)
			if tc.refused {
				if !errors.Is(err, carver.ErrInvalidSettings) || s != (carver.Settings{}) {
					t.Errorf("Target %d, %s: got %+v, %v, want the zero value and ErrInvalidSettings", target, tc.name, s, err)
				}
				continue
			}

			if err != nil {
				t.Fatalf("Target %d, %s: got %v, want nil", target, tc.name, err)
			}

			g := params.Gear(s)
			if g == nil {
				t.Fatalf("Target %d, %s: got a nil table", target, tc.name)
			}
			for i := range g {
				if g[i] != tc.want[i] {
					t.Errorf("Target %d, %s: table word %d is %d, the vectors hold %d", target, tc.name, i, g[i], tc.want[i])
					break
				}
			}
		}
	}
}

// Building settings starts no goroutine, keyed or unkeyed. The count is
// process-wide, so this test never runs in parallel, and no test in this
// binary may leave a goroutine running that starts others.
func TestNewSettingsStartsNoGoroutine(t *testing.T) {
	sample := []metrics.Sample{{Name: "/sched/goroutines-created:goroutines"}}
	created := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}

	runtime.GC() // the collector's workers start on its first cycle, not ours
	for _, tc := range []struct {
		name       string
		encrypting bool
		chunking   [32]byte
	}{{"unkeyed", false, [32]byte{}}, {"keyed", true, cutKey}} {
		before := created()
		for range 100 {
			if _, err := NewSettings(1<<20, tc.encrypting, tc.chunking, idKey); err != nil {
				t.Fatalf("%s NewSettings(1 MiB): got %v, want nil", tc.name, err)
			}
		}
		if n := created() - before; n != 0 {
			t.Errorf("100 %s NewSettings: %d goroutines created, want 0", tc.name, n)
		}
	}
}
