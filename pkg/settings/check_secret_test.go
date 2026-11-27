package settings_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// Sections with a reference field, shaped as the tree's real ones are: an
// installation directory with a bind credential, and an installation record
// a node may override. remoteCredential, the store-scope field of the gates
// tests, is reused below as it is.
type (
	dirSettings struct {
		valid
		BindName       string           `json:"bind_name"       class:"live"`
		BindCredential entity.SecretRef `json:"bind_credential" class:"live"`
	}
	overCredSettings struct {
		valid
		Credential entity.SecretRef `json:"credential" class:"live"`
	}
)

// The scopes the secret tests write at.
var (
	storeB = entity.SettingScope{Kind: entity.ScopeRemote, ID: [16]byte{0x5b, 15: 1}}
	storeC = entity.SettingScope{Kind: entity.ScopeRemote, ID: [16]byte{0x5c, 15: 2}}
	userID = entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{0x5d, 15: 3}}
)

// dirTable is the directory whose bind credential the tests point at other
// holders' secrets.
func dirTable() settings.Table {
	return settings.Table{settings.Of(
		"identity.directory",
		settings.At(entity.ScopeInstallation),
		dirSettings{},
		settings.SecretRef("BindCredential", entity.SecretDirectoryCredential),
	)}
}

// tokenTable declares a string field as a directory credential, whose
// plaintext reaches neither a Validate nor a refusal.
func tokenTable() settings.Table {
	return settings.Table{settings.Of(
		"test.token",
		settings.At(entity.ScopeInstallation),
		tokenSettings{},
		settings.SecretRef("Token", entity.SecretDirectoryCredential),
	)}
}

// credKeys writes one real key file per role in a temporary directory and
// opens both, closed when the test ends.
func credKeys(t *testing.T) map[secrets.Role]keyprovider.KeyProvider {
	t.Helper()

	const passphrase = "secret test passphrase"

	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	dir := t.TempDir()
	out := map[secrets.Role]keyprovider.KeyProvider{}

	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatalf("generate the %s key file: %v", role, err)
		}

		path := filepath.Join(dir, role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write the %s key file: %v", role, err)
		}

		kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{
			Kind: keyprovider.KindLocal,
			File: path,
		})
		if err != nil {
			t.Fatalf("open the %s key file: %v", role, err)
		}

		t.Cleanup(func() { _ = kp.Close() })

		out[role] = kp
	}

	return out
}

// sealer seals real secrets over s's own store.
func (s store) sealer(t *testing.T) *secrets.Provider {
	t.Helper()

	p, err := secrets.New(s.cp, credKeys(t))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// refJSON is the reference row's value: the JSON list of the ref's 16 bytes.
func refJSON(ref entity.SecretRef) string {
	b, err := json.Marshal(ref)
	if err != nil {
		panic(err)
	}

	return string(b)
}

// seal seals value as a fresh secret for kind and owner under role, commits
// it as its first version through the store's own write, and returns the
// reference row's value.
func (s store) seal(
	t *testing.T,
	p *secrets.Provider,
	role secrets.Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	value string,
) string {
	t.Helper()

	sealed, err := p.Seal(
		context.Background(),
		role,
		kind,
		owner,
		entity.SecretRef(entity.NewShareID()),
		[]byte(value),
		secrets.Meta{},
	)
	if err != nil {
		t.Fatalf("seal a %s: %v", kind, err)
	}

	err = s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(sealed, sealed.Version-1)
	})
	if err != nil {
		t.Fatalf("commit the sealed %s: %v", kind, err)
	}

	return refJSON(sealed.ID)
}

// sealNext seals at the ref an earlier seal returned, as a new version, and
// commits it through the store's own write.
func (s store) sealNext(
	t *testing.T,
	p *secrets.Provider,
	role secrets.Role,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	value string,
	sealed entity.SecretRef,
) {
	t.Helper()

	next, err := p.Seal(
		context.Background(),
		role,
		kind,
		owner,
		sealed,
		[]byte(value),
		secrets.Meta{},
	)
	if err != nil {
		t.Fatalf("seal the next %s: %v", kind, err)
	}

	err = s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(next, next.Version-1)
	})
	if err != nil {
		t.Fatalf("commit the next sealed %s: %v", kind, err)
	}
}

