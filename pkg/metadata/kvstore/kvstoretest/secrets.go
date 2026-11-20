package kvstoretest

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// secretOf is version v of a user's password hash, owned by user under
// "password", sealed bytes standing in for a value.
func secretOf(id entity.SecretRef, user entity.PrincipalID, v uint64, value string) entity.Secret {
	return entity.Secret{
		ID:      id,
		Kind:    entity.SecretPassword,
		Version: v,
		KeyID:   "protocol-1",
		Owner: entity.SecretOwner{
			Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(user[:])},
			Key:    "password",
		},
		Value: []byte(value),
	}
}

// secretInvalid reports whether err is the store's invalid-argument refusal.
func secretInvalid(err error) bool {
	var se *merrs.StoreError

	return errors.As(err, &se) && se.Code == merrs.ErrInvalidArgument
}

func newSecretRef() entity.SecretRef {
	return entity.SecretRef(uuid.New())
}

// secretValue is the value of version v of id, failing the test if it
// cannot be read.
func secretValue(t *testing.T, cp kvstore.ControlPlane, id entity.SecretRef, v uint64) string {
	t.Helper()

	s, err := cp.Secret(ctx, id, v)
	must(t, err)

	return string(s.Value)
}

func settingsCounter(t *testing.T, s *kvstore.Store) uint64 {
	t.Helper()

	return counters(t, s).Settings
}

// Version 0 reads the current version, a version by its number reads that
// one with every field as put, and an absent version or reference is not
// found.
func testSecretReadsByVersion(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
	first := secretOf(id, owner, 1, "one")
	second := secretOf(id, owner, 2, "two")
	second.KeyID, second.KeyVersion, second.HandedOver = "protocol-2", math.MaxUint32, true

	must(t, cp.PutSecret(ctx, first, 0))
	must(t, cp.PutSecret(ctx, second, 1))

	got, err := cp.Secret(ctx, id, 0)
	must(t, err)

	if !sameSecret(got, second) {
		t.Errorf("Secret(id, 0) = %+v, want %+v", got, second)
	}

	got, err = cp.Secret(ctx, id, 1)
	must(t, err)

	if !sameSecret(got, first) {
		t.Errorf("Secret(id, 1) = %+v, want %+v", got, first)
	}

	for _, c := range []struct {
		name string
		id   entity.SecretRef
		v    uint64
	}{
		{"a version above the current", id, 3},
		{"another reference", newSecretRef(), 0},
	} {
		if _, err := cp.Secret(ctx, c.id, c.v); !merrs.IsNotFoundError(err) {
			t.Errorf("%s: Secret = %v, want not found", c.name, err)
		}
	}
}

func sameSecret(a, b entity.Secret) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Version == b.Version && a.KeyID == b.KeyID &&
		a.Owner.Equal(b.Owner) && bytes.Equal(a.Value, b.Value) && a.KeyVersion == b.KeyVersion &&
		a.HandedOver == b.HandedOver
}

// A put names the current version it read: a second first version, or a
// rotation sealed against a version already replaced, is refused and
// overwrites nothing.
func testSecretPutExpect(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	id, owner := newSecretRef(), entity.PrincipalID(uuid.New())

	must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "first"), 0))

	if err := cp.PutSecret(ctx, secretOf(id, owner, 1, "again"), 0); !errors.Is(
		err,
		entity.ErrGenerationMismatch,
	) {
		t.Errorf("a second first version: got %v, want ErrGenerationMismatch", err)
	}

	if got := secretValue(t, cp, id, 0); got != "first" {
		t.Errorf("current after a second first version: got %q, want %q", got, "first")
	}

	must(t, cp.PutSecret(ctx, secretOf(id, owner, 2, "rotated"), 1))

	if err := cp.PutSecret(ctx, secretOf(id, owner, 2, "stale"), 1); !errors.Is(
		err,
		entity.ErrGenerationMismatch,
	) {
		t.Errorf("a rotation sealed against version 1: got %v, want ErrGenerationMismatch", err)
	}

	if got := secretValue(t, cp, id, 2); got != "rotated" {
		t.Errorf("version 2 after a stale rotation: got %q, want %q", got, "rotated")
	}
}

