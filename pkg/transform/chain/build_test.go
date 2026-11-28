package chain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// logLine is one line a JSON slog handler wrote.
type logLine struct {
	Level     string `json:"level"`
	Transform string `json:"transform"`
	Reason    string `json:"reason"`
}

// newChain builds cfg over r with deps whose Logger writes JSON lines, and
// returns the chain, the lines logged and the error.
func newChain(t *testing.T, r chain.Registry, cfg []transform.Config, require ...string) (transform.Chain, []logLine, error) {
	t.Helper()
	var buf bytes.Buffer
	c, err := r.NewChain(context.Background(), cfg, keystest.NewMemory(), chain.Deps{
		Logger:   slog.New(slog.NewJSONHandler(&buf, nil)),
		Recorder: sink{new([]string)},
		Require:  require,
		Encrypts: func(entity.NamespaceID) bool { return false },
	})
	var lines []logLine
	for dec := json.NewDecoder(&buf); dec.More(); {
		var l logLine
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("decoding a log line: %v", err)
		}
		lines = append(lines, l)
	}
	return c, lines, err
}

// stages registers one identity transform per stage, c, e and r, and a second
// compressor, c2.
func stages() chain.Registry {
	r := chain.NewRegistry()
	r.Register(idA, "c", pad{id: idA, stage: transform.StageCompress}.factory())
	r.Register(idB, "e", pad{id: idB, stage: transform.StageEncrypt}.factory())
	r.Register(idC, "r", pad{id: idC, stage: transform.StageRedundancy, half: true}.factory())
	r.Register(idC+1, "c2", pad{id: idC + 1, stage: transform.StageCompress}.factory())
	return r
}

func names(ns ...string) []transform.Config {
	cfg := make([]transform.Config, len(ns))
	for i, n := range ns {
		cfg[i] = transform.Config{Name: n}
	}
	return cfg
}

// wantRefused asserts NewChain refused with no chain and logged exactly one
// Error line naming transform, whose reason holds every one of reason.
func wantRefused(t *testing.T, what string, c transform.Chain, err error, lines []logLine, transform string, reason ...string) {
	t.Helper()
	if err == nil || c != nil {
		t.Fatalf("NewChain(%s) = %v, %v, want no chain and an error", what, c, err)
	}
	if len(lines) != 1 {
		t.Fatalf("NewChain(%s) logged %d lines (%+v), want 1", what, len(lines), lines)
	}
	if l := lines[0]; l.Level != "ERROR" || l.Transform != transform || l.Reason != err.Error() {
		t.Errorf("NewChain(%s) logged %+v, want level ERROR, transform %q and reason %q", what, l, transform, err)
	}
	for _, w := range reason {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("NewChain(%s): err = %q, want it to name %q", what, err, w)
		}
	}
}

// A chain is at most one transform per stage, listed in stage order; anything
// else is refused as written, never reordered into a chain that builds.
func TestNewChainStageRules(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name   string
		list   []string
		at     string
		reason []string
	}{
		{"out of stage order", []string{"e", "c"}, "c", []string{`"c"`, `"e"`, "out of stage order"}},
		{"redundancy before encrypt", []string{"c", "r", "e"}, "e", []string{`"e"`, `"r"`, "out of stage order"}},
		{"two in one stage", []string{"c", "c2"}, "c2", []string{`"c"`, `"c2"`, "both of stage 1"}},
		{"one twice", []string{"e", "e"}, "e", []string{`"e"`, "twice"}},
		{"one twice, apart", []string{"e", "r", "e"}, "e", []string{`"e"`, "twice"}},
		{"unknown name", []string{"c", "zstd"}, "zstd", []string{`"zstd"`, "not registered"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, lines, err := newChain(t, stages(), names(tc.list...))
			wantRefused(t, strings.Join(tc.list, ", "), c, err, lines, tc.at, tc.reason...)
		})
	}
	for _, list := range [][]string{nil, {"c"}, {"e", "r"}, {"c", "r"}, {"c2", "r"}} {
		t.Run("builds "+strings.Join(list, ", "), func(t *testing.T) {
			c, lines, err := newChain(t, stages(), names(list...))
			if err != nil || c == nil {
				t.Fatalf("NewChain(%q) = %v, %v, want a chain", list, c, err)
			}
			if len(lines) != 0 {
				t.Errorf("NewChain(%q) logged %+v, want nothing", list, lines)
			}
			// The bound is the registry's, whatever is configured: c, e and
			// c2 add nothing and r adds half, plus the 8-byte envelope.
			if got := c.MaxDecodeLen(1000); got != 1500+8 {
				t.Errorf("NewChain(%q).MaxDecodeLen(1000) = %d, want %d", list, got, 1500+8)
			}
		})
	}
}

