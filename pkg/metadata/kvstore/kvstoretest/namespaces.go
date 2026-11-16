package kvstoretest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// testStore is the remote store every fixture namespace is created on.
var testStore = [16]byte{0: 0x5E, 15: 1}

// createShare creates sh in a namespace of its own on testStore, both in one
// write, as a share's create names a namespace that names a remote store.
func createShare(cp kvstore.ControlPlane, sh entity.Share) (uint64, error) {
	var gen uint64
	err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
		ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: testStore}
		if _, err = tx.PutNamespace(ns, 0); err != nil {
			return err
		}
		sh.Namespaces = []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}
		gen, err = tx.PutShare(sh, 0)
		return err
	})
	return gen, err
}

// inNamespace puts a namespace on testStore and returns sh naming it.
func inNamespace(t *testing.T, cp kvstore.ControlPlane, sh entity.Share) entity.Share {
	t.Helper()
	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: testStore}
	must(t, errOf(cp.PutNamespace(ctx, ns, 0)))
	sh.Namespaces = []entity.NamespaceGen{{Generation: 1, Namespace: ns.ID}}
	return sh
}

// read runs fn in a write that commits nothing of its own.
func read(t *testing.T, cp kvstore.ControlPlane, fn func(tx entity.ControlPlaneTx) error) {
	t.Helper()
	must(t, cp.Write(ctx, entity.WriteMeta{}, fn))
}

// A namespace is named by its store from its put until its deletion, under
// the store it names now, and lists with the fields it was put with.
func testNamespaceStoreIndex(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	a, b := [16]byte{15: 0xA}, [16]byte{15: 0xB}
	named := func(store [16]byte) (got bool) {
		read(t, cp, func(tx entity.ControlPlaneTx) (err error) {
			got, err = tx.StoreNamed(store)
			return err
		})
		return got
	}
	n := entity.Namespace{ID: entity.NamespaceID{15: 2}, Store: a, Bound: []byte("bound")}
	if named(a) {
		t.Fatalf("store a named before any namespace: got true, want false")
	}
	gen, err := cp.PutNamespace(ctx, n, 0)
	must(t, err)
	if !named(a) || named(b) {
		t.Errorf("after a put on a: store a named %v, b %v; want true, false", named(a), named(b))
	}
	other := entity.Namespace{ID: entity.NamespaceID{15: 1}, Store: b}
	must(t, errOf(cp.PutNamespace(ctx, other, 0)))
	n.Generation, other.Generation = gen, 1
	if got := all(t, cp.Namespaces(ctx, entity.NamespaceID{})); !reflect.DeepEqual(got, []entity.Namespace{other, n}) {
		t.Errorf("namespaces: got %+v, want %+v in ID order", got, []entity.Namespace{other, n})
	}
	if got := all(t, cp.Namespaces(ctx, other.ID)); !reflect.DeepEqual(got, []entity.Namespace{n}) {
		t.Errorf("namespaces after %v: got %+v, want %+v", other.ID, got, []entity.Namespace{n})
	}
	must(t, cp.DeleteNamespace(ctx, other.ID, 1))

	n.Store = b
	gen, err = cp.PutNamespace(ctx, n, gen)
	must(t, err)
	if named(a) || !named(b) {
		t.Errorf("after a move to b: store a named %v, b %v; want false, true", named(a), named(b))
	}
	must(t, cp.DeleteNamespace(ctx, n.ID, gen))
	if named(b) {
		t.Errorf("store b after its only namespace's deletion: named true, want false")
	}
	read(t, cp, func(tx entity.ControlPlaneTx) error {
		if _, ok, err := tx.Namespace(n.ID); err != nil || ok {
			t.Errorf("namespace after its deletion: found %v, %v; want absent", ok, err)
		}
		return nil
	})
}

// A namespace naming no remote store is refused, and a put or delete under
// a stale expect commits nothing.
func testNamespaceRefusals(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	n := entity.Namespace{ID: entity.NamespaceID{15: 1}}
	if _, err := cp.PutNamespace(ctx, n, 0); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("namespace with no store: %v, want %v", err, merrs.ErrInvalidArgument)
	}
	n.Store = testStore
	must(t, errOf(cp.PutNamespace(ctx, n, 0)))
	for name, err := range map[string]error{
		"create over an existing one": errOf(cp.PutNamespace(ctx, n, 0)),
		"put under a stale expect":    errOf(cp.PutNamespace(ctx, n, 2)),
		"delete under a stale expect": cp.DeleteNamespace(ctx, n.ID, 2),
	} {
		if !errors.Is(err, entity.ErrGenerationMismatch) {
			t.Errorf("%s: %v, want %v", name, err, entity.ErrGenerationMismatch)
		}
	}
	got := all(t, cp.Namespaces(ctx, entity.NamespaceID{}))
	if len(got) != 1 || got[0].Generation != 1 {
		t.Errorf("namespaces after refused writes: got %+v, want one at generation 1", got)
	}
}

