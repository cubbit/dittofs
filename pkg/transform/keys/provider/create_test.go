package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// encryptingKinds and plainKinds are the kinds an encrypting and a
// non-encrypting namespace get at creation.
var (
	encryptingKinds = []string{keys.KindChunkIDKey, kindExportKey, keys.KindDataKey, keys.KindChunkingKey}
	plainKinds      = []string{keys.KindChunkIDKey, kindExportKey}
)

// escrowedConfig is a Config over a fresh key file carrying that key's
// escrow pass, so the gate is open.
func escrowedConfig(t *testing.T) Config {
	t.Helper()

	path, id, key := keyFile(t)
	return Config{KeyFile: path, Passphrase: passphrase, Escrowed: []Escrow{{MasterKeyID: id, Fingerprint: fingerprint(key)}}}
}

func newOver(t *testing.T, cfg Config, records KeyRecords) *Provider {
	t.Helper()

	p, err := New(context.Background(), cfg, Deps{Records: records, Rand: rand.Reader})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// wrapCount is masterKeyID's wrap count in records, found by raises of zero,
// which change nothing.
func wrapCount(t *testing.T, records KeyRecords, masterKeyID []byte) uint64 {
	t.Helper()

	lo, hi := uint64(0), uint64(1<<32)
	for lo < hi {
		mid := lo + (hi-lo)/2
		ok, err := records.RaiseWrapCount(context.Background(), masterKeyID, 0, mid)
		if err != nil {
			t.Fatalf("RaiseWrapCount by 0: %v", err)
		}
		if ok {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// currentIDs is the ID Current answers for each of kinds in ns, after
// checking that Get serves 32 bytes matching its fingerprint.
func currentIDs(t *testing.T, p *Provider, ns entity.NamespaceID, kinds []string) map[string]string {
	t.Helper()

	ctx := context.Background()
	ids := make(map[string]string, len(kinds))
	for _, kind := range kinds {
		mid, err := p.Current(ctx, ns, kind)
		if err != nil {
			t.Fatalf("Current of %s: %v", kind, err)
		}
		key, err := p.Get(ctx, mid)
		if err != nil {
			t.Fatalf("Get of the current %s: %v", kind, err)
		}
		if len(key) != 32 || fingerprint(key) != mid.Fingerprint {
			t.Errorf("Get of the current %s = %d bytes, fingerprint match %v; want 32 bytes matching", kind, len(key), fingerprint(key) == mid.Fingerprint)
		}
		ids[kind] = mid.ID
	}
	return ids
}

// loadRows is ns's rows as the view holds them.
func loadRows(t *testing.T, records KeyRecords, ns entity.NamespaceID) []Row {
	t.Helper()

	rows, err := records.Load(context.Background(), ns)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return rows
}

// An encrypting namespace gets four current keys, all wrapped; a
// non-encrypting one gets two, its chunk-ID key in the clear with no master
// key ID, and no chunking or data key. Each is served by Current and Get, and
// the count rises once by the number of wrapped keys. A missing or extra
// kind, a wrapped clear key, a clear wrapped one or an uncounted wrap fails
// here.
func TestCreateKeysGivesTheKeyTable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		encrypts  bool
		kinds     []string
		clearKind string // the one kind held in the clear, if any
	}{
		{"encrypting", true, encryptingKinds, ""},
		{"non-encrypting", false, plainKinds, keys.KindChunkIDKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := keystest.NewRecords()
			p := newOver(t, escrowedConfig(t), records)
			ns := entity.NewNamespaceID()

			if err := p.CreateKeys(ctx, ns, tc.encrypts); err != nil {
				t.Fatalf("CreateKeys: %v", err)
			}

			rows := loadRows(t, records, ns)
			var kinds []string
			wrappedKeys := uint64(0)
			for _, row := range rows {
				kinds = append(kinds, row.Kind)
				if row.State != stateCurrent {
					t.Errorf("%s row state = %q, want %q", row.Kind, row.State, stateCurrent)
				}
				rec, err := decodeKeyRecord(row.Record)
				if err != nil {
					t.Fatalf("decode the %s record: %v", row.Kind, err)
				}
				clearKey := len(rec.masterKeyID) == 0
				if clearKey != (row.Kind == tc.clearKind) {
					t.Errorf("%s record held in the clear = %v, want %v", row.Kind, clearKey, row.Kind == tc.clearKind)
				}
				if !clearKey {
					wrappedKeys++
					if !bytes.Equal(rec.masterKeyID, p.masterKeyID[:]) {
						t.Errorf("%s record names master key %x, want %x", row.Kind, rec.masterKeyID, p.masterKeyID)
					}
				}
			}
			if !sameKinds(kinds, tc.kinds) {
				t.Errorf("rows of kinds %v, want %v", kinds, tc.kinds)
			}
			if got := wrapCount(t, records, p.masterKeyID[:]); got != wrappedKeys {
				t.Errorf("wrap count after CreateKeys = %d, want %d, one per wrapped key", got, wrappedKeys)
			}

			// A fresh provider serves every key from the records alone.
			currentIDs(t, newOver(t, Config{KeyFile: p.cfg.Load().KeyFile, Passphrase: passphrase}, records), ns, tc.kinds)
			if !tc.encrypts {
				for _, kind := range []string{keys.KindChunkingKey, keys.KindDataKey} {
					if _, err := p.Current(ctx, ns, kind); !isOnly(err, keys.ErrUnknownMaterial) {
						t.Errorf("Current of %s in a non-encrypting namespace = %v, want ErrUnknownMaterial alone", kind, err)
					}
				}
			}
		})
	}
}

func sameKinds(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// Every key ID is a random UUID's 16 bytes; the export key's is 24, its
// first 8 the big-endian generation 1. A generation laid out otherwise, or an
// ID that is not minted, fails here.
func TestCreateKeysMintsIDs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	records := keystest.NewRecords()
	p := newOver(t, escrowedConfig(t), records)
	ns := entity.NewNamespaceID()
	if err := p.CreateKeys(context.Background(), ns, true); err != nil {
		t.Fatalf("CreateKeys: %v", err)
	}

	for kind, id := range currentIDs(t, p, ns, encryptingKinds) {
		minted := []byte(id)
		if kind == kindExportKey {
			if len(id) != 24 || binary.BigEndian.Uint64(minted[:8]) != 1 {
				t.Errorf("export key ID = %x, want 24 bytes starting with the big-endian generation 1", id)
				continue
			}
			minted = minted[8:]
		}
		u, err := uuid.FromBytes(minted)
		if err != nil || u.Version() != 4 || u.Variant() != uuid.RFC4122 {
			t.Errorf("%s ID = %x, want a random UUID's 16 bytes", kind, id)
		}
	}
}

// A second CreateKeys of a namespace writes no row and keeps its IDs, its
// wraps only overcounting. A set replaced on retry fails here.
func TestCreateKeysIsIdempotent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	records := keystest.NewRecords()
	p := newOver(t, escrowedConfig(t), records)
	ns := entity.NewNamespaceID()
	if err := p.CreateKeys(ctx, ns, true); err != nil {
		t.Fatalf("CreateKeys: %v", err)
	}
	before := loadRows(t, records, ns)
	ids := currentIDs(t, p, ns, encryptingKinds)

	if err := p.CreateKeys(ctx, ns, true); err != nil {
		t.Fatalf("the second CreateKeys: %v", err)
	}
	if after := loadRows(t, records, ns); !sameRows(before, after) {
		t.Errorf("rows after the second CreateKeys = %s, want them unchanged from %s", rowNames(after), rowNames(before))
	}
	if got := currentIDs(t, p, ns, encryptingKinds); !maps.Equal(got, ids) {
		t.Errorf("IDs after the second CreateKeys = %x, want %x", got, ids)
	}
}