// refOf parses the reference row's value back into its ref, for a fixture
// that seals first and writes the row after.
func refOf(t *testing.T, value string) entity.SecretRef {
	t.Helper()

	var ref entity.SecretRef
	if err := json.Unmarshal([]byte(value), &ref); err != nil {
		t.Fatalf("parse the reference value %s: %v", value, err)
	}

	return ref
}

// secretRow rewrites the secret's current version row straight to the
// Badger, as a concurrent sealer would, with no settings generation raised.
func (s store) secretRow(t *testing.T, ref entity.SecretRef, version uint64, b []byte) {
	t.Helper()

	key := keys.ID([]byte{keys.KindSEC}, ref[:])

	err := s.db.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(keys.Uint64(key, version), b)
	})
	if err != nil {
		t.Fatalf("write the secret's row: %v", err)
	}
}

// A bind credential naming a secret of another kind is refused naming the
// kind it wants, and one naming its own holder's secret of the declared
// kind passes and stores.
func TestSecretRefAnotherKindRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := dirTable()

	oidc := s.seal(t, p, secrets.Storage, entity.SecretKey,
		entity.SecretOwner{Scope: installation, Key: "identity.oidc.client_secret"},
		"MARKER-OIDC")

	err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
		"identity.directory.bind_credential", oidc)))
	refused(t, "the client secret", err, settings.ErrSecretRef,
		"setting (installation): identity.directory.bind_credential: "+
			"names a secret of another kind: key, not directory credential")

	if strings.Contains(err.Error(), "MARKER-OIDC") || strings.Contains(err.Error(), "oidc") {
		t.Errorf("the refusal prints the secret: %q", err.Error())
	}

	own := s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential,
		entity.SecretOwner{Scope: installation, Key: "identity.directory.bind_credential"},
		"MARKER-PASSWORD")

	_, err = s.attemptPut(tb, entity.RoleOperator, setting(installation,
		"identity.directory.bind_credential", own), holdsContent, nil)
	passed(t, "the directory's own secret", err)

	if got, want := rowsStored(t, s, installation),
		map[string]string{"identity.directory.bind_credential": own}; !maps.Equal(got, want) {
		t.Errorf("the passed reference: rows = %v, want %v", got, want)
	}
}

// A bind credential naming a same-kind secret owned by another record or
// another setting key is refused, and the other owner is never named.
func TestSecretRefAnotherOwnerRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := dirTable()

	user := entity.SecretOwner{Record: userID, Key: "bind_credential"}
	otherKey := entity.SecretOwner{Scope: installation, Key: "identity.legacy.bind_credential"}

	for name, owner := range map[string]entity.SecretOwner{
		"a user record":       user,
		"another setting key": otherKey,
	} {
		err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
			"identity.directory.bind_credential",
			s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, owner, "x"))))
		refused(t, name, err, settings.ErrSecretRef,
			"setting (installation): identity.directory.bind_credential: "+
				"names a secret of another owner")

		if strings.Contains(err.Error(), hex.EncodeToString(userID.ID)) ||
			strings.Contains(err.Error(), "identity.legacy") {
			t.Errorf("%s: the refusal names the other owner: %q", name, err.Error())
		}
	}
}

// A reference naming no secret at all is refused: a value that never sealed,
// the zero ref, and a plaintext typed into a string field declared a
// reference. No refusal echoes the value.
func TestSecretRefNoSecretRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := dirTable()

	never := refJSON(entity.SecretRef(entity.NewShareID()))

	for name, value := range map[string]string{
		"a never-sealed ref": never,
		"the zero ref":       "[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]",
	} {
		err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
			"identity.directory.bind_credential", value)))
		refused(t, name, err, settings.ErrSecretRef,
			"setting (installation): identity.directory.bind_credential: "+
				"names no secret of kind directory credential")
	}

	err := s.check(tokenTable(), entity.RoleOperator, settings.Put(setting(installation,
		"test.token.token", `"MARKER-PLAINTEXT"`)))
	refused(t, "a plaintext into a string field", err, settings.ErrSecretRef,
		"setting (installation): test.token.token: "+
			"names no secret of kind directory credential")

	if strings.Contains(err.Error(), "MARKER-PLAINTEXT") {
		t.Errorf("the refusal prints the plaintext: %q", err.Error())
	}
}

