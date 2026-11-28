package chain

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Register makes a transform available for configuration, by name, and for
// decoding, by ID (RFC 5 §2.5, §3.1). It is called at init; a duplicate ID
// panics, since an ID is assigned once and never reused (RFC 5 §4), and so does
// a duplicate name, since a configuration names its transform by name, and
// either of the names the chain reserves, envelope and unregistered. A custom
// transform takes its ID from 0x8000–0xFFFF, a shipped one from 0x0000–0x7FFF
// (RFC 5 §4 item 1); the registry sees no package, so review holds the ranges.
func Register(id transform.ID, name string, f transform.Factory) {
	process.register(id, name, f)
}

// process is the registry every compiled-in transform registers in at init.
// A test builds a registry of its own instead.
var process = newRegistry()

// decision: the names envelope and unregistered are taken from the start, so
// no transform can register either. A refused envelope belongs to no
// transform, so its decode failure is recorded under envelope, and an alert on
// transform_decode_failures_total{error="malformed"} sees a planted envelope;
// unregistered labels a block written by a transform since dropped from the
// build. A transform registering either would share its series with them.
const (
	nameEnvelope     = "envelope"
	nameUnregistered = "unregistered"
)

var reserved = []string{nameEnvelope, nameUnregistered}

// registered is one Register call.
type registered struct {
	name    string
	factory transform.Factory
}

// registry maps every registered transform's ID to its name and factory.
type registry struct {
	mu     sync.Mutex
	byID   map[transform.ID]registered
	byName map[string]transform.ID
}

func newRegistry() *registry {
	return &registry{byID: make(map[transform.ID]registered), byName: make(map[string]transform.ID)}
}

func (r *registry) register(id transform.ID, name string, f transform.Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f == nil || name == "" {
		panic(fmt.Sprintf("chain: transform %q (ID %#04x) registers without a name or a factory", name, id))
	}
	if slices.Contains(reserved, name) {
		panic(fmt.Sprintf("chain: transform %q (ID %#04x) takes a name the chain reserves", name, id))
	}
	if prev, ok := r.byID[id]; ok {
		panic(fmt.Sprintf("chain: transform %q takes ID %#04x, already registered by %q", name, id, prev.name))
	}
	if prev, ok := r.byName[name]; ok {
		panic(fmt.Sprintf("chain: transform ID %#04x takes name %q, already registered by ID %#04x", id, name, prev))
	}
	r.byID[id] = registered{name: name, factory: f}
	r.byName[name] = id
}

// decodeSet is every registered transform, built once for one store (RFC 5
// §2.5): Chain.Decode looks a body's transforms up here by the envelope's IDs,
// never in the configured chain.
type decodeSet struct {
	byID map[transform.ID]transform.Transform
	// stages groups the decoders by stage, StageCompress first, for
	// maxDecodeLen.
	stages [3][]transform.Transform
}

// build builds every registered transform with empty settings and the store's
// provider m, holding the registry's lock, so a factory must not call
// Register. It refuses a transform whose factory fails, whose ID is not the
// one it registered under (the envelope and the census would name the wrong
// transform), or whose stage is not one of the three (RFC 5 §2.3).
//
// decision: a registered transform that fails to build for decoding fails the
// whole build, so the store does not open, although no configuration uses it.
// A compiled-in transform's default build needs only the provider, so its
// failure is a fault the operator must see; building on without it would leave
// the bodies it wrote unreadable with no sign until a read. This requires that
// no transform's default build reach material. Withdraw it if a shipped
// transform's default build ever needs material a store that does not use the
// transform lacks.
func (r *registry) build(ctx context.Context, m keys.Provider) (*decodeSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &decodeSet{byID: make(map[transform.ID]transform.Transform, len(r.byID))}
	for _, id := range slices.Sorted(maps.Keys(r.byID)) {
		reg := r.byID[id]
		t, err := reg.factory(ctx, map[string]any{}, m)
		if err != nil {
			return nil, fmt.Errorf("chain: building transform %q for decoding: %w", reg.name, err)
		}
		if t == nil {
			return nil, fmt.Errorf("chain: transform %q's factory returned no transform", reg.name)
		}
		if got := t.ID(); got != id {
			return nil, fmt.Errorf("chain: transform %q registered as ID %#04x reports ID %#04x", reg.name, id, got)
		}
		st := t.Stage()
		if st < transform.StageCompress || st > transform.StageRedundancy {
			return nil, fmt.Errorf("chain: transform %q declares stage %d, not one of the three", reg.name, st)
		}
		s.byID[id] = t
		s.stages[st-transform.StageCompress] = append(s.stages[st-transform.StageCompress], t)
	}
	return s, nil
}

// envelopeMax is the envelope of a body through all three stages: its version
// and count bytes, then a 2-byte ID per transform (RFC 5 §2.4).
const envelopeMax = 2 + 2*3

// maxDecodeLen is the largest body any registered transforms could have
// written from n bytes, one per stage, under any material and settings, plus
// the envelope (RFC 5 §3.1). Each stage's bound is the larger of its input and
// its registered transforms' bounds of that input, so a stage that is absent or
// declines costs nothing. Which transform bounds a stage can change with its
// input, so every call takes each stage's maximum anew.
func (s *decodeSet) maxDecodeLen(n int) int {
	for _, stage := range s.stages {
		in := n
		for _, t := range stage {
			n = max(n, t.MaxEncodedLen(in, nil))
		}
	}
	return n + envelopeMax
}