// rowNames names rows by kind, ID in hex and state, for a failure message.
func rowNames(rows []Row) []string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, fmt.Sprintf("%s %x %s", r.Kind, r.ID, r.State))
	}
	return names
}

func sameRows(a, b []Row) bool {
	key := func(r Row) string { return r.Kind + "\x00" + r.ID }
	cmp := func(x, y Row) int { return bytes.Compare([]byte(key(x)), []byte(key(y))) }
	a, b = slices.SortedFunc(slices.Values(a), cmp), slices.SortedFunc(slices.Values(b), cmp)
	return slices.EqualFunc(a, b, func(x, y Row) bool {
		return x.Kind == y.Kind && x.ID == y.ID && x.State == y.State && bytes.Equal(x.Record, y.Record)
	})
}

// Eight creations of one namespace, over two providers sharing the records,
// each paused after its wraps until all eight have wrapped: one set of rows
// results, and every caller then reads its IDs. A creation outside the
// view's guard fails here.
func TestConcurrentCreateKeysLeaveOneSet(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const n = 8
	ctx := context.Background()
	cfg := escrowedConfig(t)
	records := keystest.NewRecords()
	providers := []*Provider{newOver(t, cfg, records), newOver(t, cfg, records)}

	var wrapped sync.WaitGroup
	wrapped.Add(n)
	release := make(chan struct{})
	for _, p := range providers {
		p.pauseBeforeCreate(func() {
			wrapped.Done()
			<-release
		})
	}

	ns := entity.NewNamespaceID()
	errs := make(chan error, n)
	for i := range n {
		go func() { errs <- providers[i%2].CreateKeys(ctx, ns, true) }()
	}

	allWrapped := make(chan struct{})
	go func() { wrapped.Wait(); close(allWrapped) }()
	select {
	case <-allWrapped:
	case <-time.After(30 * time.Second):
		t.Fatal("the eight creations did not all reach their update")
	}
	close(release)
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("CreateKeys: %v", err)
		}
	}

	rows := loadRows(t, records, ns)
	if len(rows) != len(encryptingKinds) {
		t.Fatalf("%d rows after eight concurrent creations, want %d, one set", len(rows), len(encryptingKinds))
	}
	want := make(map[string]string, len(rows))
	for _, row := range rows {
		want[row.Kind] = row.ID
	}
	for i, p := range providers {
		if got := currentIDs(t, p, ns, encryptingKinds); !maps.Equal(got, want) {
			t.Errorf("provider %d reads IDs %x, want the one set's %x", i, got, want)
		}
	}
	if got, want := wrapCount(t, records, providers[0].masterKeyID[:]), uint64(n*len(encryptingKinds)); got != want {
		t.Errorf("wrap count = %d, want %d: every creation's wraps counted", got, want)
	}
}

