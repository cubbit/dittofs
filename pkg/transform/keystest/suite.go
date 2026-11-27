package keystest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
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
// second Give of one. The salvage-key wrap is part of it, so the salvage row
// reaches every provider.
type Harness interface {
	keys.Provider
	keys.SalvageWrapper

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
	{"owned", owned},
	{"calls", calls},
	{"salvage", salvage},
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

// The caller owns the slice Get returns (keys.Provider.Get): a change to it
// never reaches the provider, and a later Replace or Destroy never changes it.
// Catches a provider that returns the slice it holds, and one that wipes the
// slices it handed out.
func owned(ctx context.Context, h Harness) error {
	id := h.Give(namespace(1), keys.KindDataKey, "owned", materialA)

	got, err := h.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("Get(%q): %w, want its content", id.ID, err)
	}

	got[0] ^= 0xff

	if err := served(ctx, h, id, materialA); err != nil {
		return fmt.Errorf("after the caller changed the slice Get returned: %w", err)
	}

	held, err := h.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("Get(%q): %w, want its content", id.ID, err)
	}

	h.Replace(id, materialB)
	h.Destroy(id)

	if !bytes.Equal(held, materialA) {
		return fmt.Errorf("a slice Get returned = %x after a Replace and a Destroy, want %x unchanged", held, materialA)
	}

	return nil
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

// A salvage key is a fresh 32-byte key for one export, returned with its wrap
// in RFC 5 Appendix B.1's stored-wrap layout, and belongs to no namespace
// (Appendix B.2): two calls for one export give two keys and two wraps, each
// wrap parses with nothing left over and holds its key neither raw, in hex
// nor in base64, and Current answers as before in the row's namespace, the
// zero one and one never given, for every kind. The wrap bytes and their
// unwrap are each provider's: the row checks neither that a wrap opens under
// the current master key nor that it binds its export, so a provider's own
// tests must. Catches a reused key, a key returned in the clear as its own
// wrap, and a salvage key written as a namespace's material.
func salvage(ctx context.Context, h Harness) error {
	ns := namespace(1)
	h.Give(ns, keys.KindDataKey, "given", materialA)
	export := []byte("salvage export")

	type answer struct {
		id keys.MaterialID
		ok bool
	}

	type asked struct {
		ns   entity.NamespaceID
		kind string
	}

	currents := func() map[asked]answer {
		m := make(map[asked]answer)
		for _, n := range []entity.NamespaceID{ns, {}, namespace(9)} {
			for _, kind := range []string{keys.KindDataKey, keys.KindChunkIDKey, keys.KindChunkingKey} {
				id, err := h.Current(ctx, n, kind)
				m[asked{n, kind}] = answer{id, err == nil}
			}
		}

		return m
	}
	before := currents()

	var got [2]struct{ key, wrap []byte }

	for i := range got {
		key, wrap, err := h.WrapSalvageKey(ctx, export)
		if err != nil {
			return fmt.Errorf("WrapSalvageKey(%q): %w", export, err)
		}

		if len(key) != 32 {
			return fmt.Errorf("WrapSalvageKey(%q) returned a %d-byte key, want 32", export, len(key))
		}

		if _, _, _, err := parseStoredWrap(wrap); err != nil {
			return fmt.Errorf("WrapSalvageKey(%q) wrap %x: %w", export, wrap, err)
		}

		if form := holds(wrap, key); form != "" {
			return fmt.Errorf("WrapSalvageKey(%q) wrap %x holds its key %x %s", export, wrap, key, form)
		}

		got[i].key, got[i].wrap = key, wrap
	}

	if bytes.Equal(got[0].key, got[1].key) {
		return fmt.Errorf("two WrapSalvageKey(%q) calls returned one key %x, want two", export, got[0].key)
	}

	if bytes.Equal(got[0].wrap, got[1].wrap) {
		return fmt.Errorf("two WrapSalvageKey(%q) calls returned one wrap %x, want two", export, got[0].wrap)
	}

	for q, now := range currents() {
		if was := before[q]; now != was {
			return fmt.Errorf("Current(namespace %s, %s) = %q (ok %t) after WrapSalvageKey, was %q (ok %t)", q.ns, q.kind, now.id.ID, now.ok, was.id.ID, was.ok)
		}
	}

	return nil
}

// parseStoredWrap splits wrap as RFC 5 Appendix B.1's stored wrap: provider
// kind (0x01 key file, 0x02 key service), master-key ID length (2 bytes,
// big-endian), master-key ID, wrap length (4 bytes, big-endian), wrap bytes.
// The master-key ID and the wrap bytes must be non-empty, and the lengths must
// account for every byte.
func parseStoredWrap(wrap []byte) (kind byte, masterID, wrapped []byte, err error) {
	if len(wrap) < 3 {
		return 0, nil, nil, fmt.Errorf("%d bytes, shorter than a provider kind and a master-key ID length", len(wrap))
	}

	kind, n, rest := wrap[0], int(binary.BigEndian.Uint16(wrap[1:3])), wrap[3:]
	if kind != 0x01 && kind != 0x02 {
		return 0, nil, nil, fmt.Errorf("provider kind %#02x, want 0x01 or 0x02", kind)
	}

	if n == 0 {
		return 0, nil, nil, errors.New("empty master-key ID")
	}

	if len(rest) < n+4 {
		return 0, nil, nil, fmt.Errorf("master-key ID length %d and a wrap length need %d bytes, %d follow", n, n+4, len(rest))
	}

	masterID, rest = rest[:n], rest[n:]
	m, rest := int(binary.BigEndian.Uint32(rest)), rest[4:]

	switch {
	case m == 0:
		return 0, nil, nil, errors.New("empty wrap bytes")
	case m != len(rest):
		return 0, nil, nil, fmt.Errorf("wrap length %d, but %d bytes follow", m, len(rest))
	}

	return kind, masterID, rest, nil
}

// holds reports in which form wrap contains key, raw, in hex or in base64,
// or "" for none.
func holds(wrap, key []byte) string {
	for _, f := range []struct {
		name string
		form []byte
	}{
		{"raw", key},
		{"in hex", []byte(hex.EncodeToString(key))},
		{"in base64", []byte(base64.RawStdEncoding.EncodeToString(key))},
	} {
		if bytes.Contains(wrap, f.form) {
			return f.name
		}
	}

	return ""
}
