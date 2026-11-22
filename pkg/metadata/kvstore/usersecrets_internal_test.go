package kvstore

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// A user's deletion takes every version of its password hash and NT hash
// with its record, in the batch that deletes the record and in no earlier
// one, and leaves another user's secret alone. The deletion runs once on one
// open store, and once with the store reopened after the mark and after the
// first batch, as a restart in the middle of the deletion does. The test
// reads the store's raw keys, so a version no read would reach still counts.
// It catches the versions going in an earlier batch than the record's, not
// in another transaction of the record's own batch, which only a failing
// store would show.
func TestUserSecretsGoWithTheUser(t *testing.T) {
	for _, c := range []struct {
		name    string
		restart bool
	}{
		{"no restart", false},
		{"restart after the mark and after batch 1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			userSecretsGo(t, c.restart)
		})
	}
}

func userSecretsGo(t *testing.T, restart bool) {
	ctx := context.Background()

	// One key a batch: the grant takes the first batch, the record the
	// second.
	r := newRestarting(t, 1)
	cp := r.s.ControlPlane()
	pw, nt, bobPw := entity.SecretRef{15: 1}, entity.SecretRef{15: 2}, entity.SecretRef{15: 3}
	alice := entity.User{
		Principal: entity.NewUser(),
		Name:      "alice",
		Password:  pw,
		NTHash:    nt,
	}
	bob := entity.User{
		Principal: entity.NewUser(),
		Name:      "bob",
		Password:  bobPw,
	}

	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	r.must(err)

	_, err = cp.PutUser(ctx, bob, 0, entity.Alloc{})
	r.must(err)

	// Each of alice's secrets is written, then rotated once.
	for _, v := range []uint64{1, 2} {
		r.must(cp.PutSecret(ctx, userSecret(pw, entity.SecretPassword, alice, entity.UserPasswordKey, v), v-1))
		r.must(cp.PutSecret(ctx, userSecret(nt, entity.SecretNTHash, alice, entity.UserNTHashKey, v), v-1))
	}

	bobSecret := userSecret(bobPw, entity.SecretPassword, bob, entity.UserPasswordKey, 1)
	r.must(cp.PutSecret(ctx, bobSecret, 0))

	grant := entity.ShareGrant{
		Share:     r.share("a"),
		Principal: alice.Principal,
		Access:    entity.AccessRead,
	}
	_, err = cp.SetGrant(ctx, grant, 0)
	r.must(err)

	held := func(when string, record bool) {
		t.Helper()
		heldSecrets(t, r, alice, when, record, pw, nt)
	}
	r.must(cp.DeleteUser(ctx, alice.Principal, gen))

	if restart {
		r.reopen()
	}

	held("after the mark", true)

	cfg := r.counter(cfgGenKey)
	done, err := r.s.Nodes().ContinueDelete(ctx, alice.Principal)
	r.must(err)

	if done {
		t.Fatalf("batch 1 answered done, want not done: the grant is left")
	}

	if restart {
		r.reopen()
	}

	held("after batch 1", true)

	if got := r.counter(cfgGenKey); got != cfg {
		t.Errorf("settings generation after batch 1: got %d, want %d", got, cfg)
	}

	done, err = r.s.Nodes().ContinueDelete(ctx, alice.Principal)
	r.must(err)

	if !done {
		t.Fatalf("batch 2 answered not done, want done")
	}

	held("after batch 2", false)

	if got := r.counter(cfgGenKey); got != cfg+1 {
		t.Errorf("settings generation after batch 2: got %d, want %d", got, cfg+1)
	}

	bobHeld(t, r, bobSecret)

	if !restart {
		return
	}

	// A deletion resumed once more finds no record and ends, deleting
	// nothing else.
	r.reopen()
	done, err = r.s.Nodes().ContinueDelete(ctx, alice.Principal)
	r.must(err)

	if !done {
		t.Errorf("a resumed deletion of a deleted user answered not done, want done")
	}

	held("after a resumed deletion", false)
	bobHeld(t, r, bobSecret)
}

// userSecret is version v of u's secret id of kind, owned by u's field key,
// with sealed bytes standing in for a value.
func userSecret(
	id entity.SecretRef,
	kind entity.SecretKind,
	u entity.User,
	key string,
	v uint64,
) entity.Secret {
	return entity.Secret{
		ID:      id,
		Kind:    kind,
		Version: v,
		KeyID:   "protocol-1",
		Owner:   entity.UserSecretOwner(u.Principal.ID, key),
		Value:   fmt.Appendf(nil, "%s %d", key, v),
	}
}

