package keystest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// Harness is a provider under the suite: the provider, and the calls the
// suite sets it up and reads it through, as Memory's methods of the same names
// define them. A real provider is adapted to it over its own records, and an
// adapter that cannot set one up fails the test through t.Fatal on the t
// newHarness was given. A row gives each ID once, so an adapter may refuse a
// second Give of one.
type Harness interface {
	keys.Provider

	Give(ns entity.NamespaceID, kind, id string, material []byte) keys.MaterialID
	Replace(id keys.MaterialID, material []byte)
	Destroy(id keys.MaterialID)
	Calls() uint64
}

// Run holds the provider newHarness builds to RFC 5 §2.5, §2.7 and §3.1, one
// subtest per row, each on a fresh harness holding no material.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.check(t.Context(), newHarness(t)); err != nil {
				t.Error(err)
			}
		})
	}
}

// row is one check of the suite. It returns an error naming what it found
// wrong, or nil.
type row struct {
	name  string
	check func(ctx context.Context, h Harness) error
}

// rows is the suite, in the order Run runs it.
var rows = []row{
	{"namespaces", namespaces},
	{"kinds", kinds},
	{"current", currentIsLatest},
	{"unknown", unknown},
	{"replaced", replaced},
	{"destroyed", destroyed},
	{"calls", calls},
}

// Material contents the rows give. Each is distinct, so a provider serving
// the wrong one is seen.
var (
	materialA = bytes.Repeat([]byte{0xa1}, 32)
	materialB = bytes.Repeat([]byte{0xb2}, 32)
	materialC = bytes.Repeat([]byte{0xc3}, 32)
)

// namespace returns a namespace ID of its own for each n.
func namespace(n byte) entity.NamespaceID { return entity.NamespaceID{15: n} }

// served reports what Get of id returns, against the content want.
func served(ctx context.Context, h Harness, id keys.MaterialID, want []byte) error {
	got, err := h.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("Get(%s %q of namespace %s): %w, want its content", id.Kind, id.ID, id.Namespace, err)
	}

	if !bytes.Equal(got, want) {
		return fmt.Errorf("Get(%s %q of namespace %s) = %x, want %x", id.Kind, id.ID, id.Namespace, got, want)
	}

	return nil
}

// isCurrent reports whether Current of kind in ns names want and serves its
// content.
func isCurrent(ctx context.Context, h Harness, ns entity.NamespaceID, kind string, want keys.MaterialID, content []byte) error {
	got, err := h.Current(ctx, ns, kind)
	if err != nil {
		return fmt.Errorf("Current(namespace %s, %s): %w, want %q", ns, kind, err, want.ID)
	}

	if got != want {
		return fmt.Errorf("Current(namespace %s, %s) = %+v, want %+v", ns, kind, got, want)
	}

	return served(ctx, h, got, content)
}

// refused reports whether err is want.
func refused(call string, err, want error) error {
	if !errors.Is(err, want) {
		return fmt.Errorf("%s: got %v, want %v", call, err, want)
	}

	return nil
}

// Every call names its namespace (RFC 5 §2.5, §7 T14): two namespaces given a
// data key under one ID each serve their own, Current of one never names the
// other's, Get of an ID in a namespace never given it is ErrUnknownMaterial,
// and Current of a kind given only in another namespace fails. Catches a
// lookup by ID alone, and one that falls back to another namespace.
func namespaces(ctx context.Context, h Harness) error {
	nsA, nsB := namespace(1), namespace(2)

	a := h.Give(nsA, keys.KindDataKey, "shared", materialA)
	b := h.Give(nsB, keys.KindDataKey, "shared", materialB)
	onlyA := h.Give(nsA, keys.KindChunkIDKey, "only-a", materialC)

	for _, c := range []struct {
		got  keys.MaterialID
		ns   entity.NamespaceID
		kind string
		id   string
	}{
		{a, nsA, keys.KindDataKey, "shared"},
		{b, nsB, keys.KindDataKey, "shared"},
		{onlyA, nsA, keys.KindChunkIDKey, "only-a"},
	} {
		if c.got.Namespace != c.ns || c.got.Kind != c.kind || c.got.ID != c.id {
			return fmt.Errorf("Give(namespace %s, %s, %q) = %+v, want that namespace, kind and ID", c.ns, c.kind, c.id, c.got)
		}
	}

	if err := isCurrent(ctx, h, nsA, keys.KindDataKey, a, materialA); err != nil {
		return err
	}

	if err := isCurrent(ctx, h, nsB, keys.KindDataKey, b, materialB); err != nil {
		return err
	}

	_, err := h.Get(ctx, keys.MaterialID{Namespace: nsB, Kind: keys.KindChunkIDKey, ID: "only-a", Fingerprint: onlyA.Fingerprint})
	if err := refused(fmt.Sprintf("Get of namespace %s's ID %q in namespace %s", nsA, "only-a", nsB), err, keys.ErrUnknownMaterial); err != nil {
		return err
	}

	// Which error is the unknown row's to check; this row checks only that
	// nothing of namespace A is served.
	if got, err := h.Current(ctx, nsB, keys.KindChunkIDKey); err == nil {
		return fmt.Errorf("Current of %s in namespace %s, given only in namespace %s, = %+v, want an error", keys.KindChunkIDKey, nsB, nsA, got)
	}

	return nil
}

// A kind is specific to what consumes it, so an ID given as one kind is
// ErrUnknownMaterial looked up as another (RFC 5 §2.5). Catches a lookup that
// ignores the kind.
func kinds(ctx context.Context, h Harness) error {
	ns := namespace(1)
	id := h.Give(ns, keys.KindDataKey, "data", materialA)

	asOther := id
	asOther.Kind = keys.KindChunkIDKey
	_, err := h.Get(ctx, asOther)

	return refused(fmt.Sprintf("Get of %s %q as %s", keys.KindDataKey, id.ID, keys.KindChunkIDKey), err, keys.ErrUnknownMaterial)
}

