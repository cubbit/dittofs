package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	kvkeys "github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// resolveKey passes every call to the real provider. It counts the Unwrap
// calls and keeps each data key Unwrap returned, so a test can see whether a
// refusal came before the unwrap and whether the key was cleared after use.
type resolveKey struct {
	keyprovider.KeyProvider
	unwraps int
	kept    [][]byte
}

func (k *resolveKey) Unwrap(ctx context.Context, wrapped []byte, id string) ([]byte, error) {
	dataKey, err := k.KeyProvider.Unwrap(ctx, wrapped, id)

	k.unwraps++
	if err == nil {
		k.kept = append(k.kept, dataKey)
	}

	return dataKey, err
}

// storeOp is one operation that reached the store.
type storeOp struct {
	op  string
	key string
}

// opRecorder is a fault that fails nothing and records every operation, so a
// test can count the store reads a call makes.
type opRecorder struct {
	mu  sync.Mutex
	ops []storeOp
}

func (r *opRecorder) At(op string, key []byte) (time.Duration, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ops = append(r.ops, storeOp{
		op:  op,
		key: string(key),
	})

	return 0, 0, nil
}

func (r *opRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ops = nil
}

func (r *opRecorder) taken() []storeOp {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.ops)
}

// resolveFixture is a provider over a recorded in-memory store and the two
// real role keys, each behind a resolveKey.
type resolveFixture struct {
	db       *kvtest.FaultKV
	cp       kvstore.ControlPlane
	keys     map[Role]keyprovider.KeyProvider
	protocol *resolveKey
	storage  *resolveKey
	p        *Provider
	rec      *opRecorder
}

func newResolveFixture(t *testing.T) *resolveFixture {
	t.Helper()

	db := kvtest.NewFaultKV(sealStore(t, ""))
	cp := sealControlPlane(t, db)
	keys := sealKeys(t)
	protocol := &resolveKey{KeyProvider: keys[Protocol]}
	storage := &resolveKey{KeyProvider: keys[Storage]}
	rec := &opRecorder{}
	db.SetFault(rec)

	return &resolveFixture{
		db:       db,
		cp:       cp,
		keys:     keys,
		protocol: protocol,
		storage:  storage,
		p: sealProvider(t, cp, map[Role]keyprovider.KeyProvider{
			Protocol: protocol,
			Storage:  storage,
		}),
		rec: rec,
	}
}

func (f *resolveFixture) unwraps() int {
	return f.protocol.unwraps + f.storage.unwraps
}

// resolvePut seals value with p as the next version of ref and commits it.
func resolvePut(
	t *testing.T,
	p *Provider,
	cp kvstore.ControlPlane,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value string,
	m Meta,
) entity.Secret {
	t.Helper()

	ctx := context.Background()

	role, _ := roleOf(kind)

	s, err := p.Seal(ctx, role, kind, owner, ref, []byte(value), m)
	if err != nil {
		t.Fatalf("seal %s: %v", kind, err)
	}

	if err := sealCommit(ctx, cp, s); err != nil {
		t.Fatalf("commit %s: %v", kind, err)
	}

	return s
}

// resolveCommit commits a record built by hand, as a writer of the store
// could.
func resolveCommit(t *testing.T, cp kvstore.ControlPlane, s entity.Secret) {
	t.Helper()

	if err := sealCommit(context.Background(), cp, s); err != nil {
		t.Fatalf("commit a crafted record: %v", err)
	}
}

// secretKVKey is the store key of one version of a secret, as kvstore lays
// it out: the SEC kind, the reference, the version big-endian.
func secretKVKey(ref entity.SecretRef, version uint64) []byte {
	return kvkeys.Uint64(kvkeys.ID([]byte{kvkeys.KindSEC}, ref[:]), version)
}

func rawGet(t *testing.T, db kv.KV, key []byte) []byte {
	t.Helper()

	var out []byte

	err := db.View(context.Background(), func(r kv.Reader) error {
		b, err := r.Get(key)
		out = append([]byte(nil), b...)

		return err
	})
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}

	return out
}