// heldSecrets fails unless u's record is held, still marked for deletion,
// exactly when record says, and each reference holds versions 1 and 2 while
// it is, none once it is gone.
func heldSecrets(
	t *testing.T,
	r *restarting,
	u entity.User,
	when string,
	record bool,
	refs ...entity.SecretRef,
) {
	t.Helper()

	var want []uint64
	if record {
		want = []uint64{1, 2}
	}

	err := r.s.kv.View(context.Background(), func(rd kv.Reader) error {
		b, ok, err := get(rd, userKey(u.Principal.ID))
		if err != nil {
			return err
		}

		if ok != record {
			t.Errorf("%s: %s's record held %v, want %v", when, u.Name, ok, record)
		}

		if ok {
			got, err := decodeUser(u.Principal.ID, b)
			if err != nil {
				return err
			}

			if !got.Deleted {
				t.Errorf("%s: %s's record Deleted false, want true", when, u.Name)
			}
		}

		for _, ref := range refs {
			got, err := rawVersions(rd, ref)
			if err != nil {
				return err
			}

			if !slices.Equal(got, want) {
				t.Errorf("%s: versions of secret %x: got %v, want %v", when, ref[:], got, want)
			}
		}

		return nil
	})
	r.must(err)
}

// rawVersions lists the versions held under ref's keys, read raw.
func rawVersions(rd kv.Reader, ref entity.SecretRef) ([]uint64, error) {
	var out []uint64

	prefix := secretPrefix(ref)
	for row, err := range rd.Scan(prefix, nil) {
		if err != nil {
			return nil, err
		}

		v, err := secretVersion(prefix, row.Key)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}

// bobHeld fails unless s is still held, byte for byte as it was written.
func bobHeld(t *testing.T, r *restarting, s entity.Secret) {
	t.Helper()

	err := r.s.kv.View(context.Background(), func(rd kv.Reader) error {
		b, ok, err := get(rd, secretKey(s.ID, s.Version))
		if err != nil {
			return err
		}

		if !ok || !bytes.Equal(b, encodeSecret(s)) {
			t.Errorf(
				"another user's secret %x version %d: held %v, want held as written",
				s.ID[:],
				s.Version,
				ok,
			)
		}

		return nil
	})
	r.must(err)
}

// A user's deletion whose record names a secret sealed for another holder,
// or as another kind than the field's, is refused in the batch that would
// delete the record, and leaves every version of that secret and the record
// itself. Each foreign secret is sealed after alice's record names its
// reference, as no PutUser check can see.
func TestUserDeletionRefusesAForeignSecret(t *testing.T) {
	for _, c := range []struct {
		name   string
		secret func(alice, bob entity.User, v uint64) entity.Secret
	}{
		{"another user's password", func(alice, bob entity.User, v uint64) entity.Secret {
			return userSecret(alice.Password, entity.SecretPassword, bob, entity.UserPasswordKey, v)
		}},
		{"alice's field, another kind", func(alice, bob entity.User, v uint64) entity.Secret {
			return userSecret(alice.Password, entity.SecretNTHash, alice, entity.UserPasswordKey, v)
		}},
		{"alice's other field", func(alice, bob entity.User, v uint64) entity.Secret {
			return userSecret(alice.Password, entity.SecretPassword, alice, entity.UserNTHashKey, v)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			r := newRestarting(t, 100)
			cp := r.s.ControlPlane()
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", Password: entity.SecretRef{15: 1}, NTHash: entity.SecretRef{15: 2}}
			bob := entity.User{Principal: entity.NewUser(), Name: "bob"}
			gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
			r.must(err)
			_, err = cp.PutUser(ctx, bob, 0, entity.Alloc{})
			r.must(err)
			for _, v := range []uint64{1, 2} {
				r.must(cp.PutSecret(ctx, c.secret(alice, bob, v), v-1))
				r.must(cp.PutSecret(ctx, userSecret(alice.NTHash, entity.SecretNTHash, alice, entity.UserNTHashKey, v), v-1))
			}

			r.must(cp.DeleteUser(ctx, alice.Principal, gen))
			done, err := r.s.Nodes().ContinueDelete(ctx, alice.Principal)
			if !isInvalid(err) || done {
				t.Errorf("deletion naming a foreign secret: done %v, error %v; want not done, invalid argument", done, err)
			}

			heldSecrets(t, r, alice, "after the refused batch", true, alice.Password, alice.NTHash)
		})
	}
}