// Two rotations sealed against the same version, the second committing
// inside the first's transaction: the second passes and the first is
// refused, so neither is lost silently.
func testSecretPutRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
			must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "first"), 0))

			outer, inner := race(h,
				func() error {
					return cp.PutSecret(ctx, secretOf(id, owner, 2, "outer"), 1)
				},
				func() error {
					return cp.PutSecret(ctx, secretOf(id, owner, 2, "inner"), 1)
				})

			must(t, inner)

			if !errors.Is(outer, entity.ErrGenerationMismatch) {
				t.Errorf(
					"the rotation that committed second: got %v, want ErrGenerationMismatch",
					outer,
				)
			}

			if got := secretValue(t, cp, id, 0); got != "inner" {
				t.Errorf("current after the race: got %q, want %q", got, "inner")
			}
		})
	}
}

// A put whose version is not the one after the current, or with no
// reference or no declared kind, is refused, writes nothing and raises
// nothing.
func testSecretVersionNumber(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
	must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "first"), 0))
	g := settingsCounter(t, s)

	for _, c := range []struct {
		name   string
		s      entity.Secret
		expect uint64
	}{
		{"version 3 over version 1", secretOf(id, owner, 3, "bad"), 1},
		{"version 1 over version 1", secretOf(id, owner, 1, "bad"), 1},
		{"version 0", secretOf(id, owner, 0, "bad"), 1},
		{"version 0 after the largest expect", secretOf(id, owner, 0, "bad"), math.MaxUint64},
		{"the zero reference", secretOf(entity.SecretRef{}, owner, 1, "bad"), 0},
	} {
		if err := cp.PutSecret(ctx, c.s, c.expect); !secretInvalid(err) {
			t.Errorf("%s: got %v, want an invalid argument", c.name, err)
		}
	}

	// An undeclared kind is put as a first version of a fresh reference, so
	// no current version's kind can refuse it in its place.
	for _, k := range []entity.SecretKind{0, 200} {
		fresh := newSecretRef()
		sec := secretOf(fresh, owner, 1, "bad")

		sec.Kind = k
		if err := cp.PutSecret(ctx, sec, 0); !secretInvalid(err) {
			t.Errorf("kind %d: got %v, want an invalid argument", k, err)
		}

		if _, err := cp.Secret(ctx, fresh, 0); !merrs.IsNotFoundError(err) {
			t.Errorf("kind %d: Secret after the refused put: got %v, want not found", k, err)
		}
	}

	got, err := cp.Secret(ctx, id, 0)
	must(t, err)

	if got.Version != 1 || string(got.Value) != "first" {
		t.Errorf(
			"current after refused puts: version %d value %q, want 1 %q",
			got.Version,
			got.Value,
			"first",
		)
	}

	if got := settingsCounter(t, s); got != g {
		t.Errorf("settings generation after refused puts: got %d, want %d", got, g)
	}
}