func rawUpdate(t *testing.T, db kv.KV, fn func(kv.Txn) error) {
	t.Helper()

	if err := db.Update(context.Background(), fn); err != nil {
		t.Fatalf("raw update: %v", err)
	}
}

func resolveOwnerVariants() map[string]entity.SecretOwner {
	out := map[string]entity.SecretOwner{}

	o := sealOwner()
	o.Record.Kind = entity.ResourceGroup
	out["record kind"] = o

	o = sealOwner()
	o.Record.ID = []byte{0x0a, 0x0b, 0x0d}
	out["record ID"] = o

	o = sealOwner()
	o.Scope.Kind = entity.ScopeInstallation
	out["scope kind"] = o

	o = sealOwner()
	o.Scope.ID[0] = 0x6a
	out["scope ID"] = o

	o = sealOwner()
	o.Key = "password"
	out["key"] = o

	return out
}

func TestResolveRoleKeys(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	nt, cred := sealRef(t), sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, nt, "the NT hash", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretRemoteCredential, owner, cred, "the credential", Meta{})

	if got, err := f.p.Resolve(ctx, Protocol, entity.SecretNTHash, owner, nt); err != nil ||
		string(got) != "the NT hash" {
		t.Errorf("the NT hash under protocol: %q, %v; want \"the NT hash\"", got, err)
	}

	got, err := f.p.Resolve(ctx, Storage, entity.SecretRemoteCredential, owner, cred)
	if err != nil || string(got) != "the credential" {
		t.Errorf("the credential under storage: %q, %v; want \"the credential\"", got, err)
	}

	swapped := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{
		Protocol: f.keys[Storage],
		Storage:  f.keys[Protocol],
	})

	got, err = swapped.Resolve(ctx, Protocol, entity.SecretNTHash, owner, nt)
	if !errors.Is(err, keyprovider.ErrWrongMasterKey) || got != nil {
		t.Errorf("the NT hash under the storage key: %q, %v; want ErrWrongMasterKey", got, err)
	}

	got, err = swapped.Resolve(ctx, Storage, entity.SecretRemoteCredential, owner, cred)
	if !errors.Is(err, keyprovider.ErrWrongMasterKey) || got != nil {
		t.Errorf("the credential under the protocol key: %q, %v; want ErrWrongMasterKey", got, err)
	}
}

func TestResolveRefusesWrongRole(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	nt, cred := sealRef(t), sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, nt, "nt", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretRemoteCredential, owner, cred, "c", Meta{})

	undeclared := firstUndeclaredKind()

	refused := []struct {
		role Role
		kind entity.SecretKind
		ref  entity.SecretRef
	}{
		{Protocol, 0, nt},
		{Storage, 0, nt},
		{0, 0, nt},
		{Protocol, undeclared, nt},
		{Storage, undeclared, nt},
		{0, undeclared, nt},
		{0, entity.SecretNTHash, nt},
		{3, entity.SecretNTHash, nt},
		{3, entity.SecretPassword, nt},
		{Storage, entity.SecretNTHash, nt},
		{Protocol, entity.SecretRemoteCredential, cred},
	}

	for _, c := range refused {
		f.rec.reset()

		value, err := f.p.Resolve(ctx, c.role, c.kind, owner, c.ref)
		if !errors.Is(err, ErrWrongRole) || value != nil {
			t.Errorf(
				"Resolve kind %d under role %d: %q, %v; want ErrWrongRole",
				c.kind,
				c.role,
				value,
				err,
			)
		}

		list, err := f.p.Versions(ctx, c.role, c.kind, owner, c.ref)
		if !errors.Is(err, ErrWrongRole) || list != nil {
			t.Errorf(
				"Versions kind %d under role %d: %v, %v; want ErrWrongRole",
				c.kind,
				c.role,
				list,
				err,
			)
		}

		if ops := f.rec.taken(); len(ops) != 0 {
			t.Errorf("kind %d under role %d: store ops %v, want none", c.kind, c.role, ops)
		}

		if n := f.unwraps(); n != 0 {
			t.Errorf("kind %d under role %d: %d unwraps, want none", c.kind, c.role, n)
		}
	}
}

func TestResolveNeedsTheRoleKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	nt, cred := sealRef(t), sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, nt, "nt", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretRemoteCredential, owner, cred, "the credential", Meta{})

	storageOnly := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{
		Storage: f.keys[Storage],
	})

	f.rec.reset()

	value, err := storageOnly.Resolve(ctx, Protocol, entity.SecretNTHash, owner, nt)
	if !errors.Is(err, errNoRoleKey) || errors.Is(err, ErrWrongRole) || value != nil {
		t.Errorf("Resolve an NT hash with no protocol key: %q, %v; want errNoRoleKey", value, err)
	}

	list, err := storageOnly.Versions(ctx, Protocol, entity.SecretNTHash, owner, nt)
	if !errors.Is(err, errNoRoleKey) || errors.Is(err, ErrWrongRole) || list != nil {
		t.Errorf(
			"Versions of an NT hash with no protocol key: %v, %v; want errNoRoleKey",
			list,
			err,
		)
	}

	if ops := f.rec.taken(); len(ops) != 0 {
		t.Errorf("a refusal for a missing key: store ops %v, want none", ops)
	}

	got, err := storageOnly.Resolve(ctx, Storage, entity.SecretRemoteCredential, owner, cred)
	if err != nil || string(got) != "the credential" {
		t.Errorf(
			"the credential with the storage key only: %q, %v; want \"the credential\"",
			got,
			err,
		)
	}

	// Called as the identity server calls it, on the zero provider its tests
	// build.
	zero := &Provider{}
	dirOwner := entity.SecretOwner{
		Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key:   "identity.directory",
	}

	value, err = zero.Resolve(ctx, Protocol, entity.SecretDirectoryCredential, dirOwner, nt)
	if !errors.Is(err, errNoRoleKey) || value != nil {
		t.Errorf("Resolve on the zero provider: %q, %v; want errNoRoleKey", value, err)
	}

	list, err = zero.Versions(ctx, Protocol, entity.SecretDirectoryCredential, dirOwner, nt)
	if !errors.Is(err, errNoRoleKey) || list != nil {
		t.Errorf("Versions on the zero provider: %v, %v; want errNoRoleKey", list, err)
	}
}

func TestResolveRefusesKindOrOwner(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	nt := sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, nt, "nt", Meta{})

	type ask struct {
		kind  entity.SecretKind
		owner entity.SecretOwner
	}

	cases := map[string]ask{
		"another kind": {entity.SecretKeytab, owner},
	}

	for name, o := range resolveOwnerVariants() {
		cases["another "+name] = ask{entity.SecretNTHash, o}
	}

	for name, c := range cases {
		value, err := f.p.Resolve(ctx, Protocol, c.kind, c.owner, nt)
		if !errors.Is(err, ErrWrongKind) || value != nil {
			t.Errorf("Resolve, %s: %q, %v; want ErrWrongKind", name, value, err)
		}

		list, err := f.p.Versions(ctx, Protocol, c.kind, c.owner, nt)
		if !errors.Is(err, ErrWrongKind) || list != nil {
			t.Errorf("Versions, %s: %v, %v; want ErrWrongKind", name, list, err)
		}

		if n := f.unwraps(); n != 0 {
			t.Errorf("%s: %d unwraps, want none before the refusal", name, n)
		}
	}

	// A version below the current one, written straight into the store with
	// another owner, is refused by Versions before its unwrap.
	other := sealOwner()
	other.Key = "password"
	foreign, target := sealRef(t), sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, other, foreign, "foreign", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, target, "t1", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, target, "t2", Meta{})

	stored := rawGet(t, f.db, secretKVKey(foreign, 1))
	rawUpdate(t, f.db, func(tx kv.Txn) error {
		return tx.Set(secretKVKey(target, 1), stored)
	})

	f.protocol.unwraps = 0

	list, err := f.p.Versions(ctx, Protocol, entity.SecretNTHash, owner, target)
	if !errors.Is(err, ErrWrongKind) || list != nil {
		t.Errorf("Versions over a version of another owner: %v, %v; want ErrWrongKind", list, err)
	}

	if n := f.protocol.unwraps; n != 1 {
		t.Errorf("Versions over a version of another owner: %d unwraps, want 1 (the current)", n)
	}
}