// A share is created only in a namespace whose record names a remote store,
// and a namespace a share names is named and cannot be deleted.
func testShareNamesNamespace(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	sh := share("data", "/data")
	sh.Namespaces = []entity.NamespaceGen{{Generation: 1, Namespace: entity.NamespaceID{15: 9}}}
	if _, err := cp.PutShare(ctx, sh, 0); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("share in a namespace with no record: %v, want %v", err, merrs.ErrInvalidArgument)
	}
	sh.Namespaces = nil
	if _, err := cp.PutShare(ctx, sh, 0); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("share naming no namespace: %v, want %v", err, merrs.ErrInvalidArgument)
	}
	if got := all(t, cp.Shares(ctx)); len(got) != 0 {
		t.Errorf("shares after refused creates: got %d, want 0", len(got))
	}

	sh = inNamespace(t, cp, sh)
	ns := sh.Namespaces[0].Namespace
	nsNamed := func() (got bool) {
		read(t, cp, func(tx entity.ControlPlaneTx) (err error) {
			got, err = tx.NamespaceNamed(ns)
			return err
		})
		return got
	}
	if nsNamed() {
		t.Errorf("namespace before a share's create: named true, want false")
	}
	must(t, errOf(cp.PutShare(ctx, sh, 0)))
	if !nsNamed() {
		t.Errorf("namespace after a share's create: named false, want true")
	}
	if got := all(t, cp.Shares(ctx)); len(got) != 1 || len(got[0].Namespaces) != 1 || got[0].Namespaces[0].Namespace != ns {
		t.Errorf("share's namespaces: got %+v, want the one it was created in, %v", got, ns)
	}
	if err := cp.DeleteNamespace(ctx, ns, 1); !merrs.IsConflictError(err) {
		t.Errorf("delete of a namespace a share names: %v, want a conflict", err)
	}
}

var errStoreNamed = errors.New("a namespace names the store")

// deleteStore stands in for a remote store's deletion: refused while a
// namespace names the store, and otherwise committing a write of the
// store's setting, so the test sees whether it committed.
func deleteStore(cp kvstore.ControlPlane, store [16]byte) error {
	return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		named, err := tx.StoreNamed(store)
		if err != nil {
			return err
		}
		if named {
			return errStoreNamed
		}
		return errOf(tx.PutSetting(entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "deleted"}, 0))
	})
}

// A store's deletion is refused while a namespace names the store, also
// when the namespace is created on it after the deletion read the index:
// the deletion then runs again and finds it.
func testStoreDeleteRacesNamespace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			first := entity.Namespace{ID: entity.NewNamespaceID(), Store: testStore}
			must(t, errOf(cp.PutNamespace(ctx, first, 0)))
			if err := deleteStore(cp, testStore); !errors.Is(err, errStoreNamed) {
				t.Errorf("delete of a store a namespace names: %v, want %v", err, errStoreNamed)
			}
			must(t, cp.DeleteNamespace(ctx, first.ID, 1))

			n := entity.Namespace{ID: entity.NewNamespaceID(), Store: testStore}
			outer, inner := race(h, func() error { return deleteStore(cp, testStore) },
				func() error { return errOf(cp.PutNamespace(ctx, n, 0)) })
			must(t, inner)
			if !errors.Is(outer, errStoreNamed) {
				t.Errorf("store deletion raced by a namespace's create on it: %v, want %v", outer, errStoreNamed)
			}
			if got := all(t, cp.Settings(ctx, entity.SettingScope{Kind: entity.ScopeRemote, ID: testStore})); len(got) != 0 {
				t.Errorf("store settings after a refused deletion: got %+v, want none", got)
			}
		})
	}
}

// A namespace's deletion and a share's create into it race: whichever
// commits second runs again and is refused, in both orders.
func testNamespaceDeleteRacesShare(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		for _, c := range []struct {
			name           string
			deleteIsOuter  bool
			wantOuter      func(error) bool
			wantShares     int
			wantNamespaces int
		}{
			{"delete commits second", true, merrs.IsConflictError, 1, 1},
			{"create commits second", false, func(err error) bool { return isCode(err, merrs.ErrInvalidArgument) }, 0, 0},
		} {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				s, h := heldStore(t, b)
				cp := s.ControlPlane()
				sh := inNamespace(t, cp, share("data", "/data"))
				del := func() error { return cp.DeleteNamespace(ctx, sh.Namespaces[0].Namespace, 1) }
				create := func() error { return errOf(cp.PutShare(ctx, sh, 0)) }
				outer, inner := create, del
				if c.deleteIsOuter {
					outer, inner = del, create
				}
				gotOuter, gotInner := race(h, outer, inner)
				must(t, gotInner)
				if !c.wantOuter(gotOuter) {
					t.Errorf("the write committing second: %v, want refused", gotOuter)
				}
				if got := len(all(t, cp.Shares(ctx))); got != c.wantShares {
					t.Errorf("shares after the race: got %d, want %d", got, c.wantShares)
				}
				if got := len(all(t, cp.Namespaces(ctx, entity.NamespaceID{}))); got != c.wantNamespaces {
					t.Errorf("namespaces after the race: got %d, want %d", got, c.wantNamespaces)
				}
			})
		}
	}
}
