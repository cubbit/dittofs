package kvstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// restarting is a store on disk that reopen closes and opens again over its
// data, as a restart does, with each deletion batch held to k keys.
type restarting struct {
	t   *testing.T
	dir string
	k   int
	db  *badgerkv.Store
	s   *Store
}

func newRestarting(t *testing.T, k int) *restarting {
	r := &restarting{t: t, dir: t.TempDir(), k: k}
	t.Cleanup(func() {
		if err := r.db.Close(); err != nil {
			t.Error(err)
		}
	})
	r.reopen()
	return r
}

func (r *restarting) reopen() {
	r.t.Helper()
	ctx := context.Background()
	if r.db != nil {
		if err := r.db.Close(); err != nil {
			r.t.Fatal(err)
		}
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: r.dir})
	if err != nil {
		r.t.Fatal(err)
	}
	if r.db = db; r.s == nil {
		if err := Init(ctx, db, uuid.New()); err != nil {
			r.t.Fatal(err)
		}
	}
	if r.s, err = Open(ctx, db); err != nil {
		r.t.Fatal(err)
	}
	r.s.SetKeyBudget(r.k)
}

// deleteInBatches runs p's deletion from its mark, reopening the store
// before every batch. It must take one batch per entry of pgen, the last
// answering done, each raising the principals generation by its entry; and
// CFGGEN must hold until that last batch and move by cfgRaise in it.
func (r *restarting) deleteInBatches(p entity.Principal, pgen []uint64, cfgRaise uint64) {
	r.t.Helper()
	cfg := r.counter(cfgGenKey)
	for n, want := range pgen {
		r.reopen()
		before := r.counter(pgenKey)
		done, err := r.s.Nodes().ContinueDelete(context.Background(), p)
		if err != nil {
			r.t.Fatalf("batch %d of %v's deletion: %v", n+1, p, err)
		}
		if got := r.counter(pgenKey) - before; got != want {
			r.t.Errorf("principals generation raised by batch %d: got %d, want %d", n+1, got, want)
		}
		last := n == len(pgen)-1
		if last {
			cfg += cfgRaise
		}
		if got := r.counter(cfgGenKey); got != cfg {
			r.t.Errorf("settings generation after batch %d: got %d, want %d", n+1, got, cfg)
		}
		if done != last {
			r.t.Fatalf("batch %d of %d answered done %v, want %v", n+1, len(pgen), done, last)
		}
	}
}