// A new version keeps its kind and its owner, every part of it: a version
// naming another is refused, so a reference cannot be moved to another
// holder.
func testSecretKeepsKindAndOwner(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	userID := entity.PrincipalID(uuid.New())
	pw := newSecretRef()
	must(t, cp.PutSecret(ctx, secretOf(pw, userID, 1, "first"), 0))

	keytab := newSecretRef()
	scope := entity.SettingScope{Kind: entity.ScopeShare, ID: uuid.New()}
	byScope := func(v uint64, sc entity.SettingScope) entity.Secret {
		sec := secretOf(keytab, userID, v, "keytab")
		sec.Kind = entity.SecretKeytab
		sec.Owner = entity.SecretOwner{Scope: sc, Key: "identity.keytab"}

		return sec
	}
	must(t, cp.PutSecret(ctx, byScope(1, scope), 0))

	userOwner := func(kind entity.ResourceKind, id []byte, key string) entity.SecretOwner {
		return entity.SecretOwner{Record: entity.ResourceRef{Kind: kind, ID: id}, Key: key}
	}
	moved := func(kind entity.SecretKind, owner entity.SecretOwner) entity.Secret {
		sec := secretOf(pw, userID, 2, "moved")
		sec.Kind, sec.Owner = kind, owner

		return sec
	}
	otherID := userID
	otherID[15]++

	user, group, pass := entity.ResourceUser, entity.ResourceGroup, entity.SecretPassword
	share, ns := entity.ScopeShare, entity.ScopeNamespace

	for _, c := range []struct {
		name string
		s    entity.Secret
	}{
		{"another kind", moved(entity.SecretNTHash, userOwner(user, userID[:], "password"))},
		{"a record ID one byte off", moved(pass, userOwner(user, otherID[:], "password"))},
		{"another record kind", moved(pass, userOwner(group, userID[:], "password"))},
		{"another key", moved(pass, userOwner(user, userID[:], "nt_hash"))},
		{"a setting owner", moved(pass, entity.SecretOwner{Scope: scope, Key: "password"})},
		{"another scope ID", byScope(2, entity.SettingScope{Kind: share, ID: uuid.New()})},
		{"another scope kind", byScope(2, entity.SettingScope{Kind: ns, ID: scope.ID})},
	} {
		if err := cp.PutSecret(ctx, c.s, 1); !secretInvalid(err) {
			t.Errorf("%s: got %v, want an invalid argument", c.name, err)
		}
	}

	for _, id := range []entity.SecretRef{pw, keytab} {
		if got, err := cp.Secret(ctx, id, 0); err != nil || got.Version != 1 {
			t.Errorf("current after refused moves: version %d, %v, want 1", got.Version, err)
		}
	}

	must(t, cp.PutSecret(ctx, moved(pass, userOwner(user, bytes.Clone(userID[:]), "password")), 1))
}

// Each put raises the settings generation by exactly one, and a refused one
// raises nothing.
func testSecretRaisesSettingsGeneration(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
	g := settingsCounter(t, s)

	must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "one"), 0))

	if got := settingsCounter(t, s); got != g+1 {
		t.Errorf("after the first put: got %d, want %d", got, g+1)
	}

	must(t, cp.PutSecret(ctx, secretOf(id, owner, 2, "two"), 1))

	if got := settingsCounter(t, s); got != g+2 {
		t.Errorf("after the second put: got %d, want %d", got, g+2)
	}

	if err := cp.PutSecret(ctx, secretOf(id, owner, 2, "stale"), 1); err == nil {
		t.Fatal("a stale put passed")
	}

	if got := settingsCounter(t, s); got != g+2 {
		t.Errorf("after a refused put: got %d, want %d", got, g+2)
	}
}

// failSet fails every set of a key whose description starts with prefix.
type failSet struct {
	prefix string
}

var errSecretInjected = errors.New("kvstoretest: injected")

func (f failSet) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpSet && strings.HasPrefix(string(key), f.prefix) {
		return 0, 0, errSecretInjected
	}

	return 0, 0, nil
}