// Current is the material new bodies of a kind use in a namespace (RFC 5
// §3.1): a second Give of a kind becomes current while the first still reads,
// and a Give of another kind leaves it current. Catches a provider serving
// only its first material, or only its latest.
func currentIsLatest(ctx context.Context, h Harness) error {
	ns := namespace(1)

	first := h.Give(ns, keys.KindDataKey, "first", materialA)
	if err := isCurrent(ctx, h, ns, keys.KindDataKey, first, materialA); err != nil {
		return err
	}

	second := h.Give(ns, keys.KindDataKey, "second", materialB)
	h.Give(ns, keys.KindChunkIDKey, "other-kind", materialC)

	if err := isCurrent(ctx, h, ns, keys.KindDataKey, second, materialB); err != nil {
		return err
	}

	return served(ctx, h, first, materialA)
}

// Material never given is ErrUnknownMaterial, from Get and from Current alike
// (RFC 5 §2.7, §3.1): an ID never given, and a kind never given in a
// namespace holding another. Catches an absence
// reported as an outage.
func unknown(ctx context.Context, h Harness) error {
	ns := namespace(1)
	given := h.Give(ns, keys.KindDataKey, "given", materialA)

	_, err := h.Get(ctx, keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "never-given", Fingerprint: given.Fingerprint})
	if err := refused(`Get of ID "never-given"`, err, keys.ErrUnknownMaterial); err != nil {
		return err
	}

	_, err = h.Current(ctx, ns, keys.KindChunkingKey)

	return refused(fmt.Sprintf("Current of %s, never given in namespace %s", keys.KindChunkingKey, ns), err, keys.ErrUnknownMaterial)
}

// Material whose content no longer matches its recorded fingerprint is
// refused as ErrMaterialUnavailable, by Get and by Current (RFC 5 §2.5, §2.7,
// §7 T12), even when the caller names the new content's fingerprint. Catches
// a provider that re-fingerprints what it holds, and one that checks the
// content against the caller's fingerprint.
func replaced(ctx context.Context, h Harness) error {
	ns := namespace(1)
	id := h.Give(ns, keys.KindDataKey, "replaced", materialA)
	h.Replace(id, materialB)

	_, err := h.Get(ctx, id)
	if err := refused(`Get of replaced ID "replaced"`, err, keys.ErrMaterialUnavailable); err != nil {
		return err
	}

	asB := id
	asB.Fingerprint = h.Give(namespace(2), keys.KindChunkingKey, "probe", materialB).Fingerprint
	_, err = h.Get(ctx, asB)
	if err := refused(`Get of replaced ID "replaced" naming its new content's fingerprint`, err, keys.ErrMaterialUnavailable); err != nil {
		return err
	}

	_, err = h.Current(ctx, ns, keys.KindDataKey)

	return refused(`Current naming replaced ID "replaced"`, err, keys.ErrMaterialUnavailable)
}

// Material lost for good is ErrMaterialDestroyed, by Get and by Current, and
// other material of the namespace still reads (RFC 5 §2.7). Catches a
// destruction folded into an outage.
func destroyed(ctx context.Context, h Harness) error {
	ns := namespace(1)
	kept := h.Give(ns, keys.KindChunkIDKey, "kept", materialA)
	id := h.Give(ns, keys.KindDataKey, "destroyed", materialB)
	h.Destroy(id)

	_, err := h.Get(ctx, id)
	if err := refused(`Get of destroyed ID "destroyed"`, err, keys.ErrMaterialDestroyed); err != nil {
		return err
	}

	_, err = h.Current(ctx, ns, keys.KindDataKey)
	if err := refused(`Current naming destroyed ID "destroyed"`, err, keys.ErrMaterialDestroyed); err != nil {
		return err
	}

	return served(ctx, h, kept, materialA)
}

// Calls rises by exactly one for each Current and each Get, failing calls
// included, and not at all for Give, Replace or Destroy, so a test can assert
// the provider was asked nothing across its own setup (RFC 5 §8.2 T12, T14).
// Catches a count that skips errors, and one that counts setup.
func calls(ctx context.Context, h Harness) error {
	ns := namespace(1)
	never := keys.MaterialID{Namespace: ns, Kind: keys.KindDataKey, ID: "never-given"}

	before := h.Calls()
	id := h.Give(ns, keys.KindDataKey, "counted", materialA)
	gone := h.Give(ns, keys.KindChunkIDKey, "gone", materialB)
	h.Replace(gone, materialC)
	h.Destroy(gone)

	if got := int64(h.Calls()) - int64(before); got != 0 {
		return fmt.Errorf("Calls rose by %d over a Give, a Replace and a Destroy, want 0", got)
	}

	for _, c := range []struct {
		name string
		fail bool
		call func() error
	}{
		{"a Current that succeeds", false, func() error { _, err := h.Current(ctx, ns, keys.KindDataKey); return err }},
		{"a Get that succeeds", false, func() error { _, err := h.Get(ctx, id); return err }},
		{"a Current that fails", true, func() error { _, err := h.Current(ctx, ns, keys.KindChunkingKey); return err }},
		{"a Get that fails", true, func() error { _, err := h.Get(ctx, never); return err }},
	} {
		before := h.Calls()
		err := c.call()

		if (err != nil) != c.fail {
			return fmt.Errorf("%s: got error %v", c.name, err)
		}

		if got := int64(h.Calls()) - int64(before); got != 1 {
			return fmt.Errorf("Calls rose by %d over %s, want 1", got, c.name)
		}
	}

	return nil
}