// errCommit is the fault failSets injects.
var errCommit = errors.New("injected commit failure")

// failSets fails every write whose key, as keys.Describe renders it, starts
// with prefix, so the transaction making it commits nothing.
type failSets struct{ prefix string }

func (f failSets) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpSet && bytes.HasPrefix(key, []byte(f.prefix)) {
		return 0, 0, errCommit
	}
	return 0, 0, nil
}

// openBadger opens a Badger store under dir, closed when the test ends.
func openBadger(t *testing.T, dir string) kv.KV {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open Badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func storeOver(t *testing.T, db kv.KV) kvstore.KeyRecords {
	t.Helper()

	s, err := kvstore.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("kvstore.Open: %v", err)
	}
	return s.KeyRecords()
}

// With the metadata store over Badger, a creation whose records' commit fails
// leaves nothing a fresh provider serves, its wraps counted, and the next
// creation makes a full set; a committed set is found by a fresh provider
// over the reopened store and kept by a repeated creation. A key served
// without its record, or a set replaced on retry, fails here.
func TestCreateKeysFailedAndCommittedOnTheStore(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	dir := t.TempDir()
	cfg := escrowedConfig(t)
	inner, err := badgerkv.Open(badgerkv.Options{Dir: dir})
	if err != nil {
		t.Fatalf("open Badger: %v", err)
	}
	db := kvtest.NewFaultKV(inner)
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatalf("kvstore.Init: %v", err)
	}
	records := storeOver(t, db)
	p := newOver(t, cfg, records)
	ns := entity.NewNamespaceID()

	db.SetFault(failSets{prefix: "NS‖"})
	if err := p.CreateKeys(ctx, ns, true); !errors.Is(err, errCommit) {
		t.Fatalf("CreateKeys whose commit fails = %v, want the injected failure", err)
	}
	db.SetFault(nil)

	fresh := newOver(t, cfg, records)
	for _, kind := range encryptingKinds {
		if _, err := fresh.Current(ctx, ns, kind); !isOnly(err, keys.ErrUnknownMaterial) {
			t.Errorf("Current of %s after the failed creation = %v, want ErrUnknownMaterial alone", kind, err)
		}
	}
	if got := wrapCount(t, records, p.masterKeyID[:]); got != uint64(len(encryptingKinds)) {
		t.Errorf("wrap count after the failed creation = %d, want %d: its wraps counted", got, len(encryptingKinds))
	}

	if err := p.CreateKeys(ctx, ns, true); err != nil {
		t.Fatalf("CreateKeys after the failed one: %v", err)
	}
	ids := currentIDs(t, newOver(t, cfg, records), ns, encryptingKinds)

	if err := inner.Close(); err != nil {
		t.Fatalf("close Badger: %v", err)
	}
	reopened := storeOver(t, openBadger(t, dir))
	again := newOver(t, cfg, reopened)
	if got := currentIDs(t, again, ns, encryptingKinds); !maps.Equal(got, ids) {
		t.Errorf("IDs over the reopened store = %x, want the committed %x", got, ids)
	}
	if err := again.CreateKeys(ctx, ns, true); err != nil {
		t.Fatalf("CreateKeys over the reopened store: %v", err)
	}
	if got := currentIDs(t, newOver(t, cfg, reopened), ns, encryptingKinds); !maps.Equal(got, ids) {
		t.Errorf("IDs after a repeated creation = %x, want the committed %x", got, ids)
	}
}