func TestResolveMovedEnvelopeFails(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)

	// One key behind both roles, so the key-ID check passes and only the
	// open can refuse.
	one := &resolveKey{KeyProvider: f.keys[Protocol]}
	p := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{
		Protocol: one,
		Storage:  one,
	})

	owner := sealOwner()
	other := sealOwner()
	other.Key = "password"

	a := resolvePut(t, p, f.cp, entity.SecretNTHash, owner, sealRef(t), "a", Meta{})
	moved := func(
		ref entity.SecretRef,
		kind entity.SecretKind,
		o entity.SecretOwner,
		version uint64,
	) entity.Secret {
		return entity.Secret{
			ID:      ref,
			Kind:    kind,
			Version: version,
			KeyID:   a.KeyID,
			Owner:   o,
			Value:   a.Value,
		}
	}

	versioned := sealRef(t)
	v1 := resolvePut(t, p, f.cp, entity.SecretNTHash, owner, versioned, "b1", Meta{})
	resolvePut(t, p, f.cp, entity.SecretNTHash, owner, versioned, "b2", Meta{})

	third := v1
	third.Version = 3

	flipped := sealRef(t)

	toTrue, err := p.Seal(ctx, Protocol, entity.SecretNTHash, owner, flipped, []byte("f"), Meta{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	toTrue.HandedOver = true

	unflipped := sealRef(t)

	toFalse, err := p.Seal(
		ctx,
		Protocol,
		entity.SecretNTHash,
		owner,
		unflipped,
		[]byte("g"),
		Meta{HandedOver: true},
	)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	toFalse.HandedOver = false

	cases := []struct {
		name string
		s    entity.Secret
		role Role
	}{
		{"another reference", moved(sealRef(t), entity.SecretNTHash, owner, 1), Protocol},
		{"another version", third, Protocol},
		{"another kind", moved(sealRef(t), entity.SecretKeytab, owner, 1), Protocol},
		{"another role", moved(sealRef(t), entity.SecretPassword, owner, 1), Storage},
		{"another owner", moved(sealRef(t), entity.SecretNTHash, other, 1), Protocol},
		{"handed-over set", toTrue, Protocol},
		{"handed-over cleared", toFalse, Protocol},
	}

	for _, c := range cases {
		resolveCommit(t, f.cp, c.s)

		before := one.unwraps

		value, err := p.Resolve(ctx, c.role, c.s.Kind, c.s.Owner, c.s.ID)
		if !errors.Is(err, errOpen) || value != nil {
			t.Errorf("%s: %q, %v; want errOpen", c.name, value, err)
		}

		if n := one.unwraps - before; n != 1 {
			t.Errorf("%s: %d unwraps, want 1", c.name, n)
		}
	}
}

func TestResolveCurrentAndMeta(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretMachinePassword, owner, ref, "v1", Meta{
		KeyVersion: 7,
		HandedOver: true,
	})

	resolvePut(t, f.p, f.cp, entity.SecretMachinePassword, owner, ref, "v2", Meta{KeyVersion: 8})

	got, err := f.p.Resolve(ctx, Protocol, entity.SecretMachinePassword, owner, ref)
	if err != nil || string(got) != "v2" {
		t.Errorf("Resolve after a rotation: %q, %v; want \"v2\"", got, err)
	}
}

