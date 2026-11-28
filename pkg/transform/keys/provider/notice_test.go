package provider

import (
	"context"
	mrand "math/rand/v2"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// destroyedRow is the destroyed record of kind and id, keeping key's
// fingerprint, and the material ID the notice names it by.
func destroyedRow(p *Provider, ns entity.NamespaceID, kind, id string, key []byte) (Row, keys.MaterialID) {
	fp := fingerprint(key)
	return Row{Kind: kind, ID: id, State: stateDestroyed, Record: p.record(fp, nil, nil)},
		keys.MaterialID{Namespace: ns, Kind: kind, ID: id, Fingerprint: fp}
}

// destroyedSince is DestroyedSince of cursor, failing the test on an error.
func destroyedSince(t *testing.T, p *Provider, cursor uint64) ([]keys.MaterialID, uint64) {
	t.Helper()

	ids, next, err := p.DestroyedSince(context.Background(), cursor)
	if err != nil {
		t.Fatalf("DestroyedSince(%#x): %v", cursor, err)
	}
	return ids, next
}

// A cursor from a provider before a restart names, on the provider after it,
// every ID that one finds, the ones the old cursor had passed included: two
// records destroyed and reported, a cursor taken after the first, then a
// fresh provider drawing another incarnation from its own Deps.Rand, a third
// record destroyed while none ran, and an Open. A cursor whose index is read
// against another incarnation's list reads from index 1 and skips one here.
func TestNoticeCursorFromAnotherIncarnation(t *testing.T) {
	prof.Watch(t, prof.Options{})

	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	ctx := context.Background()
	over := func(seed byte) *Provider {
		p, err := New(ctx, Config{KeyFile: path, Passphrase: passphrase}, Deps{Records: records, Rand: mrand.NewChaCha8([32]byte{seed})})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}

	before := over(1)
	ns := entity.NewNamespaceID()
	put(t, records, ns, wrapped(t, before, ns, keys.KindChunkIDKey, "kept", stateCurrent, key32(9)))
	a, idA := destroyedRow(before, ns, keys.KindDataKey, "a", key32(1))
	b, idB := destroyedRow(before, ns, keys.KindDataKey, "b", key32(2))
	c, idC := destroyedRow(before, ns, keys.KindDataKey, "c", key32(3))

	put(t, records, ns, a)
	if err := before.Open(ctx, ns); err != nil {
		t.Fatalf("Open after the first destroy: %v", err)
	}
	ids, cursor := destroyedSince(t, before, 0)
	if !slices.Equal(ids, []keys.MaterialID{idA}) {
		t.Fatalf("DestroyedSince(0) after the first destroy = %+v, want %+v", ids, []keys.MaterialID{idA})
	}
	put(t, records, ns, b)
	if err := before.Open(ctx, ns); err != nil {
		t.Fatalf("Open after the second destroy: %v", err)
	}
	if ids, _ := destroyedSince(t, before, cursor); !slices.Equal(ids, []keys.MaterialID{idB}) {
		t.Fatalf("DestroyedSince(%#x) after the second destroy = %+v, want %+v", cursor, ids, []keys.MaterialID{idB})
	}

	put(t, records, ns, c)
	after := over(2)
	if err := after.Open(ctx, ns); err != nil {
		t.Fatalf("Open on the fresh provider: %v", err)
	}
	if _, next := destroyedSince(t, after, 0); next>>32 == cursor>>32 {
		t.Fatalf("both providers drew incarnation %#x, want two, or this row checks nothing", cursor>>32)
	}
	want := []keys.MaterialID{idA, idB, idC}
	if ids, _ := destroyedSince(t, after, cursor); !slices.Equal(ids, want) {
		t.Errorf("DestroyedSince(%#x, the old provider's cursor after one ID) on the fresh provider = %+v, want all three %+v", cursor, ids, want)
	}
}

// Only an Open feeds the notice: a record the provider's own update marks
// destroyed is named by no DestroyedSince until an Open finds it, then once,
// with the fingerprint its record kept, even by an Open that fails on
// another record, and a later Open names it no more. A feed from the
// writer's refresh, one lost when Open refuses, or one that repeats per
// Open fails here.
func TestNoticeFedByOpenOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	bad := wrapped(t, f.p, ns, keys.KindChunkingKey, "bad", stateCurrent, key32(2))
	bad.Record = append([]byte{2}, bad.Record[1:]...)
	put(t, f.records, ns, wrapped(t, f.p, ns, keys.KindDataKey, "d", stateCurrent, key32(1)), bad)

	gone, id := destroyedRow(f.p, ns, keys.KindDataKey, "d", key32(1))
	if err := f.p.update(ctx, ns, func([]Row) ([]Row, error) { return []Row{gone}, nil }); err != nil {
		t.Fatalf("update marking the data key destroyed: %v", err)
	}
	if ids, _ := destroyedSince(t, f.p, 0); len(ids) != 0 {
		t.Fatalf("DestroyedSince(0) after the provider's own update = %+v, want nothing before an Open", ids)
	}

	if err := f.p.Open(ctx, ns); err == nil {
		t.Fatal("Open over an unreadable current record = nil, want its refusal")
	}
	ids, next := destroyedSince(t, f.p, 0)
	if !slices.Equal(ids, []keys.MaterialID{id}) {
		t.Fatalf("DestroyedSince(0) after a refused Open = %+v, want %+v", ids, []keys.MaterialID{id})
	}

	if err := f.p.Open(ctx, ns); err == nil {
		t.Fatal("second Open over an unreadable current record = nil, want its refusal")
	}
	if ids, _ := destroyedSince(t, f.p, next); len(ids) != 0 {
		t.Errorf("DestroyedSince after a second Open = %+v, want nothing more", ids)
	}
	if ids, _ := destroyedSince(t, f.p, 0); !slices.Equal(ids, []keys.MaterialID{id}) {
		t.Errorf("DestroyedSince(0) after a second Open = %+v, want %+v once", ids, []keys.MaterialID{id})
	}
}

// A destroyed record that no longer decodes is named with a zero
// fingerprint, and once: rewritten as a destroyed record that decodes, it is
// not named again under its fingerprint. A record skipped for its bytes, or
// a notice that lists one ID per fingerprint, fails here.
func TestNoticeOfARecordThatNoLongerDecodes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newReadFixture(t)
	ctx := context.Background()
	ns := entity.NewNamespaceID()
	gone, id := destroyedRow(f.p, ns, keys.KindDataKey, "d", key32(1))
	torn := gone
	torn.Record = []byte{2}
	put(t, f.records, ns, torn)

	if err := f.p.Open(ctx, ns); err != nil {
		t.Fatalf("Open over a destroyed record only: %v", err)
	}
	unnamed := id
	unnamed.Fingerprint = [32]byte{}
	ids, next := destroyedSince(t, f.p, 0)
	if !slices.Equal(ids, []keys.MaterialID{unnamed}) {
		t.Fatalf("DestroyedSince(0) over a destroyed record that does not decode = %+v, want %+v, its fingerprint zero", ids, []keys.MaterialID{unnamed})
	}

	put(t, f.records, ns, gone)
	f.p.reload(ctx)
	if ids, _ := destroyedSince(t, f.p, next); len(ids) != 0 {
		t.Errorf("DestroyedSince after the record decodes again = %+v, want nothing: the ID was named", ids)
	}
}
