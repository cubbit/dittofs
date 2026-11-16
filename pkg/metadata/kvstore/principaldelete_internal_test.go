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
// data, as a restart does, with each deletion batch held to one item.
type restarting struct {
	t   *testing.T
	dir string
	db  *badgerkv.Store
	s   *Store
}

func newRestarting(t *testing.T) *restarting {
	r := &restarting{t: t, dir: t.TempDir()}
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
	r.s.SetKeyBudget(1)
}

// deleteInBatches runs p's deletion from its mark, reopening the store
// before every batch, and returns how many batches it took before the one
// that answered done. CFGGEN must hold until that last batch and move by
// cfgRaise in it.
func (r *restarting) deleteInBatches(p entity.Principal, cfgRaise uint64) int {
	r.t.Helper()
	cfg := r.counter(cfgGenKey)
	for n := 0; n < 100; n++ {
		r.reopen()
		done, err := r.s.Nodes().ContinueDelete(context.Background(), p)
		if err != nil {
			r.t.Fatalf("batch %d of %v's deletion: %v", n+1, p, err)
		}
		want := cfg
		if done {
			want += cfgRaise
		}
		if got := r.counter(cfgGenKey); got != want {
			r.t.Errorf("settings generation after batch %d (done %v): got %d, want %d", n+1, done, got, want)
		}
		if done {
			return n
		}
	}
	r.t.Fatalf("%v's deletion not done after 100 batches", p)
	return 0
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
// each, drops every row of the user, moves the grant-list version of each
// share it held a grant on, and raises the settings generation in its last
// batch, which deletes the record and its secret's versions.
func TestUserDeletionResumesFromTheMarkAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t)
	cp := r.s.ControlPlane()
	ref, otherRef := entity.SecretRef{15: 1}, entity.SecretRef{15: 2}
	u := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1000"}}, Secret: ref}
	other := entity.User{Principal: entity.NewUser(), Name: "bob", Secret: otherRef}
	g1, g2 := entity.Group{Principal: entity.NewGroup(), Name: "g1"}, entity.Group{Principal: entity.NewGroup(), Name: "g2"}
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	r.must(err)
	for _, err := range []error{errOnly(cp.PutUser(ctx, other, 0, entity.Alloc{})), errOnly(cp.PutGroup(ctx, g1, 0, entity.Alloc{})), errOnly(cp.PutGroup(ctx, g2, 0, entity.Alloc{}))} {
		r.must(err)
	}
	a, b := r.share("a"), r.share("b")
	r.must(cp.AddMember(ctx, entity.Membership{Group: g1.Principal, Member: u.Principal}))
	r.must(cp.AddMember(ctx, entity.Membership{Group: g2.Principal, Member: u.Principal}))
	for _, sh := range []entity.ShareID{a, b} {
		r.must(errOnly(cp.SetGrant(ctx, entity.ShareGrant{Share: sh, Principal: u.Principal, Access: entity.AccessRead}, 0)))
	}
	r.must(errOnly(cp.SetQuota(ctx, a, entity.Quota{Principal: u.Principal, Hard: entity.Limit{Files: 10}}, 0)))
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
	gvA, gvB := r.counter(shareKey(a, keys.SGv)), r.counter(shareKey(b, keys.SGv))

	r.must(cp.DeleteUser(ctx, u.Principal, gen))
	// Two grants, two memberships, a quota, a token, a session and a count.
	if got := r.deleteInBatches(u.Principal, 1); got != 8 {
		t.Errorf("batches before the last: got %d, want 8, one per item", got)
	}

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
	if got := r.counter(shareKey(a, keys.SGv)); got <= gvA {
		t.Errorf("share a's grant-list version after the deletion: got %d, want above %d", got, gvA)
	}
	if got := r.counter(shareKey(b, keys.SGv)); got <= gvB {
		t.Errorf("share b's grant-list version after the deletion: got %d, want above %d", got, gvB)
	}
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

// A group's deletion drops its memberships both ways: its members' rows and
// the groups it is in.
func TestGroupDeletionDropsMembershipsBothWays(t *testing.T) {
	ctx := context.Background()
	r := newRestarting(t)
	cp := r.s.ControlPlane()
	m := entity.User{Principal: entity.NewUser(), Name: "m"}
	g, outer := entity.Group{Principal: entity.NewGroup(), Name: "g"}, entity.Group{Principal: entity.NewGroup(), Name: "outer"}
	r.must(errOnly(cp.PutUser(ctx, m, 0, entity.Alloc{})))
	gen, err := cp.PutGroup(ctx, g, 0, entity.Alloc{})
	r.must(err)
	r.must(errOnly(cp.PutGroup(ctx, outer, 0, entity.Alloc{})))
	r.must(cp.AddMember(ctx, entity.Membership{Group: g.Principal, Member: m.Principal}))
	r.must(cp.AddMember(ctx, entity.Membership{Group: outer.Principal, Member: g.Principal}))

	r.must(cp.DeleteGroup(ctx, g.Principal, gen))
	if got := r.deleteInBatches(g.Principal, 0); got != 2 {
		t.Errorf("batches before the last: got %d, want 2, one per membership", got)
	}
	r.gone("group", [][]byte{groupKey(g.Principal.ID), memberRevKey(m.Principal.ID, g.Principal.ID), memberKey(outer.Principal.ID, g.Principal.ID)},
		[][]byte{membersPrefix(g.Principal.ID), memberOfPrefix(g.Principal.ID)})
}

func errOnly(_ uint64, err error) error { return err }