// A stored reference that names no secret is repaired through the gate
// itself: a write of its own key that names a well-owned secret passes, and
// so does its delete, whose removal never leaves a holder behind, while a
// write of another key of the section stays refused, since the dangling row
// is still what the section holds effective.
func TestSecretRefDanglingRepairedOrCleared(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := dirTable()

	never := refJSON(entity.SecretRef(entity.NewShareID()))
	s.put(t, installation, "identity.directory.bind_credential", never)

	err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
		"identity.directory.bind_name", `"cn=dir"`)))
	refused(t, "a plain field over the dangling row", err, settings.ErrSecretRef,
		"setting (installation): identity.directory.bind_credential: "+
			"names no secret of kind directory credential")

	passed(t, "the delete of the dangling row",
		s.check(tb, entity.RoleOperator, settings.Delete(installation,
			"identity.directory.bind_credential")))

	own := s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential,
		entity.SecretOwner{Scope: installation, Key: "identity.directory.bind_credential"},
		"MARKER-PASSWORD")

	passed(t, "the repair naming a well-owned secret",
		s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
			"identity.directory.bind_credential", own))))

	err = s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
		"identity.directory.bind_name", `"cn=dir"`)))
	refused(t, "a plain field after the checks, the stored row still dangling", err,
		settings.ErrSecretRef,
		"setting (installation): identity.directory.bind_credential: "+
			"names no secret of kind directory credential")
}

// A node override of an Override section is judged by its own row, never by
// the wider one below it: naming the installation row's secret is refused
// with the owner rule, naming its own passes.
func TestSecretRefNodeOverrideOwner(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := settings.Table{settings.Of(
		"identity.bind",
		settings.Override(entity.ScopeInstallation, entity.ScopeNode),
		overCredSettings{},
		settings.SecretRef("Credential", entity.SecretDirectoryCredential),
	)}

	wide := entity.SecretOwner{Scope: installation, Key: "identity.bind.credential"}
	node := entity.SecretOwner{Scope: nodeA, Key: "identity.bind.credential"}
	wideRef := s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, wide, "wide")
	s.put(t, installation, "identity.bind.credential", wideRef)

	err := s.check(tb, entity.RoleOperator, settings.Put(setting(nodeA,
		"identity.bind.credential", wideRef)))
	refused(t, "node naming the installation's secret", err, settings.ErrSecretRef,
		"setting (node "+hex.EncodeToString(nodeA.ID[:])+"): identity.bind.credential: "+
			"names a secret of another owner")

	passed(t, "node naming its own secret", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "identity.bind.credential",
			s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, node, "mine")))))
}

// A store's first credential value passes whole, since the store location
// gate lets a first value through, and the same field repointed at a
// freshly sealed secret is refused as unproven, not by the reference gate.
func TestSecretRefFirstValueAndRepoint(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := settings.Table{settings.Of(
		"test.store",
		settings.At(entity.ScopeRemote),
		remoteCredential{},
		settings.Required("Credential"),
		settings.SecretRef("Credential", entity.SecretRemoteCredential),
	)}

	_, err := s.attemptPut(tb, entity.RoleOperator, setting(storeB,
		"test.store.credential", s.seal(t, p, secrets.Storage,
			entity.SecretRemoteCredential,
			entity.SecretOwner{Scope: storeB, Key: "test.store.credential"}, "first")),
		holdsContent, nil)
	passed(t, "the first credential value", err)

	next := s.seal(t, p, secrets.Storage, entity.SecretRemoteCredential,
		entity.SecretOwner{Scope: storeB, Key: "test.store.credential"}, "second")

	err = s.check(tb, entity.RoleOperator, settings.Put(setting(storeB,
		"test.store.credential", next)))
	refused(t, "the repointed credential", err, settings.ErrUnproven, "")

	if errors.Is(err, settings.ErrSecretRef) {
		t.Errorf("the repoint is refused by the reference gate: %q", err.Error())
	}
}