// The version and the settings generation's raise are one transaction: a
// failure of either write leaves neither.
func testSecretPutIsOneTransaction(t *testing.T, kvo Opener) {
	for _, prefix := range []string{"SEC‖", "CFGGEN"} {
		t.Run(prefix, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			f := kvtest.NewFaultKV(kvo(t))
			must(t, kvstore.Init(ctx, f, uuid.New()))
			s, err := kvstore.Open(ctx, f)
			must(t, err)

			cp := s.ControlPlane()
			id := newSecretRef()
			g := settingsCounter(t, s)

			f.SetFault(failSet{prefix: prefix})

			err = cp.PutSecret(ctx, secretOf(id, entity.PrincipalID(uuid.New()), 1, "one"), 0)

			f.SetFault(nil)

			if !errors.Is(err, errSecretInjected) {
				t.Errorf(
					"put with a failed set of %s: got %v, want the injected error",
					prefix,
					err,
				)
			}

			if _, err := cp.Secret(ctx, id, 0); !merrs.IsNotFoundError(err) {
				t.Errorf("Secret after the failed put: got %v, want not found", err)
			}

			if got := settingsCounter(t, s); got != g {
				t.Errorf("settings generation after the failed put: got %d, want %d", got, g)
			}
		})
	}
}

// The transaction form writes inside the caller's Write: a Write that fails
// after it leaves no version and no raise.
func testSecretTxPutRollsBack(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	id := newSecretRef()
	g := settingsCounter(t, s)

	err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		sec := secretOf(id, entity.PrincipalID(uuid.New()), 1, "one")
		if err := tx.PutSecret(sec, 0); err != nil {
			return err
		}

		return errSecretInjected
	})
	if !errors.Is(err, errSecretInjected) {
		t.Fatalf("Write: got %v, want the injected error", err)
	}

	if _, err := cp.Secret(ctx, id, 0); !merrs.IsNotFoundError(err) {
		t.Errorf("Secret after the failed Write: got %v, want not found", err)
	}

	if got := settingsCounter(t, s); got != g {
		t.Errorf("settings generation after the failed Write: got %d, want %d", got, g)
	}
}

// A refused put raises nothing, even inside a Write that goes on and
// commits: the refusal comes before any write.
func testSecretRefusedTxPutRaisesNothing(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	g := settingsCounter(t, s)

	err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		sec := secretOf(entity.SecretRef{}, entity.PrincipalID(uuid.New()), 1, "bad")
		if err := tx.PutSecret(sec, 0); !secretInvalid(err) {
			t.Errorf("put of the zero reference: got %v, want an invalid argument", err)
		}

		return nil
	})
	must(t, err)

	if got := settingsCounter(t, s); got != g {
		t.Errorf(
			"settings generation after a refused put in a committed Write: got %d, want %d",
			got,
			g,
		)
	}
}

// A secret read through the transaction is tracked: a new version that
// commits first makes the write run again, and the attempt that commits
// reads it. The write ends in a netgroup, which shares no key with a secret
// put, so only the read can make it conflict.
func testSecretTxReadTracked(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
			must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "first"), 0))

			attempts, seen := 0, uint64(0)
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						attempts++

						sec, ok, err := tx.Secret(id, 0)
						if err != nil {
							return err
						}

						if !ok {
							return errors.New("the secret read as absent")
						}

						seen = sec.Version

						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return cp.PutSecret(ctx, secretOf(id, owner, 2, "second"), 1)
				})

			must(t, inner)
			must(t, outer)

			if attempts != 2 {
				t.Errorf("attempts: got %d, want 2", attempts)
			}

			if seen != 2 {
				t.Errorf("version read by the attempt that committed: got %d, want 2", seen)
			}

			err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				sec, ok, err := tx.Secret(newSecretRef(), 0)
				if err != nil || ok || sec.Version != 0 {
					t.Errorf(
						"an absent secret: got %+v, %v, %v, want zero, false, nil",
						sec,
						ok,
						err,
					)
				}

				return nil
			})
			must(t, err)
		})
	}
}

// pruneForms are the two ways to prune: the view's own write, and the
// transaction form inside the caller's Write.
var pruneForms = map[string]func(cp kvstore.ControlPlane, id entity.SecretRef, below uint64) error{
	"view": func(cp kvstore.ControlPlane, id entity.SecretRef, below uint64) error {
		return cp.PruneSecret(ctx, id, below)
	},
	"tx": func(cp kvstore.ControlPlane, id entity.SecretRef, below uint64) error {
		return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tx.PruneSecret(id, below)
		})
	},
}