func (r *restarting) counter(key []byte) (v uint64) {
	r.t.Helper()
	if err := r.s.kv.View(context.Background(), func(rd kv.Reader) (err error) {
		v, err = counterIn(rd, key)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return v
}

// gone fails for each key still present and each prefix with a row under it.
func (r *restarting) gone(what string, ks [][]byte, prefixes [][]byte) {
	r.t.Helper()
	if err := r.s.kv.View(context.Background(), func(rd kv.Reader) error {
		for _, k := range ks {
			if _, ok, err := get(rd, k); err != nil || ok {
				r.t.Errorf("%s: key %s present after the deletion (%v)", what, keys.Describe(k), err)
			}
		}
		for _, p := range prefixes {
			for row, err := range rd.Scan(p, nil) {
				r.t.Errorf("%s: row %s under %s after the deletion (%v)", what, keys.Describe(row.Key), keys.Describe(p), err)
				break
			}
		}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *restarting) must(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *restarting) share(name string) entity.ShareID {
	sh := entity.Share{ID: entity.NewShareID(), Name: name, Path: "/" + name, Root: entity.NewFileID()}
	_, err := createShare(r.s.ControlPlane(), sh)
	r.must(err)
	return sh.ID
}

func secretVersionKey(ref entity.SecretRef, version uint64) []byte {
	return keys.Uint64(keys.ID([]byte{keys.KindSEC}, ref[:]), version)
}

// A user's deletion, run one item per batch with the store reopened before
// each, drops every row of the user, raises the principals generation in
// each batch dropping a membership and in the last, and raises the settings
// generation in that last batch, which deletes the record and its secret's
// versions.
func TestUserDeletionResumesFromTheMarkAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t, 1)
	cp := r.s.ControlPlane()
	ref, otherRef := entity.SecretRef{15: 1}, entity.SecretRef{15: 2}
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1000"}}, Secret: ref}
	other := entity.User{Principal: entity.NewUser(), Name: "bob", Secret: otherRef}
	g1, g2 := entity.Group{Principal: entity.NewGroup(), Name: "g1"}, entity.Group{Principal: entity.NewGroup(), Name: "g2"}
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	r.must(err)
	for _, err := range []error{errOf(cp.PutUser(ctx, other, 0, entity.Alloc{})), errOf(cp.PutGroup(ctx, g1, 0, entity.Alloc{})), errOf(cp.PutGroup(ctx, g2, 0, entity.Alloc{}))} {
		r.must(err)
	}
	a, b := r.share("a"), r.share("b")
	r.must(cp.AddMember(ctx, entity.Membership{Group: g1.Principal, Member: u.Principal}))
	r.must(cp.AddMember(ctx, entity.Membership{Group: g2.Principal, Member: u.Principal}))
	for _, sh := range []entity.ShareID{a, b} {
		r.must(errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: sh, Principal: u.Principal, Access: entity.AccessRead}, 0)))
	}
	r.must(errOf(cp.SetQuota(ctx, a, entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 10}}, 0)))
	expires := time.Now().Add(time.Hour)
	tok := entity.APIToken{ID: [16]byte{15: 1}, Account: u.Principal, Hash: [32]byte{31: 1}, Expires: expires}
	sess := entity.Session{ID: [16]byte{15: 2}, Account: u.Principal, Refresh: [32]byte{31: 2}, Expires: expires}
	count := entity.SignInKey{Account: u.Principal}
	r.must(cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutAPIToken(tok); err != nil {
			return err
		}
		if err := tx.PutSession(sess); err != nil {
			return err
		}
		return tx.PutSignInFailures(count, entity.SignInFailures{Count: 1})
	}))
	rows(t, r.s, secretVersionKey(ref, 1), []byte{1}, secretVersionKey(ref, 2), []byte{2}, secretVersionKey(otherRef, 1), []byte{3})

	r.must(cp.DeleteUser(ctx, u.Principal, gen))
	// Two grants, two memberships, a quota, a token, the session's refresh
	// hash then the session, which outgrows a one-key batch, a count, and the
	// last batch.
	r.deleteInBatches(u.Principal, []uint64{0, 0, 1, 1, 0, 0, 0, 0, 0, 1}, 1)

	tag, tail, err := signInTail(count)
	r.must(err)
	quota, err := quotaSuffix(QuotaTarget{Principal: u.Principal})
	r.must(err)
	r.gone("user", [][]byte{
		userKey(u.Principal.ID),
		grantKey(a, u.Principal.ID), grantKey(b, u.Principal.ID),
		memberKey(g1.Principal.ID, u.Principal.ID), memberKey(g2.Principal.ID, u.Principal.ID),
		quotaKey(a, keys.SQt, quota),
		tokenKey(tok.ID), tokenHashKey(tok.Hash), tokenExpiryKey(tok),
		sessionKey(sess.ID), refreshKey(sess.Refresh), sessionExpiryKey(sess),
		signInKey(tag, tail),
	}, [][]byte{
		grantsOfPrefix(u.Principal.ID), memberOfPrefix(u.Principal.ID),
		accountTokensPrefix(u.Principal), accountSessionsPrefix(u.Principal),
		keys.ID([]byte{keys.KindSEC}, ref[:]),
	})
	if err := r.s.kv.View(ctx, func(rd kv.Reader) error {
		_, ok, err := get(rd, secretVersionKey(otherRef, 1))
		if err == nil && !ok {
			t.Errorf("another user's secret version deleted with alice's")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A group's deletion drops its memberships both ways, its members' rows and
// the groups it is in, one a batch, each raising the principals generation,
// as the last batch does.
func TestGroupDeletionDropsMembershipsBothWays(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t, 1)
	cp := r.s.ControlPlane()
	m := entity.User{Principal: entity.NewUser(), Name: "m"}
	g, outer := entity.Group{Principal: entity.NewGroup(), Name: "g"}, entity.Group{Principal: entity.NewGroup(), Name: "outer"}
	r.must(errOf(cp.PutUser(ctx, m, 0, entity.Alloc{})))
	gen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	r.must(err)
	r.must(errOf(cp.PutGroup(ctx, outer, 0, entity.Alloc{})))
	r.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: m.Principal}))
	r.must(cp.AddMember(ctx, entity.Membership{Group: outer.Principal, Member: g.Principal}))

	r.must(cp.DeleteGroup(ctx, g.Principal, gen))
	r.deleteInBatches(g.Principal, []uint64{1, 1, 1}, 0)
	r.gone("group", [][]byte{groupKey(g.Principal.ID), memberRevKey(m.Principal.ID, g.Principal.ID), memberKey(outer.Principal.ID, g.Principal.ID)},
		[][]byte{membersPrefix(g.Principal.ID), memberOfPrefix(g.Principal.ID)})
}

