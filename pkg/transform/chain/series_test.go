package chain_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// seriesRegistry registers an identity compressor, a compressor declining
// every chunk, the census tests' sealer, a redundancy step adding half, and a
// compressor no chain configures.
func seriesRegistry() chain.Registry {
	r := chain.NewRegistry()
	r.Register(idA, "plain", pad{id: idA, stage: transform.StageCompress}.factory())
	r.Register(idD, "skip", decline{id: idD, stage: transform.StageCompress}.factory())
	r.Register(idB, "seal", sealer.factory())
	r.Register(idC, "parity", pad{id: idC, stage: transform.StageRedundancy, half: true}.factory())
	r.Register(idD+1, "spare", pad{id: idD + 1, stage: transform.StageCompress}.factory())
	return r
}

// buildInto builds cfg over r and p recording into rec.
func buildInto(t *testing.T, r chain.Registry, p keys.Provider, rec chain.Recorder, cfg ...string) transform.Chain {
	t.Helper()
	list := names(cfg...)
	for i := range list {
		list[i].Settings = map[string]any{"with_encryption": "accept"}
	}
	c, err := r.NewChain(context.Background(), list, p, chain.Deps{
		Logger:   slog.New(slog.DiscardHandler),
		Recorder: rec,
		Encrypts: func(entity.NamespaceID) bool { return false },
	})
	if err != nil {
		t.Fatalf("NewChain(%v): %v", cfg, err)
	}
	return c
}

// tally counts a sink's lines with their byte counts dropped, so a line
// names the transform, the call and its label alone.
func tally(lines []string) map[string]int {
	out := make(map[string]int)
	for _, l := range lines {
		f := strings.Fields(l)
		switch {
		case len(f) > 2 && f[1] == "encoded":
			f = f[:3]
		case len(f) > 1 && f[1] == "decoded":
			f = f[:2]
		}
		out[strings.Join(f, " ")]++
	}
	return out
}

// TestSeriesCountEveryCall holds RFC 5 §6's four series, recorded by the
// chain around each transform's call and labelled by its registered name:
// after a scripted run of encodes, declines, decodes and each decode failure,
// every transform and label holds exactly its count. A failure the envelope
// refuses lands under envelope, so an alert on malformed sees a planted
// envelope; unknown material lands under material_unknown, not
// material_unavailable; and a transform no chain configures is resolved too,
// since a body may still name it. Every series is resolved when the chain is
// built, none on a call.
func TestSeriesCountEveryCall(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := seriesRegistry()
	writer := keystest.NewMemory()
	writer.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	destroyed := keystest.NewMemory()
	destroyed.Destroy(destroyed.Give(ns, keys.KindDataKey, "d1", span(0, 32)))

	rec := newSink()
	full := buildInto(t, r, writer, rec, "plain", "seal", "parity")
	declining := buildInto(t, r, writer, rec, "skip", "seal")
	unknown := buildInto(t, r, keystest.NewMemory(), rec, "plain", "seal", "parity")
	gone := buildInto(t, r, destroyed, rec, "plain", "seal", "parity")
	down := buildInto(t, r, failingGet{writer, errors.New("the key service hung up")}, rec, "plain", "seal", "parity")
	resolved := len(rec.lines())

	encode := func(c transform.Chain) ([]byte, []transform.Encoded) {
		t.Helper()
		a, err := c.Pin(ctx, ns)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		body, encs, err := a.Encode(ctx, nil, hash, []byte("chunk"))
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return body, encs
	}
	decode := func(c transform.Chain, body []byte, limit int, encs []transform.Encoded, want error) {
		t.Helper()
		if _, err := c.Decode(ctx, ns, nil, hash, body, limit, encs); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Fatalf("Decode = %v, want %v", err, want)
		}
	}

	body, encs := encode(full)
	encode(full)
	declined, declinedEncs := encode(declining)

	decode(full, body, chunkMax, encs, nil)
	decode(declining, declined, chunkMax, declinedEncs, nil)
	decode(full, []byte{2, 0, 'x'}, chunkMax, nil, transform.ErrMalformed)
	decode(full, append(chain.AppendEnvelope(nil, nil), make([]byte, chunkMax+1)...), chunkMax, nil, transform.ErrTooLarge)
	decode(full, body, chunkMax, nil, transform.ErrMalformed)
	decode(unknown, body, chunkMax, encs, keys.ErrUnknownMaterial)
	decode(gone, body, chunkMax, encs, keys.ErrMaterialDestroyed)
	decode(down, body, chunkMax, encs, keys.ErrMaterialUnavailable)
	decode(full, body, 1, encs, transform.ErrTooLarge)

	got := rec.lines()
	for _, l := range got[resolved:] {
		if strings.HasPrefix(l, "for ") {
			t.Errorf("a call on a built chain resolved a series: %q", l)
		}
	}
	// Five chains, each resolving every registered transform and envelope.
	want := map[string]int{
		"for plain": 5, "for skip": 5, "for seal": 5, "for parity": 5, "for spare": 5, "for envelope": 5,

		"plain encoded true":  2,
		"seal encoded true":   3,
		"parity encoded true": 2,
		"skip encoded false":  1,

		"parity decoded": 6, // the full chain's read, then each of the four reads that fail beneath it
		"seal decoded":   2,
		"plain decoded":  1,

		"envelope decode failed malformed":        1,
		"envelope decode failed too_large":        1,
		"seal decode failed malformed":            1,
		"seal decode failed material_unknown":     1,
		"seal decode failed material_destroyed":   1,
		"seal decode failed material_unavailable": 1,
		"seal decode failed too_large":            1, // its 5 bytes past a limit of 1
	}
	g := tally(got)
	for _, k := range slices.Sorted(maps.Keys(g)) {
		if _, ok := want[k]; !ok {
			t.Errorf("series %q holds %d, want none", k, g[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if g[k] != want[k] {
			t.Errorf("series %q holds %d, want %d", k, g[k], want[k])
		}
	}
	// A declined chunk passes on unchanged, so its bytes out are its bytes in.
	for _, line := range []string{"plain encoded true 5 5", "skip encoded false 5 5", "plain decoded 5 5"} {
		if !slices.Contains(got, line) {
			t.Errorf("no %q among %q", line, got)
		}
	}
}

// TestNewChainRefusesARecorderGivingNone: a Recorder whose For gives no
// recorder would panic on the first chunk, so NewChain refuses it at build,
// naming the transform.
func TestNewChainRefusesARecorderGivingNone(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, name := range []string{"spare", "envelope"} {
		_, err := seriesRegistry().NewChain(context.Background(), names("plain"), keystest.NewMemory(), chain.Deps{
			Logger:   slog.New(slog.DiscardHandler),
			Recorder: noneFor{newSink(), name},
			Encrypts: func(entity.NamespaceID) bool { return false },
		})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("NewChain with no recorder for %s: err = %v, want a refusal naming it", name, err)
		}
	}
}

// noneFor is a sink that gives no recorder for one name.
type noneFor struct {
	*sink
	name string
}

func (n noneFor) For(name string) chain.TransformRecorder {
	if name == n.name {
		return nil
	}
	return n.sink.For(name)
}