// putVersions puts versions 1 to n of a secret, version v holding the value
// "v<v>".
func putVersions(t *testing.T, cp kvstore.ControlPlane, id entity.SecretRef, n uint64) {
	t.Helper()

	owner := entity.PrincipalID(uuid.New())

	for v := uint64(1); v <= n; v++ {
		must(t, cp.PutSecret(ctx, secretOf(id, owner, v, "v"+strconv.FormatUint(v, 10)), v-1))
	}
}

// heldVersions lists, in order, the versions among 1 to upto that still
// read, each with its value as put.
func heldVersions(
	t *testing.T,
	cp kvstore.ControlPlane,
	id entity.SecretRef,
	upto uint64,
) []string {
	t.Helper()

	var held []string

	for v := uint64(1); v <= upto; v++ {
		s, err := cp.Secret(ctx, id, v)
		if merrs.IsNotFoundError(err) {
			continue
		}

		must(t, err)

		held = append(held, strconv.FormatUint(v, 10)+"="+string(s.Value))
	}

	return held
}

func wantHeld(t *testing.T, got []string, want ...string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("versions held: got %v, want %v", got, want)
	}
}

// A prune deletes every version below the one named and keeps that one and
// those above it, in both forms.
func testPruneSecretBelow(t *testing.T, kvo Opener) {
	for name, prune := range pruneForms {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, _ := newStore(t, kvo)
			cp := s.ControlPlane()
			id := newSecretRef()
			putVersions(t, cp, id, 4)

			must(t, prune(cp, id, 3))

			wantHeld(t, heldVersions(t, cp, id, 4), "3=v3", "4=v4")
		})
	}
}

// A prune names a version above the current, or the highest number there
// is: the current version stays whatever below is.
func testPruneSecretKeepsCurrent(t *testing.T, kvo Opener) {
	for name, prune := range pruneForms {
		for _, below := range []uint64{4, 99, math.MaxUint64} {
			t.Run(name+"/"+strconv.FormatUint(below, 10), func(t *testing.T) {
				prof.Watch(t, prof.Options{})

				s, _ := newStore(t, kvo)
				cp := s.ControlPlane()
				id := newSecretRef()
				putVersions(t, cp, id, 3)

				must(t, prune(cp, id, below))

				wantHeld(t, heldVersions(t, cp, id, 3), "3=v3")

				if got := secretValue(t, cp, id, 0); got != "v3" {
					t.Errorf("current version after the prune: got %q, want %q", got, "v3")
				}
			})
		}
	}
}

// A prune below the lowest version drops nothing, and a prune of a secret
// that is not there is no error and writes nothing.
func testPruneSecretNothingToDrop(t *testing.T, kvo Opener) {
	for name, prune := range pruneForms {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, _ := newStore(t, kvo)
			cp := s.ControlPlane()
			id := newSecretRef()
			putVersions(t, cp, id, 3)

			for _, below := range []uint64{0, 1} {
				must(t, prune(cp, id, below))

				wantHeld(t, heldVersions(t, cp, id, 3), "1=v1", "2=v2", "3=v3")
			}

			absent := newSecretRef()

			must(t, prune(cp, absent, 5))

			if _, err := cp.Secret(ctx, absent, 0); !merrs.IsNotFoundError(err) {
				t.Errorf("an absent secret after a prune: got %v, want not found", err)
			}
		})
	}
}

// A prune of five versions below 3 leaves exactly 3, 4 and 5: nothing under
// the cut stays, so the versions that remain are one run up to the current.
func testPruneSecretLeavesNoHole(t *testing.T, kvo Opener) {
	for name, prune := range pruneForms {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, _ := newStore(t, kvo)
			cp := s.ControlPlane()
			id := newSecretRef()
			putVersions(t, cp, id, 5)

			must(t, prune(cp, id, 3))

			wantHeld(t, heldVersions(t, cp, id, 5), "3=v3", "4=v4", "5=v5")
		})
	}
}