// A new store's first credential naming another store's secret passes the
// store location gate, whose first value is no change, and is refused by the
// owner check alone.
func TestSecretRefAnotherStoreCredential(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := settings.Table{settings.Of(
		"test.store",
		settings.At(entity.ScopeRemote),
		remoteCredential{},
		settings.Required("Credential"),
		settings.SecretRef("Credential", entity.SecretRemoteCredential),
	)}

	ref := entity.SecretRef(entity.NewShareID())

	sealed, err := p.Seal(
		context.Background(),
		secrets.Storage,
		entity.SecretRemoteCredential,
		entity.SecretOwner{
			Scope: storeB,
			Key:   "test.store.credential",
		},
		ref,
		[]byte("mine"),
		secrets.Meta{},
	)
	if err != nil {
		t.Fatalf("seal store B's credential: %v", err)
	}

	if err := s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(sealed, sealed.Version-1)
	}); err != nil {
		t.Fatalf("commit store B's credential: %v", err)
	}

	err = s.check(tb, entity.RoleOperator, settings.Put(setting(storeC,
		"test.store.credential", refJSON(ref))))
	refused(t, "store C naming store B's secret", err, settings.ErrSecretRef,
		"setting (remote "+hex.EncodeToString(storeC.ID[:])+"): test.store.credential: "+
			"names a secret of another owner")

	if strings.Contains(err.Error(), refJSON(ref)) ||
		strings.Contains(err.Error(), hex.EncodeToString(ref[:])) {
		t.Errorf("the refusal prints the reference: %q", err.Error())
	}
}

// A delete is judged against the reference it leaves effective: a poisoned
// wider row below the deleted one refuses the delete, a valid wider reference
// does not, and deleting the last row leaves no reference to judge.
func TestSecretRefDeleteComparesEffective(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := settings.Table{settings.Of(
		"identity.bind",
		settings.Override(entity.ScopeInstallation, entity.ScopeNode),
		overCredSettings{},
		settings.SecretRef("Credential", entity.SecretDirectoryCredential),
	)}

	node := entity.SecretOwner{Scope: nodeA, Key: "identity.bind.credential"}
	wide := entity.SecretOwner{Scope: installation, Key: "identity.bind.credential"}
	user := entity.SecretOwner{Record: userID, Key: "bind_credential"}

	s.put(t, installation, "identity.bind.credential",
		s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, user, "poison"))
	s.put(t, nodeA, "identity.bind.credential",
		s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, node, "mine"))

	err := s.check(tb, entity.RoleOperator, settings.Delete(nodeA, "identity.bind.credential"))
	refused(t, "the delete over a poisoned wider row", err, settings.ErrSecretRef,
		"setting (installation): identity.bind.credential: "+
			"names a secret of another owner")

	s.put(t, installation, "identity.bind.credential",
		s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, wide, "wide"))

	passed(t, "the delete over a valid wider row",
		s.check(tb, entity.RoleOperator, settings.Delete(nodeA, "identity.bind.credential")))
	passed(t, "the installation row's own delete",
		s.check(tb, entity.RoleOperator, settings.Delete(installation, "identity.bind.credential")))
}

// The gate judges every reference the section holds effective, not only the
// written field's own: a poisoned reference row beside a plain field's write
// refuses the write, naming the reference field.
func TestSecretRefJudgesWholeSection(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := dirTable()

	other := entity.SecretOwner{Scope: storeB, Key: "test.store.credential"}
	s.put(t, installation, "identity.directory.bind_credential",
		s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential, other, "poison"))

	_, err := s.attemptPut(tb, entity.RoleOperator, setting(installation,
		"identity.directory.bind_name", `"cn=dir"`), holdsContent, nil)
	refused(t, "the plain field beside a poisoned reference", err, settings.ErrSecretRef,
		"setting (installation): identity.directory.bind_credential: "+
			"names a secret of another owner")
}