// A batch takes items while their keys fit its budget, less the one key kept
// for the principals generation: a grant costs 3, a membership 2, a quota 1,
// a token 4, a session 3 and 2 per refresh hash, a count 2. A session that
// outgrows a batch as its first item loses refresh hashes first.
func TestDeletionBatchesFollowKeyCosts(t *testing.T) {
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	session := func(r *restarting, u entity.Principal, id byte, hashes int) {
		s := entity.Session{ID: [16]byte{15: id}, Account: u, Expires: expires}
		for h := range hashes {
			s.Refresh = [32]byte{30: id, 31: byte(h)}
			if h == 0 {
				r.must(r.s.ControlPlane().PutSession(ctx, s))
			} else {
				r.must(r.s.ControlPlane().UpdateSession(ctx, s))
			}
		}
	}
	for _, c := range []struct {
		name  string
		k     int
		setup func(*restarting, ControlPlane, entity.Principal)
		pgen  []uint64
	}{
		{"two grants at 6 keys, one a batch", 6, func(r *restarting, cp ControlPlane, u entity.Principal) {
			for _, sh := range []string{"a", "b"} {
				r.must(errOf(cp.SetGrant(ctx, entity.ShareGrant{Share: r.share(sh), Principal: u, Access: entity.AccessRead}, 0)))
			}
		}, []uint64{0, 0, 1}},
		{"two memberships at 5 keys, in one batch", 5, func(r *restarting, cp ControlPlane, u entity.Principal) {
			for _, name := range []string{"g1", "g2"} {
				g := entity.Group{Principal: entity.NewGroup(), Name: name}
				r.must(errOf(cp.PutGroup(ctx, g, 0, entity.Alloc{})))
				r.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: u}))
			}
		}, []uint64{1, 1}},
		{"a quota and a count at 3 keys, one a batch", 3, func(r *restarting, cp ControlPlane, u entity.Principal) {
			r.must(errOf(cp.SetQuota(ctx, r.share("a"), entity.Quota{Principal: u, Hard: entity.Limit{Files: 1}}, 0)))
			r.must(cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return tx.PutSignInFailures(entity.SignInKey{Account: u}, entity.SignInFailures{Count: 1})
			}))
		}, []uint64{0, 0, 1}},
		{"two tokens at 5 keys, one a batch", 5, func(r *restarting, cp ControlPlane, u entity.Principal) {
			for id := range byte(2) {
				r.must(cp.PutAPIToken(ctx, entity.APIToken{ID: [16]byte{15: id}, Account: u, Hash: [32]byte{31: id}, Expires: expires}))
			}
		}, []uint64{0, 0, 1}},
		{"two sessions of one hash at 7 keys, one a batch", 7, func(r *restarting, _ ControlPlane, u entity.Principal) {
			session(r, u, 1, 1)
			session(r, u, 2, 1)
		}, []uint64{0, 0, 1}},
		{"a session of three hashes at 3 keys, a hash a batch, then the session", 3, func(r *restarting, _ ControlPlane, u entity.Principal) {
			session(r, u, 1, 3)
		}, []uint64{0, 0, 0, 0, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRestarting(t, c.k)
			cp := r.s.ControlPlane()
			u := entity.User{Principal: entity.NewUser(), Name: "alice"}
			gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
			r.must(err)
			c.setup(r, cp, u.Principal)
			r.must(cp.DeleteUser(ctx, u.Principal, gen))
			r.deleteInBatches(u.Principal, c.pgen, 0)
		})
	}
}
