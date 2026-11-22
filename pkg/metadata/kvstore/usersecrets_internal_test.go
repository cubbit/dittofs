package kvstore

import (
	"bytes"
	"context"
	"errors"
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

// foreignSecrets are the secrets alice's password reference may name that
// are not hers: sealed for another holder, or as another kind than the
// field's.
var foreignSecrets = []struct {
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
}

// DeleteUser refuses a user whose record names a secret sealed for another
// holder, or as another kind than the field's, before it marks anything:
// the record stays unmarked, so a deletion batch run after it drops nothing,
// and its grant, its membership and every version of its secrets stay held.
func TestDeleteUserRefusesAForeignSecret(t *testing.T) {
	for _, c := range foreignSecrets {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			r := newRestarting(t, 100)
			cp := r.s.ControlPlane()
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", Password: entity.SecretRef{15: 1}, NTHash: entity.SecretRef{15: 2}}
			bob := entity.User{Principal: entity.NewUser(), Name: "bob"}
			g := fileGroup("staff", "2001")
			gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
			r.must(err)
			_, err = cp.PutUser(ctx, bob, 0, entity.Alloc{})
			r.must(err)
			_, err = cp.PutGroup(ctx, g, 0, entity.Alloc{})
			r.must(err)
			r.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: alice.Principal}))
			share := r.share("a")
			_, err = cp.SetGrant(ctx, entity.ShareGrant{Share: share, Principal: alice.Principal, Access: entity.AccessRead}, 0)
			r.must(err)
			for _, v := range []uint64{1, 2} {
				r.must(cp.PutSecret(ctx, c.secret(alice, bob, v), v-1))
				r.must(cp.PutSecret(ctx, userSecret(alice.NTHash, entity.SecretNTHash, alice, entity.UserNTHashKey, v), v-1))
			}

			if err := cp.DeleteUser(ctx, alice.Principal, gen); !isInvalid(err) {
				t.Errorf("DeleteUser naming a foreign secret: got %v, want an invalid argument", err)
			}
			// The batch refuses an unmarked user; one marked would lose its
			// grant and membership to it.
			if done, _ := r.s.Nodes().ContinueDelete(ctx, alice.Principal); done {
				t.Errorf("a deletion batch after the refused DeleteUser answered done, want not done")
			}

			r.must(r.s.kv.View(ctx, func(rd kv.Reader) error {
				b, ok, err := get(rd, userKey(alice.Principal.ID))
				if err != nil || !ok {
					t.Fatalf("alice's record after the refused DeleteUser: held %v, error %v; want held", ok, err)
				}
				u, err := decodeUser(alice.Principal.ID, b)
				if err != nil {
					return err
				}
				if u.Deleted {
					t.Errorf("alice's record after the refused DeleteUser: Deleted true, want false")
				}
				for what, k := range map[string][]byte{
					"grant":      grantKey(share, alice.Principal.ID),
					"membership": memberKey(g.Principal.ID, alice.Principal.ID),
				} {
					if _, ok, err := get(rd, k); err != nil || !ok {
						t.Errorf("alice's %s after the refused DeleteUser: held %v, error %v; want held", what, ok, err)
					}
				}
				for _, ref := range []entity.SecretRef{alice.Password, alice.NTHash} {
					got, err := rawVersions(rd, ref)
					if err != nil {
						return err
					}
					if !slices.Equal(got, []uint64{1, 2}) {
						t.Errorf("versions of secret %x after the refused DeleteUser: got %v, want [1 2]", ref[:], got)
					}
				}
				return nil
			}))
		})
	}
}

// A user's deletion whose record names a secret sealed for another holder,
// or as another kind than the field's, after DeleteUser marked it, is
// refused in the batch that would delete the record, and leaves every
// version of that secret and the record itself. No PutUser or DeleteUser
// check can see a secret sealed that late.
func TestUserDeletionRefusesAForeignSecret(t *testing.T) {
	for _, c := range foreignSecrets {
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
				r.must(cp.PutSecret(ctx, userSecret(alice.NTHash, entity.SecretNTHash, alice, entity.UserNTHashKey, v), v-1))
			}

			r.must(cp.DeleteUser(ctx, alice.Principal, gen))
			for _, v := range []uint64{1, 2} {
				r.must(cp.PutSecret(ctx, c.secret(alice, bob, v), v-1))
			}
			done, err := r.s.Nodes().ContinueDelete(ctx, alice.Principal)
			if !isInvalid(err) || done {
				t.Errorf("deletion naming a foreign secret: done %v, error %v; want not done, invalid argument", done, err)
			}

			heldSecrets(t, r, alice, "after the refused batch", true, alice.Password, alice.NTHash)
		})
	}
}

// A user's deletion takes every version of a secret its record names by its
// keys alone, so an old version whose row no longer decodes is deleted with
// the rest and does not stall the deletion.
func TestUserDeletionTakesAnUndecodableOldVersion(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t, 100)
	cp := r.s.ControlPlane()
	pw := entity.SecretRef{15: 1}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", Password: pw}
	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	r.must(err)
	for _, v := range []uint64{1, 2} {
		r.must(cp.PutSecret(ctx, userSecret(pw, entity.SecretPassword, alice, entity.UserPasswordKey, v), v-1))
	}
	r.must(r.s.kv.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(secretKey(pw, 1), []byte("not a secret"))
	}))

	if err := cp.DeleteUser(ctx, alice.Principal, gen); err != nil {
		t.Fatalf("DeleteUser over an undecodable version 1: %v", err)
	}
	for batch := 1; ; batch++ {
		done, err := r.s.Nodes().ContinueDelete(ctx, alice.Principal)
		if err != nil {
			t.Fatalf("batch %d of a deletion over an undecodable version 1: %v", batch, err)
		}
		if done {
			break
		}
		if batch == 10 {
			t.Fatalf("deletion over an undecodable version 1 not done after %d batches", batch)
		}
	}

	r.must(r.s.kv.View(ctx, func(rd kv.Reader) error {
		got, err := rawVersions(rd, pw)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Errorf("versions of alice's password after her deletion: got %v, want none", got)
		}
		return nil
	}))
}

// DeleteUser marks a user whose current password version does not decode,
// so a corrupt secret does not keep the user live, and the batch that would
// delete the record fails on that version, leaving the record marked and
// every version held.
func TestUserDeletionStallsOnAnUndecodableCurrentVersion(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t, 100)
	cp := r.s.ControlPlane()
	pw := entity.SecretRef{15: 1}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", Password: pw}
	gen, err := cp.PutUser(ctx, alice, 0, entity.Alloc{})
	r.must(err)
	for _, v := range []uint64{1, 2} {
		r.must(cp.PutSecret(ctx, userSecret(pw, entity.SecretPassword, alice, entity.UserPasswordKey, v), v-1))
	}
	r.must(r.s.kv.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(secretKey(pw, 2), []byte("not a secret"))
	}))

	if err := cp.DeleteUser(ctx, alice.Principal, gen); err != nil {
		t.Fatalf("DeleteUser over an undecodable current version: got %v, want nil", err)
	}
	if _, ok, err := cp.UserNamed(ctx, "alice"); err != nil || ok {
		t.Errorf("UserNamed(alice) after the mark: found %v, error %v; want not found", ok, err)
	}
	done, err := r.s.Nodes().ContinueDelete(ctx, alice.Principal)
	if !errors.As(err, new(secretUndecodable)) || done {
		t.Errorf("deletion batch over an undecodable current version: done %v, error %v; want not done, the decode error", done, err)
	}

	heldSecrets(t, r, alice, "after the failed batch", true, pw)
}