// A store error while the gate reads the secret comes back wrapped, is never
// a refusal, and no row byte is echoed.
func TestSecretRefStoreErrorWrapped(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := dirTable()

	ref := entity.SecretRef(entity.NewShareID())
	s.secretRow(t, ref, 1, []byte{201, 202, 203})

	err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
		"identity.directory.bind_credential", refJSON(ref))))
	if err == nil {
		t.Fatal("err = nil, want the store's error")
	}

	if errors.Is(err, settings.ErrSecretRef) {
		t.Errorf("the store error becomes a reference refusal: %q", err.Error())
	}

	wrap := "settings: read the secret identity.directory.bind_credential names"
	if !strings.Contains(err.Error(), wrap) {
		t.Errorf("err = %q, want the wrap %q", err.Error(), wrap)
	}

	if strings.Contains(err.Error(), string([]byte{201, 202, 203})) {
		t.Errorf("the wrap echoes the row's bytes: %q", err.Error())
	}
}

// The gate's read of the secret is tracked: a secret version written straight
// to the Badger between the check and the commit makes the write retry, and
// another secret's row does not.
func TestSecretRefTrackedReader(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	p := s.sealer(t)
	tb := dirTable()

	value := s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential,
		entity.SecretOwner{Scope: installation, Key: "identity.directory.bind_credential"}, "first")
	ref := refOf(t, value)
	s.sealNext(
		t,
		p,
		secrets.Protocol,
		entity.SecretDirectoryCredential,
		entity.SecretOwner{
			Scope: installation,
			Key:   "identity.directory.bind_credential",
		},
		"second",
		ref,
	)

	other := entity.SecretRef(entity.NewShareID())

	otherSealed, err := p.Seal(context.Background(), secrets.Protocol,
		entity.SecretDirectoryCredential,
		entity.SecretOwner{Scope: installation, Key: "identity.directory.bind_credential"},
		other, []byte("x"), secrets.Meta{})
	if err != nil {
		t.Fatalf("seal the other secret: %v", err)
	}

	if err := s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(otherSealed, otherSealed.Version-1)
	}); err != nil {
		t.Fatalf("commit the other secret: %v", err)
	}

	row := func(ref entity.SecretRef, version uint64) []byte {
		key := keys.ID([]byte{keys.KindSEC}, ref[:])
		key = keys.Uint64(key, version)

		var b []byte

		err := s.db.View(ctx, func(tx kv.Reader) error {
			v, err := tx.Get(key)
			b = v

			return err
		})
		if err != nil {
			t.Fatalf("read the secret's row: %v", err)
		}

		return b
	}

	got := s.attempts(
		t,
		tb,
		setting(installation, "identity.directory.bind_credential", value),
		func() {
			s.secretRow(t, ref, 2, row(ref, 2))
		},
	)
	if got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}

	got = s.attempts(
		t,
		tb,
		setting(installation, "identity.directory.bind_credential", value),
		func() {
			s.secretRow(t, other, 1, row(other, 1))
		},
	)
	if got != 1 {
		t.Errorf("attempts over another secret = %d, want 1", got)
	}
}

// The gate's reads are point reads: the section's own rows as the write
// check already reads them, plus one guarded range and one Before read of the
// secret per reference, and never a scan. A section without a reference
// field adds none.
func TestSecretRefPointReadCount(t *testing.T) {
	prof.Watch(t, prof.Options{})

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	counting := &kv.Counting{KV: db}
	s := openOver(t, counting)
	p := s.sealer(t)
	tb := dirTable()

	value := s.seal(t, p, secrets.Protocol, entity.SecretDirectoryCredential,
		entity.SecretOwner{Scope: installation, Key: "identity.directory.bind_credential"}, "mine")

	counting.Take()

	passed(t, "the reference put", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "identity.directory.bind_credential", value))))

	got := counting.Take()

	if got.Gets != 2 || got.RangeGuards != 1 || got.Befores != 1 || got.Scanned != 0 {
		t.Errorf("gets %d, range guards %d, befores %d, scanned %d; want 2, 1, 1, 0",
			got.Gets, got.RangeGuards, got.Befores, got.Scanned)
	}

	sfree := table()

	counting.Take()

	passed(t, "the plain put", s.check(sfree, entity.RoleOperator,
		settings.Put(setting(installation, "identity.mode", `"ldapy"`))))

	got = counting.Take()

	if got.RangeGuards != 0 || got.Befores != 0 || got.Scanned != 0 {
		t.Errorf("range guards %d, befores %d, scanned %d over a secret-free section; want 0, 0, 0",
			got.RangeGuards, got.Befores, got.Scanned)
	}
}