func TestVersionsCurrentFirst(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)
	metas := []Meta{
		{KeyVersion: 1},
		{
			KeyVersion: 2,
			HandedOver: true,
		},
		{KeyVersion: 3},
	}

	for i, m := range metas {
		resolvePut(
			t,
			f.p,
			f.cp,
			entity.SecretMachinePassword,
			owner,
			ref,
			fmt.Sprintf("v%d", i+1),
			m,
		)
	}

	want := []Version{
		{Number: 3, Value: []byte("v3"), Meta: metas[2]},
		{Number: 2, Value: []byte("v2"), Meta: metas[1]},
		{Number: 1, Value: []byte("v1"), Meta: metas[0]},
	}

	got, err := f.p.Versions(ctx, Protocol, entity.SecretMachinePassword, owner, ref)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("three versions: %#v, %v; want %#v", got, err, want)
	}

	// A prune removes the lowest versions, so the first missing one ends
	// those still held; the test deletes the key itself.
	rawUpdate(t, f.db, func(tx kv.Txn) error {
		return tx.Delete(secretKVKey(ref, 1))
	})

	got, err = f.p.Versions(ctx, Protocol, entity.SecretMachinePassword, owner, ref)
	if err != nil || !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("after version 1 is pruned: %#v, %v; want %#v", got, err, want[:2])
	}

	single := sealRef(t)
	resolvePut(t, f.p, f.cp, entity.SecretKeytab, owner, single, "only", Meta{})

	got, err = f.p.Versions(ctx, Protocol, entity.SecretKeytab, owner, single)
	if err != nil || !reflect.DeepEqual(got, []Version{{Number: 1, Value: []byte("only")}}) {
		t.Errorf("one version: %#v, %v; want version 1 only", got, err)
	}
}

func TestResolveRetiredKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)

	const passphrase = "seal test passphrase" // sealKeys sets it

	dir := t.TempDir()
	open := func(file string, retired ...string) keyprovider.KeyProvider {
		kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{
			Kind:         keyprovider.KindLocal,
			File:         file,
			RetiredFiles: retired,
		})
		if err != nil {
			t.Fatalf("open %s: %v", file, err)
		}

		t.Cleanup(func() {
			_ = kp.Close()
		})

		return kp
	}

	var files []string

	for _, name := range []string{"a.key", "b.key"} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatalf("generate %s: %v", name, err)
		}

		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		files = append(files, path)
	}

	a := open(files[0])
	before := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{Protocol: a})
	resolvePut(t, before, f.cp, entity.SecretNTHash, owner, ref, "under a", Meta{})

	b := open(files[1], files[0])
	after := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{Protocol: b})

	got, err := after.Resolve(ctx, Protocol, entity.SecretNTHash, owner, ref)
	if err != nil || string(got) != "under a" {
		t.Errorf("a version under the retired key: %q, %v; want \"under a\"", got, err)
	}

	s := resolvePut(t, after, f.cp, entity.SecretNTHash, owner, ref, "under b", Meta{})
	if s.KeyID != b.CurrentMasterKeyID() || s.KeyID == a.CurrentMasterKeyID() {
		t.Errorf(
			"the new version's key ID %q, want the new key's %q",
			s.KeyID,
			b.CurrentMasterKeyID(),
		)
	}

	want := []Version{
		{Number: 2, Value: []byte("under b")},
		{Number: 1, Value: []byte("under a")},
	}

	list, err := after.Versions(ctx, Protocol, entity.SecretNTHash, owner, ref)
	if err != nil || !reflect.DeepEqual(list, want) {
		t.Errorf("both versions after the rotation: %#v, %v; want %#v", list, err, want)
	}
}

func TestResolveClearsDataKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)
	zero := make([]byte, dataKeySize)

	s := resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, ref, "v1", Meta{})

	got, err := f.p.Resolve(ctx, Protocol, entity.SecretNTHash, owner, ref)
	if err != nil || string(got) != "v1" {
		t.Fatalf("Resolve: %q, %v; want \"v1\"", got, err)
	}

	if len(f.protocol.kept) != 1 || !bytes.Equal(f.protocol.kept[0], zero) {
		t.Errorf(
			"the data key after a resolve: %x, want one key of %d zero bytes",
			f.protocol.kept,
			dataKeySize,
		)
	}

	movedRef := sealRef(t)
	resolveCommit(t, f.cp, entity.Secret{
		ID:      movedRef,
		Kind:    entity.SecretNTHash,
		Version: 1,
		KeyID:   s.KeyID,
		Owner:   owner,
		Value:   s.Value,
	})

	f.protocol.kept = nil

	if _, err := f.p.Resolve(ctx, Protocol, entity.SecretNTHash, owner, movedRef); !errors.Is(
		err,
		errOpen,
	) {
		t.Fatalf("Resolve of a moved envelope: %v, want errOpen", err)
	}

	if len(f.protocol.kept) != 1 || !bytes.Equal(f.protocol.kept[0], zero) {
		t.Errorf(
			"the data key after a failed open: %x, want one key of %d zero bytes",
			f.protocol.kept,
			dataKeySize,
		)
	}

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, ref, "v2", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, ref, "v3", Meta{})

	f.protocol.kept = nil

	if _, err := f.p.Versions(ctx, Protocol, entity.SecretNTHash, owner, ref); err != nil {
		t.Fatalf("Versions: %v", err)
	}

	if len(f.protocol.kept) != 3 {
		t.Fatalf("Versions of three versions: %d data keys, want 3", len(f.protocol.kept))
	}

	for i, k := range f.protocol.kept {
		if !bytes.Equal(k, zero) {
			t.Errorf("Versions' data key %d after use: %x, want %d zero bytes", i, k, dataKeySize)
		}
	}
}

func TestResolveErrorsCarryNoValue(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	other := sealOwner()
	other.Key = "password"

	nt := sealRef(t)
	s := resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, nt, canary, Meta{})

	storageOnly := sealProvider(t, f.cp, map[Role]keyprovider.KeyProvider{
		Storage: f.keys[Storage],
	})

	unknownKey := s
	unknownKey.ID = sealRef(t)
	unknownKey.KeyID = "no-such-key"
	resolveCommit(t, f.cp, unknownKey)

	moved := s
	moved.ID = sealRef(t)
	resolveCommit(t, f.cp, moved)

	malformed := s
	malformed.ID = sealRef(t)
	malformed.Value = []byte{1}
	resolveCommit(t, f.cp, malformed)

	// Versions whose current version opens and whose version 2 does not.
	walk := sealRef(t)
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, walk, "w1", Meta{})

	movedIn := s
	movedIn.ID, movedIn.Version = walk, 2
	resolveCommit(t, f.cp, movedIn)
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, walk, canary, Meta{})

	readFault := func() {
		f.db.SetFault(faultOn{
			op:     kvtest.OpBefore,
			prefix: "SEC‖",
		})
	}

	type call struct {
		p     *Provider
		role  Role
		kind  entity.SecretKind
		owner entity.SecretOwner
		ref   entity.SecretRef
	}

	cases := []struct {
		name  string
		c     call
		fault func()
		is    func(error) bool
	}{
		{
			"wrong role",
			call{f.p, Storage, entity.SecretNTHash, owner, nt},
			nil,
			isErr(ErrWrongRole),
		},
		{
			"undeclared kind",
			call{f.p, Protocol, firstUndeclaredKind(), owner, nt},
			nil,
			isErr(ErrWrongRole),
		},
		{
			"no key",
			call{storageOnly, Protocol, entity.SecretNTHash, owner, nt},
			nil,
			isErr(errNoRoleKey),
		},
		{
			"not found",
			call{f.p, Protocol, entity.SecretNTHash, owner, sealRef(t)},
			nil,
			merrs.IsNotFoundError,
		},
		{"kind", call{f.p, Protocol, entity.SecretKeytab, owner, nt}, nil, isErr(ErrWrongKind)},
		{"owner", call{f.p, Protocol, entity.SecretNTHash, other, nt}, nil, isErr(ErrWrongKind)},
		{
			"store read failed",
			call{f.p, Protocol, entity.SecretNTHash, owner, nt},
			readFault,
			isErr(errSealInjected),
		},
		{
			"unwrap failed",
			call{f.p, Protocol, entity.SecretNTHash, owner, unknownKey.ID},
			nil,
			isErr(keyprovider.ErrWrongMasterKey),
		},
		{
			"open failed",
			call{f.p, Protocol, entity.SecretNTHash, owner, moved.ID},
			nil,
			isErr(errOpen),
		},
		{
			"malformed envelope",
			call{f.p, Protocol, entity.SecretNTHash, owner, malformed.ID},
			nil,
			isErr(errEnvelope),
		},
	}

	for _, c := range cases {
		for _, versions := range []bool{false, true} {
			name := "Resolve, " + c.name
			if versions {
				name = "Versions, " + c.name
			}

			if c.fault != nil {
				c.fault()
			}

			var (
				got any
				err error
			)

			if versions {
				list, e := c.c.p.Versions(ctx, c.c.role, c.c.kind, c.c.owner, c.c.ref)
				got, err = list, e

				if list != nil {
					t.Errorf("%s: got %v, want a nil list", name, list)
				}
			} else {
				value, e := c.c.p.Resolve(ctx, c.c.role, c.c.kind, c.c.owner, c.c.ref)
				got, err = value, e

				if value != nil {
					t.Errorf("%s: got %q, want a nil value", name, value)
				}
			}

			f.db.SetFault(f.rec)

			if !c.is(err) {
				t.Errorf("%s: got %v (%v), want its own error", name, err, got)
			}

			resolveNoCanary(t, name, err)
		}
	}

	list, err := f.p.Versions(ctx, Protocol, entity.SecretNTHash, owner, walk)
	if !errors.Is(err, errOpen) || list != nil {
		t.Errorf("Versions over a moved version 2: %v, %v; want errOpen and a nil list", list, err)
	}

	resolveNoCanary(t, "Versions over a moved version 2", err)

	f.db.SetFault(faultOn{
		op:     kvtest.OpGet,
		prefix: "SEC‖",
	})

	list, err = f.p.Versions(ctx, Protocol, entity.SecretNTHash, owner, walk)
	f.db.SetFault(f.rec)

	if !errors.Is(err, errSealInjected) || merrs.IsNotFoundError(err) || list != nil {
		t.Errorf(
			"Versions whose version 2 read fails: %v, %v; want the injected error and a nil list",
			list,
			err,
		)
	}

	resolveNoCanary(t, "Versions whose version 2 read fails", err)
}

