package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// The vector testdata/salvage-wrap-v1.py computes: the salvage AAD of
// salvageVectorExport and its stored wrap.
//
// ponytail: pinned here from this package's own script until RFC 5 carries a
// salvage golden vector and its vectors have a shared loader; move it there
// then, and pin keystest.Memory's salvage AAD to it too.
const (
	salvageVectorExport = "journal-7/tag-3"
	salvageVectorAAD    = "000000000000000b73616c766167652d6b65790000000f6a6f75726e616c2d372f7461672d33"
	salvageVectorStored = "010010101112131415161718191a1b1c1d1e1f0000003ca0a1a2a3a4a5a6a7a8a9aaabb6492e7e119e54e83a3cdd885b279e8110cd3b73f6d2240bf4674ced13c61b6e7cd66dc39dbc6c17be314712c5dafadd"
)

// WrapSalvageKey's key is the 32 bytes Deps.Rand serves first, and its wrap
// equals the vector another AES-GCM implementation computed from Appendix
// B.1's key wrap and stored wrap, over an empty namespace and kind
// salvage-key: an AAD that drops the empty namespace, misspells the kind or
// frames the export apart from the key wrap's layout fails here.
func TestSalvageWrapVector(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var masterKeyID [16]byte
	copy(masterKeyID[:], span(0x10, 0x20))
	key, nonce := span(0x50, 0x70), span(0xA0, 0xAC)

	rnd := bytes.NewReader(slices.Concat(key, nonce))
	p, err := build(masterKeyID, span(0x00, 0x20), Deps{Records: keystest.NewRecords(), Rand: rnd})
	if err != nil {
		t.Fatal(err)
	}

	gotKey, wrap, err := p.WrapSalvageKey(context.Background(), []byte(salvageVectorExport))
	if err != nil {
		t.Fatalf("WrapSalvageKey: %v", err)
	}
	if !bytes.Equal(gotKey, key) {
		t.Errorf("salvage key %x, want the first 32 bytes Rand served, %x", gotKey, key)
	}
	if got := hex.EncodeToString(wrap); got != salvageVectorStored {
		t.Errorf("salvage wrap %s, want the vector's %s", got, salvageVectorStored)
	}
	if rnd.Len() != 0 {
		t.Errorf("WrapSalvageKey left %d of the vector's 44 random bytes unread, want 0", rnd.Len())
	}

	if got := hex.EncodeToString(wrapAAD(nil, kindSalvageKey, salvageVectorExport)); got != salvageVectorAAD {
		t.Errorf("salvage AAD %s, want the vector's %s", got, salvageVectorAAD)
	}
}

// A salvage wrap unwraps under its master key and its own export's AAD only:
// not under another export's, nor as any namespace's key, nor under another
// master key. A salvage AAD that omits the export, or one a namespace key's
// can equal, fails here.
func TestSalvageWrapBindsItsExport(t *testing.T) {
	prof.Watch(t, prof.Options{})

	p, _, _ := newProvider(t, rand.Reader)
	export := []byte("journal-1/tag-1")

	key, wrap, err := p.WrapSalvageKey(context.Background(), export)
	if err != nil {
		t.Fatalf("WrapSalvageKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("salvage key of %d bytes, want 32", len(key))
	}

	own := wrapAAD(nil, kindSalvageKey, string(export))
	if got, err := p.unwrap(wrap, own); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap under its export's AAD = %x, %v; want the key", got, err)
	}

	ns := entity.NewNamespaceID()
	for _, tc := range []struct {
		name string
		aad  []byte
	}{
		{"another export", wrapAAD(nil, kindSalvageKey, "journal-1/tag-2")},
		{"an empty export", wrapAAD(nil, kindSalvageKey, "")},
		{"the export under a namespace", wrapAAD(ns[:], kindSalvageKey, string(export))},
		{"the export as a data key's ID", wrapAAD(nil, keys.KindDataKey, string(export))},
	} {
		if got, err := p.unwrap(wrap, tc.aad); err == nil {
			t.Errorf("unwrap under %s's AAD = %x, nil; want an error", tc.name, got)
		}
	}

	elsewhere, _, _ := newProvider(t, rand.Reader)
	if got, err := elsewhere.unwrap(wrap, own); err == nil {
		t.Errorf("unwrap under another key file's master key = %x, nil; want an error", got)
	}

	sameID, err := build(p.masterKeyID, make([]byte, 32), Deps{Records: keystest.NewRecords(), Rand: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sameID.unwrap(wrap, own); err == nil || errors.Is(err, errStoredWrap) {
		t.Errorf("unwrap under other key bytes with the same master key ID = %x, %v; want the AEAD's refusal", got, err)
	}
}

// updates passes every call through to a KeyRecords and keeps the namespaces
// Update was called for, so a test can assert that no key record was written.
type updates struct {
	KeyRecords
	nss []entity.NamespaceID
}

func (u *updates) Update(ctx context.Context, ns entity.NamespaceID, fn func(uint64, []Row) (uint64, []Row, error)) error {
	u.nss = append(u.nss, ns)
	return u.KeyRecords.Update(ctx, ns, fn)
}

// Each salvage wrap raises the master key's count by exactly one, and the
// wrap past 2³² is refused with ErrWrapLimit; two wraps differ in key and
// wrap, and none writes a key record. An uncounted wrap, a wrap counted
// twice, a key drawn twice the same, or a salvage key written as material
// fails here.
func TestSalvageWrapCountsAndWritesNoRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	path, _, _ := keyFile(t)
	records := keystest.NewRecords()
	view := &updates{KeyRecords: records}
	p, err := New(ctx, Config{KeyFile: path, Passphrase: passphrase}, Deps{Records: view, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if ok, err := records.RaiseWrapCount(ctx, p.masterKeyID[:], 1<<32-2, 1<<32); !ok || err != nil {
		t.Fatalf("raise the count to 2³² − 2 = %v, %v; want true, nil", ok, err)
	}

	k1, w1, err := p.WrapSalvageKey(ctx, []byte("export-1"))
	if err != nil {
		t.Fatalf("the 2³² − 1st wrap, a salvage key's: %v, want nil", err)
	}
	k2, w2, err := p.WrapSalvageKey(ctx, []byte("export-1"))
	if err != nil {
		t.Fatalf("the 2³²nd wrap, a salvage key's: %v, want nil (a salvage wrap counted more than once?)", err)
	}
	if bytes.Equal(k1, k2) || bytes.Equal(w1, w2) {
		t.Errorf("two salvage wraps of one export returned keys %x and %x, wraps %x and %x; want both to differ", k1, k2, w1, w2)
	}

	if k, w, err := p.WrapSalvageKey(ctx, []byte("export-2")); !errors.Is(err, keys.ErrWrapLimit) || k != nil || w != nil {
		t.Errorf("the salvage wrap past 2³² = %x, %x, %v; want nil, nil, keys.ErrWrapLimit (a salvage wrap uncounted?)", k, w, err)
	}

	if len(view.nss) != 0 {
		t.Errorf("salvage wraps called Update for namespaces %v, want no key record written", view.nss)
	}
}