// With the gate shut, an encrypting namespace's creation is ErrNotEscrowed
// naming the key, writes nothing and counts nothing, while a non-encrypting
// one succeeds; once a check passes and Reconfigure hands the pass over, the
// encrypting creation succeeds. A gate on the wrong kind of namespace, or a
// wrap before the gate, fails here.
func TestCreateKeysEscrowGate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	p, records, path, _, _ := escrowProvider(t)
	ns := entity.NewNamespaceID()

	if err := p.CreateKeys(ctx, ns, true); !isNotEscrowed(p, err) {
		t.Fatalf("CreateKeys of an encrypting namespace, gate shut = %v, want ErrNotEscrowed naming the key", err)
	}
	if rows := loadRows(t, records, ns); len(rows) != 0 {
		t.Errorf("rows after the refused creation = %v, want none", rows)
	}
	if got := wrapCount(t, records, p.masterKeyID[:]); got != 0 {
		t.Errorf("wrap count after the refused creation = %d, want 0", got)
	}

	plain := entity.NewNamespaceID()
	if err := p.CreateKeys(ctx, plain, false); err != nil {
		t.Fatalf("CreateKeys of a non-encrypting namespace, gate shut = %v, want nil", err)
	}
	currentIDs(t, p, plain, plainKinds)

	pass, err := p.VerifyEscrow(ctx, readFile(t, path))
	if err != nil {
		t.Fatalf("VerifyEscrow: %v", err)
	}
	if err := p.CreateKeys(ctx, ns, true); !isNotEscrowed(p, err) {
		t.Fatalf("CreateKeys after a pass not handed back = %v, want ErrNotEscrowed", err)
	}
	if err := p.Reconfigure(ctx, Config{KeyFile: path, Passphrase: passphrase, Escrowed: []Escrow{pass}}); err != nil {
		t.Fatalf("Reconfigure with the pass: %v", err)
	}
	if err := p.CreateKeys(ctx, ns, true); err != nil {
		t.Fatalf("CreateKeys after the pass = %v, want nil", err)
	}
	currentIDs(t, p, ns, encryptingKinds)
}

// Two non-encrypting namespaces get different chunk-ID keys. A fixed or
// shared chunk-ID key fails here.
func TestCreateKeysChunkIDKeysDiffer(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	p := newOver(t, escrowedConfig(t), keystest.NewRecords())
	var got [][]byte
	for range 2 {
		ns := entity.NewNamespaceID()
		if err := p.CreateKeys(ctx, ns, false); err != nil {
			t.Fatalf("CreateKeys: %v", err)
		}
		mid, err := p.Current(ctx, ns, keys.KindChunkIDKey)
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		key, err := p.Get(ctx, mid)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got = append(got, key)
	}
	if bytes.Equal(got[0], got[1]) {
		t.Errorf("two non-encrypting namespaces share chunk-ID key %x, want different keys", got[0])
	}
}