func isErr(target error) func(error) bool {
	return func(err error) bool {
		return errors.Is(err, target)
	}
}

// resolveNoCanary fails if err is nil or carries the canary in any form.
func resolveNoCanary(t *testing.T, name string, err error) {
	t.Helper()

	if err == nil {
		t.Errorf("%s: got nil, want a refusal", name)

		return
	}

	forms := []string{
		canary,
		hex.EncodeToString([]byte(canary)),
		base64.StdEncoding.EncodeToString([]byte(canary)),
	}

	for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
		for _, form := range forms {
			if strings.Contains(text, form) {
				t.Errorf("%s: the error %q carries the value", name, text)
			}
		}
	}
}

func TestResolveOneRead(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ctx := context.Background()
	f := newResolveFixture(t)
	owner := sealOwner()
	ref := sealRef(t)

	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, ref, "v1", Meta{})
	resolvePut(t, f.p, f.cp, entity.SecretNTHash, owner, ref, "v2", Meta{})

	f.rec.reset()
	f.protocol.unwraps = 0

	if _, err := f.p.Resolve(ctx, Protocol, entity.SecretNTHash, owner, ref); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ops := f.rec.taken()
	if len(ops) != 2 || ops[0].op != kvtest.OpView || ops[1].op != kvtest.OpBefore ||
		!strings.HasPrefix(ops[1].key, "SEC‖") {
		t.Errorf("store ops of one Resolve: %v, want one view and one before on SEC", ops)
	}

	if f.protocol.unwraps != 1 {
		t.Errorf("unwraps of one Resolve: %d, want 1", f.protocol.unwraps)
	}
}