// Compression beside encryption leaks plaintext through sizes, so a chain of
// both builds only when the compress stage's settings accept it.
func TestNewChainWithEncryption(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name     string
		settings map[string]any
		built    bool
	}{
		{"no settings", nil, false},
		{"another setting", map[string]any{"level": 3}, false},
		{"with_encryption: yes", map[string]any{"with_encryption": "yes"}, false},
		{"with_encryption: true", map[string]any{"with_encryption": true}, false},
		{"with_encryption: accept", map[string]any{"with_encryption": "accept"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := []transform.Config{{Name: "c", Settings: tc.settings}, {Name: "e"}, {Name: "r"}}
			c, lines, err := newChain(t, stages(), cfg)
			if !tc.built {
				wantRefused(t, "c with "+tc.name+", e, r", c, err, lines, "c", `"c"`, `"e"`, "with_encryption: accept")
				return
			}
			if err != nil || c == nil || len(lines) != 0 {
				t.Errorf("NewChain(c with %s, e, r) = %v, %v, logging %+v, want a chain and no line", tc.name, c, err, lines)
			}
		})
	}
	// Without an encrypt stage, a compressor needs no such setting.
	if c, _, err := newChain(t, stages(), names("c", "r")); err != nil || c == nil {
		t.Errorf("NewChain(c, r) = %v, %v, want a chain", c, err)
	}
}

// A configured transform that cannot be built fails the chain, which is never
// built without it, although its decoder, built with empty settings, is fine;
// so does one whose settings build another transform than its decoder, and a
// required name that is not registered.
func TestNewChainRefusesTransformsThatCannotStart(t *testing.T) {
	prof.Watch(t, prof.Options{})
	boom := errors.New("factory fault")
	enc := pad{id: idB, stage: transform.StageEncrypt}
	for _, tc := range []struct {
		name   string
		fault  transform.Factory
		wrap   error
		reason []string
	}{
		{"failing factory", failing(boom), boom, []string{`"e"`, boom.Error()}},
		{"no transform", nilTransform, nil, []string{`"e"`, "no transform"}},
		{"another stage", pad{id: idB, stage: transform.StageRedundancy}.factory(), nil, []string{`"e"`, "stage 3", "stage 2"}},
		{"another ID", pad{id: idA, stage: transform.StageEncrypt}.factory(), nil, []string{`"e"`, "0x8001", "0x8002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			r.Register(idB, "e", onSetting("fault", enc.factory(), tc.fault))
			r.Register(idC, "r", pad{id: idC, stage: transform.StageRedundancy}.factory())
			cfg := []transform.Config{{Name: "e", Settings: map[string]any{"fault": true}}, {Name: "r"}}
			c, lines, err := newChain(t, r, cfg)
			wantRefused(t, "e faulting, r", c, err, lines, "e", tc.reason...)
			if tc.wrap != nil && !errors.Is(err, tc.wrap) {
				t.Errorf("NewChain(e faulting, r): err = %v, want it to wrap the factory's error", err)
			}
		})
	}
	t.Run("unregistered require", func(t *testing.T) {
		c, lines, err := newChain(t, stages(), names("e"), "e", "parity")
		wantRefused(t, "e, require e and parity", c, err, lines, "parity", `"parity"`, "not registered")
	})
	t.Run("registered require", func(t *testing.T) {
		if c, _, err := newChain(t, stages(), names("e"), "e", "r"); err != nil || c == nil {
			t.Errorf("NewChain(e, require e and r) = %v, %v, want a chain", c, err)
		}
	})
}

// A registered transform that fails to build for decoding fails every chain,
// and the line names it.
func TestNewChainLogsDecodeSetRefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := stages()
	r.Register(idC+2, "broken", failing(errors.New("factory fault")))
	c, lines, err := newChain(t, r, names("c"))
	wantRefused(t, "c, beside a broken registration", c, err, lines, "broken", `"broken"`, "factory fault")
}