// A prune raises no settings generation, since nothing resolves a version
// it removes.
func testPruneSecretRaisesNothing(t *testing.T, kvo Opener) {
	for name, prune := range pruneForms {
		t.Run(name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, _ := newStore(t, kvo)
			cp := s.ControlPlane()
			id := newSecretRef()
			putVersions(t, cp, id, 3)
			g := settingsCounter(t, s)

			must(t, prune(cp, id, 3))

			if got := settingsCounter(t, s); got != g {
				t.Errorf("settings generation after a prune: got %d, want %d", got, g)
			}
		})
	}
}

// The transaction form prunes inside the caller's Write: a Write that fails
// after it, or whose commit fails on its audit record, leaves every version.
func testPruneSecretInTheCallersWrite(t *testing.T, kvo Opener) {
	prof.Watch(t, prof.Options{})

	f := kvtest.NewFaultKV(kvo(t))
	must(t, kvstore.Init(ctx, f, uuid.New()))

	s, err := kvstore.Open(ctx, f)
	must(t, err)

	cp := s.ControlPlane()
	id := newSecretRef()
	putVersions(t, cp, id, 3)

	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PruneSecret(id, 3); err != nil {
			return err
		}

		return errSecretInjected
	})
	if !errors.Is(err, errSecretInjected) {
		t.Fatalf("Write that fails after the prune: got %v, want the injected error", err)
	}

	wantHeld(t, heldVersions(t, cp, id, 3), "1=v1", "2=v2", "3=v3")

	f.SetFault(failSet{prefix: "AU‖"})

	audited := entity.WriteMeta{
		Audit: &entity.AuditRecord{
			Target: entity.ResourceRef{
				Kind: entity.ResourceSecret,
				ID:   id[:],
			},
			Change: []byte("prune"),
		},
	}

	err = cp.Write(ctx, audited, func(tx entity.ControlPlaneTx) error {
		return tx.PruneSecret(id, 3)
	})

	f.SetFault(nil)

	if !errors.Is(err, errSecretInjected) {
		t.Fatalf("Write whose audit record fails: got %v, want the injected error", err)
	}

	wantHeld(t, heldVersions(t, cp, id, 3), "1=v1", "2=v2", "3=v3")
}

// A version that commits between a prune's read and its commit makes the
// Write run again, and the cut is made against the new current version.
func testPruneSecretReadTracked(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
			must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "v1"), 0))

			attempts := 0

			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						attempts++

						if err := tx.PruneSecret(id, 99); err != nil {
							return err
						}

						// A guard binds only a transaction that writes, so this
						// one writes a record of its own besides the prune.
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return cp.PutSecret(ctx, secretOf(id, owner, 2, "v2"), 1)
				})

			must(t, inner)
			must(t, outer)

			if attempts != 2 {
				t.Errorf("attempts: got %d, want 2", attempts)
			}

			wantHeld(t, heldVersions(t, cp, id, 2), "2=v2")
		})
	}
}

// A prune sees the version its own transaction put: put the next version
// and prune in one Write leaves that version, whatever below is.
func testPruneSecretSeesTheTxsOwnPut(t *testing.T, kvo Opener) {
	for _, below := range []uint64{2, 99} {
		t.Run(strconv.FormatUint(below, 10), func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			s, _ := newStore(t, kvo)
			cp := s.ControlPlane()
			id, owner := newSecretRef(), entity.PrincipalID(uuid.New())
			must(t, cp.PutSecret(ctx, secretOf(id, owner, 1, "v1"), 0))

			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				if err := tx.PutSecret(secretOf(id, owner, 2, "v2"), 1); err != nil {
					return err
				}

				return tx.PruneSecret(id, below)
			}))

			wantHeld(t, heldVersions(t, cp, id, 2), "2=v2")
		})
	}
}
