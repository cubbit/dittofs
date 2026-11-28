package chain_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// panicOf runs f and returns what it panicked with, as text, or "" if it
// returned.
func panicOf(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}

// An ID is assigned once, and a configuration names a transform by name, so a
// second registration of either panics naming both parties; so does a name the
// chain keeps for its own series.
func TestRegisterRefusesDuplicates(t *testing.T) {
	prof.Watch(t, prof.Options{})
	first := pad{id: idA, stage: transform.StageCompress}.factory()
	for _, tc := range []struct {
		name  string
		id    transform.ID
		as    string
		wants []string
	}{
		{"an ID twice", idA, "second", []string{`"first"`, `"second"`, "0x8001"}},
		{"a name twice", idB, "first", []string{`"first"`, "0x8001", "0x8002"}},
		{"envelope", idB, "envelope", []string{`"envelope"`, "reserves"}},
		{"unregistered", idB, "unregistered", []string{`"unregistered"`, "reserves"}},
		{"no name", idB, "", []string{"0x8002", "without a name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			r.Register(idA, "first", first)
			msg := panicOf(func() { r.Register(tc.id, tc.as, pad{id: tc.id, stage: transform.StageCompress}.factory()) })
			if msg == "" {
				t.Fatalf("Register(%#04x, %q) after Register(0x8001, \"first\") returned, want a panic", tc.id, tc.as)
			}
			for _, w := range tc.wants {
				if !strings.Contains(msg, w) {
					t.Errorf("Register(%#04x, %q) panic = %q, want it to name %s", tc.id, tc.as, msg, w)
				}
			}
		})
	}
}

// A factory is called only at a store's open, so a nil one panics at Register,
// at init, rather than there.
func TestRegisterRefusesNilFactory(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := chain.NewRegistry()
	msg := panicOf(func() { r.Register(idA, "first", nil) })
	if !strings.Contains(msg, `"first"`) || !strings.Contains(msg, "factory") {
		t.Errorf("Register(0x8001, \"first\", nil) panic = %q, want one naming \"first\" and its factory", msg)
	}
}

// A registered transform the decode set cannot use fails the build, although
// no configuration names it: one outside the three stages, one reporting
// another ID than it registered under, which the envelope and the census would
// then misname, and one whose default factory fails.
func TestDecodeSetBuildRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})
	boom := errors.New("factory fault")
	for _, tc := range []struct {
		name string
		f    transform.Factory
		want string
	}{
		{"stage 0", pad{id: idB, stage: 0}.factory(), "stage 0"},
		{"stage 4", pad{id: idB, stage: 4}.factory(), "stage 4"},
		{"another ID", pad{id: idC, stage: transform.StageEncrypt}.factory(), "0x8003"},
		{"failing factory", failing(boom), boom.Error()},
		{"no transform", nilTransform, "no transform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			r.Register(idA, "good", pad{id: idA, stage: transform.StageCompress}.factory())
			r.Register(idB, "bad", tc.f)
			_, err := r.MaxDecodeLen(context.Background(), keystest.NewMemory(), 100)
			if err == nil {
				t.Fatalf("decode set with %s: err = nil, want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), `"bad"`) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("decode set with %s: err = %q, want it to name \"bad\" and %q", tc.name, err, tc.want)
			}
		})
	}
	r := chain.NewRegistry()
	r.Register(idA, "bad", failing(boom))
	if _, err := r.MaxDecodeLen(context.Background(), keystest.NewMemory(), 100); !errors.Is(err, boom) {
		t.Errorf("decode set with a failing factory: err = %v, want it to wrap the factory's error", err)
	}
}

// A transform's default build reaches no material, so one declaring a data-key
// kind builds against a provider that holds no data key, and the build asks
// the provider nothing.
func TestDecodeSetBuildReachesNoMaterial(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := chain.NewRegistry()
	r.Register(idA, "keyed", pad{id: idA, stage: transform.StageEncrypt, kinds: []string{keys.KindDataKey}}.factory())
	m := keystest.NewMemory()
	if _, err := r.MaxDecodeLen(context.Background(), m, 100); err != nil {
		t.Fatalf("decode set with a data-key transform and no data key: err = %v, want nil", err)
	}
	if c := m.Calls(); c != 0 {
		t.Errorf("decode set's build: provider calls = %d, want 0", c)
	}
}

// MaxDecodeLen composes every registered transform, configured or not, one per
// stage in stage order, plus the 8-byte envelope; a stage's bound is the
// larger of its transforms' bounds of that n, chosen anew for each n.
func TestMaxDecodeLen(t *testing.T) {
	prof.Watch(t, prof.Options{})
	half := func(id transform.ID, s transform.Stage) pad { return pad{id: id, stage: s, half: true} }
	for _, tc := range []struct {
		name string
		reg  []pad
		n    int
		want int
	}{
		{"nothing registered", nil, 1000, 1000 + 8},
		{"half, then identity", []pad{half(idA, transform.StageCompress), {id: idB, stage: transform.StageEncrypt}}, 1000, 1500 + 8},
		{"half in two stages", []pad{half(idA, transform.StageCompress), half(idB, transform.StageRedundancy)}, 1000, 2250 + 8},
		{"fixed, then half", []pad{{id: idA, stage: transform.StageCompress, fixed: 1000}, half(idB, transform.StageRedundancy)}, 1000, 3000 + 8},
		{"half, then fixed", []pad{half(idA, transform.StageCompress), {id: idB, stage: transform.StageRedundancy, fixed: 1000}}, 1000, 2500 + 8},
		{"one stage, small n", []pad{{id: idA, stage: transform.StageCompress, fixed: 1000}, half(idB, transform.StageCompress)}, 100, 1100 + 8},
		{"one stage, large n", []pad{{id: idA, stage: transform.StageCompress, fixed: 1000}, half(idB, transform.StageCompress)}, 10000, 15000 + 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			for i, p := range tc.reg {
				r.Register(p.id, fmt.Sprint("t", i), p.factory())
			}
			got, err := r.MaxDecodeLen(context.Background(), keystest.NewMemory(), tc.n)
			if err != nil {
				t.Fatalf("MaxDecodeLen(%d): err = %v", tc.n, err)
			}
			if got != tc.want {
				t.Errorf("MaxDecodeLen(%d) = %d, want %d", tc.n, got, tc.want)
			}
		})
	}
}

// One decode set answers each n from every stage's maximum at that n, so the
// transform that bounds a stage at a small n does not bound it at a large one.
func TestMaxDecodeLenChoosesPerCall(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := chain.NewRegistry()
	r.Register(idA, "fixed", pad{id: idA, stage: transform.StageCompress, fixed: 1000}.factory())
	r.Register(idB, "half", pad{id: idB, stage: transform.StageCompress, half: true}.factory())
	got, err := r.MaxDecodeLens(context.Background(), keystest.NewMemory(), 100, 10000, 100)
	if err != nil {
		t.Fatalf("MaxDecodeLens: err = %v", err)
	}
	if want := []int{1108, 15008, 1108}; !slices.Equal(got, want) {
		t.Errorf("MaxDecodeLen of 100, 10000, 100 over one decode set = %v, want %v", got, want)
	}
}
