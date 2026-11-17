package kvstoretest

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// account stores a user for a session, a token or a count to name: one
// naming no user is refused.
func account(t *testing.T, s *kvstore.Store) entity.Principal {
	t.Helper()
	u := user("alice")
	must(t, errOf(s.ControlPlane().PutUser(ctx, u, 0, entity.Alloc{})))
	return u.Principal
}

// A session's refresh token rotated twice is found by each of its three
// hashes, the current one reported as Refresh; deleted, it is found by none,
// nor by its ID, nor in its account's list, and cannot be updated back.
func testSessionRefreshIndex(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := account(t, s)
	sess := entity.Session{ID: uuid.New(), Account: alice, Expires: time.Unix(1_900_000_000, 0).UTC()}
	hashes := [][32]byte{{1}, {2}, {3}}
	for i, h := range hashes {
		sess.Refresh = h
		if i == 0 {
			must(t, cp.PutSession(ctx, sess))
		} else {
			must(t, cp.UpdateSession(ctx, sess))
		}
	}
	if got, ok, err := cp.Session(ctx, sess.ID); err != nil || !ok || got != sess {
		t.Errorf("session by ID: got %+v, %v, %v; want %+v", got, ok, err, sess)
	}
	for i, h := range hashes {
		got, ok, err := cp.SessionByRefresh(ctx, h)
		if err != nil || !ok || got.ID != sess.ID || got.Refresh != hashes[2] {
			t.Errorf("session by refresh hash %d of 3: got %x current %x, %v, %v; want %x current %x",
				i+1, got.ID, got.Refresh[:1], ok, err, sess.ID, hashes[2][:1])
		}
	}
	if got := all(t, cp.Sessions(ctx, alice)); len(got) != 1 || got[0] != sess {
		t.Errorf("alice's sessions: got %+v, want only %+v", got, sess)
	}
	if got := all(t, cp.Sessions(ctx, entity.NewUser())); len(got) != 0 {
		t.Errorf("another account's sessions: got %d, want 0", len(got))
	}

	must(t, cp.DeleteSession(ctx, sess.ID))
	for i, h := range hashes {
		if _, ok, err := cp.SessionByRefresh(ctx, h); err != nil || ok {
			t.Errorf("deleted session by refresh hash %d of 3: found %v, %v; want absent", i+1, ok, err)
		}
	}
	if _, ok, err := cp.Session(ctx, sess.ID); err != nil || ok {
		t.Errorf("deleted session by ID: found %v, %v; want absent", ok, err)
	}
	for got, err := range cp.Sessions(ctx, alice) {
		t.Errorf("alice's sessions after the deletion: got %+v, %v; want none", got, err)
	}
	if err := cp.DeleteSession(ctx, sess.ID); err != nil {
		t.Errorf("a deleted session deleted again: %v, want nil", err)
	}
	if err := cp.UpdateSession(ctx, sess); !isCode(err, merrs.ErrNotFound) {
		t.Errorf("a deleted session updated: %v, want not found", err)
	}
}

// Inside one Write, a session and a token just written read back by ID and
// by hash.
func testSessionTxReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	alice := account(t, s)
	e := time.Unix(1_900_000_000, 0).UTC()
	sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: e}
	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{2}, Expires: e}
	must(t, s.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSession(sess); err != nil {
			return err
		}
		if err := tx.PutAPIToken(tok); err != nil {
			return err
		}
		if got, ok, err := tx.Session(sess.ID); err != nil || !ok || got != sess {
			t.Errorf("tx Session: got %+v, %v, %v; want %+v", got, ok, err, sess)
		}
		if got, ok, err := tx.SessionByRefresh(sess.Refresh); err != nil || !ok || got != sess {
			t.Errorf("tx SessionByRefresh: got %+v, %v, %v; want %+v", got, ok, err, sess)
		}
		if got, ok, err := tx.APIToken(tok.ID); err != nil || !ok || got != tok {
			t.Errorf("tx APIToken: got %+v, %v, %v; want %+v", got, ok, err, tok)
		}
		if got, ok, err := tx.APITokenByHash(tok.Hash); err != nil || !ok || got != tok {
			t.Errorf("tx APITokenByHash: got %+v, %v, %v; want %+v", got, ok, err, tok)
		}
		return nil
	}))
}

// A session or a token needs a user account and an expiry after 1970, a
// session keeps its account, and neither takes a hash indexed to another; a
// source's count needs a Failed and a pair's a Last after 1970. Each refusal
// leaves the store as it was.
func testSessionRefusals(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := account(t, s)
	e := time.Unix(1_900_000_000, 0).UTC()
	pre := time.Unix(-1, 0)
	sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: e}
	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{2}, Expires: e}
	must(t, cp.PutSession(ctx, sess))
	must(t, cp.PutAPIToken(ctx, tok))
	withSession := func(f func(*entity.Session)) entity.Session {
		c := sess
		c.ID = uuid.New()
		c.Refresh = [32]byte{9}
		f(&c)
		return c
	}
	withToken := func(f func(*entity.APIToken)) entity.APIToken {
		c := tok
		c.ID = uuid.New()
		c.Hash = [32]byte{9}
		f(&c)
		return c
	}
	src := entity.SignInKey{Source: netip.MustParsePrefix("192.0.2.1/32")}
	pair := entity.SignInKey{Account: alice, Source: src.Source}
	for _, c := range []struct {
		name string
		code merrs.ErrorCode
		fn   func(entity.ControlPlaneTx) error
	}{
		{"session of a group", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSession(withSession(func(s *entity.Session) { s.Account = entity.NewGroup() }))
		}},
		{"session with no expiry", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSession(withSession(func(s *entity.Session) { s.Expires = time.Time{} }))
		}},
		{"session expiring before 1970", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSession(withSession(func(s *entity.Session) { s.Expires = pre }))
		}},
		{"session moved to another account", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			moved := sess
			moved.Account = entity.NewUser()
			return tx.UpdateSession(moved)
		}},
		{"session created again", merrs.ErrAlreadyExists, func(tx entity.ControlPlaneTx) error {
			return tx.PutSession(sess)
		}},
		{"session taking another's refresh hash", merrs.ErrAlreadyExists, func(tx entity.ControlPlaneTx) error {
			return tx.PutSession(withSession(func(s *entity.Session) { s.Refresh = sess.Refresh }))
		}},
		{"session updated to another's refresh hash", merrs.ErrAlreadyExists, func(tx entity.ControlPlaneTx) error {
			other := withSession(func(*entity.Session) {})
			if err := tx.PutSession(other); err != nil {
				return err
			}
			other.Refresh = sess.Refresh
			return tx.UpdateSession(other)
		}},
		{"token of a group", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutAPIToken(withToken(func(t *entity.APIToken) { t.Account = entity.NewGroup() }))
		}},
		{"token with no expiry", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutAPIToken(withToken(func(t *entity.APIToken) { t.Expires = time.Time{} }))
		}},
		{"token expiring before 1970", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutAPIToken(withToken(func(t *entity.APIToken) { t.Expires = pre }))
		}},
		{"token taking another's hash", merrs.ErrAlreadyExists, func(tx entity.ControlPlaneTx) error {
			return tx.PutAPIToken(withToken(func(t *entity.APIToken) { t.Hash = tok.Hash }))
		}},
		{"source count with no Failed", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(src, entity.SignInFailures{Count: 1, Last: e})
		}},
		{"source count failed before 1970", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(src, entity.SignInFailures{Count: 1, Last: e, Failed: pre})
		}},
		{"pair count with no Last", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(pair, entity.SignInFailures{Count: 1, Failed: e})
		}},
		{"pair count last before 1970", merrs.ErrInvalidArgument, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(pair, entity.SignInFailures{Count: 1, Last: pre, Failed: e})
		}},
	} {
		if err := cp.Write(ctx, entity.WriteMeta{}, c.fn); !isCode(err, c.code) {
			t.Errorf("%s: %v, want code %v", c.name, err, c.code)
		}
	}
	if got, ok, err := cp.SessionByRefresh(ctx, sess.Refresh); err != nil || !ok || got != sess {
		t.Errorf("session by its refresh hash after the refusals: got %+v, %v, %v; want %+v", got, ok, err, sess)
	}
	if got, ok, err := cp.APITokenByHash(ctx, tok.Hash); err != nil || !ok || got != tok {
		t.Errorf("token by its hash after the refusals: got %+v, %v, %v; want %+v", got, ok, err, tok)
	}
}

// A token is found by its hash and listed under its account until it is
// deleted, and is never rewritten.
func testAPITokens(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := account(t, s)
	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{7},
		Created: time.Unix(1_800_000_000, 0).UTC(), Expires: time.Unix(1_900_000_000, 0).UTC()}
	must(t, cp.PutAPIToken(ctx, tok))
	if got, ok, err := cp.APITokenByHash(ctx, tok.Hash); err != nil || !ok || got != tok {
		t.Errorf("token by hash: got %+v, %v, %v; want %+v", got, ok, err, tok)
	}
	if got, ok, err := cp.APIToken(ctx, tok.ID); err != nil || !ok || got != tok {
		t.Errorf("token by ID: got %+v, %v, %v; want %+v", got, ok, err, tok)
	}
	if got := all(t, cp.APITokens(ctx, alice)); len(got) != 1 || got[0] != tok {
		t.Errorf("alice's tokens: got %+v, want only %+v", got, tok)
	}
	again := tok
	again.Hash = [32]byte{8}
	if err := cp.PutAPIToken(ctx, again); !isCode(err, merrs.ErrAlreadyExists) {
		t.Errorf("token written again under its ID: %v, want already exists", err)
	}
	if _, ok, err := cp.APITokenByHash(ctx, again.Hash); err != nil || ok {
		t.Errorf("refused token's hash: found %v, %v; want absent", ok, err)
	}

	must(t, cp.DeleteAPIToken(ctx, tok.ID))
	if _, ok, err := cp.APITokenByHash(ctx, tok.Hash); err != nil || ok {
		t.Errorf("deleted token by hash: found %v, %v; want absent", ok, err)
	}
	if got := all(t, cp.APITokens(ctx, alice)); len(got) != 0 {
		t.Errorf("alice's tokens after the deletion: got %d, want 0", len(got))
	}
	if _, ok, err := cp.APIToken(ctx, tok.ID); err != nil || ok {
		t.Errorf("deleted token by ID: found %v, %v; want absent", ok, err)
	}
}

// A sign-in count is read and raised in one Write, by account, source or
// pair, each its own count, and the view reads what the Write did; a key
// naming neither is refused; a deleted count reads as absent.
func testSignInFailures(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := account(t, s)
	src := netip.MustParsePrefix("192.0.2.1/32")
	keys := map[string]entity.SignInKey{
		"account": {Account: alice},
		"source":  {Source: src},
		"pair":    {Account: alice, Source: src},
	}
	raise := func(k entity.SignInKey) error {
		return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			f, _, err := tx.SignInFailures(k)
			if err != nil {
				return err
			}
			f.Count++
			f.Last, f.Failed = tx.Now(), tx.Now()
			return tx.PutSignInFailures(k, f)
		})
	}
	read := func(k entity.SignInKey) (f entity.SignInFailures, ok bool) {
		must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
			f, ok, err = tx.SignInFailures(k)
			return err
		}))
		return f, ok
	}
	for i, name := range []string{"account", "source", "pair"} {
		for range i + 1 {
			must(t, raise(keys[name]))
		}
	}
	for i, name := range []string{"account", "source", "pair"} {
		f, ok := read(keys[name])
		if !ok || f.Count != uint32(i+1) {
			t.Errorf("%s's count: got %d, found %v; want %d", name, f.Count, ok, i+1)
		}
		if v, vok, err := cp.SignInFailures(ctx, keys[name]); err != nil || !vok || v != f {
			t.Errorf("%s's count read on the view: got %+v, %v, %v; want %+v", name, v, vok, err, f)
		}
	}
	if _, ok, err := cp.SignInFailures(ctx, entity.SignInKey{Account: entity.NewUser()}); err != nil || ok {
		t.Errorf("another account's count on the view: found %v, %v; want absent", ok, err)
	}
	if err := raise(entity.SignInKey{}); !invalid(err) {
		t.Errorf("count naming no account and no source: %v, want invalid argument", err)
	}
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.DeleteSignInFailures(keys["pair"])
	}))
	if _, ok := read(keys["pair"]); ok {
		t.Errorf("pair's count after its deletion: found, want absent")
	}
	if f, ok := read(keys["source"]); !ok || f.Count != 2 {
		t.Errorf("source's count after the pair's deletion: got %d, found %v; want 2", f.Count, ok)
	}
}

// Two raises of one count, the second committing between the first's read
// and its commit: the first retries over the second's raise, and both land.
//
// decision: this cannot tell a tracked read from an untracked one. The kv
// contract makes a write conflict with any concurrent write of the same key,
// so the raise's own Set already retries it, and the tracked read is
// redundant today. It decides only on a backend that stops conflicting blind
// writes, which would then fail this case without it.
func testSignInFailuresRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			k := entity.SignInKey{Source: netip.MustParsePrefix("192.0.2.1/32")}
			raise := func() error {
				return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
					f, _, err := tx.SignInFailures(k)
					if err != nil {
						return err
					}
					f.Count++
					f.Last, f.Failed = tx.Now(), tx.Now()
					return tx.PutSignInFailures(k, f)
				})
			}
			outer, inner := race(h, raise, raise)
			must(t, inner)
			must(t, outer)
			var got entity.SignInFailures
			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
				got, _, err = tx.SignInFailures(k)
				return err
			}))
			if got.Count != 2 {
				t.Errorf("count after two concurrent raises: got %d, want 2", got.Count)
			}
		})
	}
}

// Clearing source counts, batch after batch until none is left, deletes
// every source's count and no account's or pair's; a store with none left
// reports none.
func testClearSourceSignIns(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	u := user("alice")
	must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
	alice := u.Principal
	e := time.Unix(1_900_000_000, 0).UTC()
	var sources []entity.SignInKey
	for i := range 5 {
		sources = append(sources, entity.SignInKey{Source: netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), 32)})
	}
	kept := []entity.SignInKey{{Account: alice}, {Account: alice, Source: sources[0].Source}}
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		for _, k := range append(slices.Clone(sources), kept...) {
			if err := tx.PutSignInFailures(k, entity.SignInFailures{Count: 1, Last: e, Failed: e}); err != nil {
				return err
			}
		}
		return nil
	}))
	for calls, more := 0, true; more; calls++ {
		if calls > len(sources) {
			t.Fatalf("source counts left after %d clears of %d counts, want none", calls, len(sources))
		}
		var err error
		more, err = cp.ClearSourceSignIns(ctx)
		must(t, err)
	}
	for _, k := range sources {
		if _, ok, err := cp.SignInFailures(ctx, k); err != nil || ok {
			t.Errorf("source %v's count after the clear: found %v, %v; want absent", k.Source, ok, err)
		}
	}
	for _, k := range kept {
		if _, ok, err := cp.SignInFailures(ctx, k); err != nil || !ok {
			t.Errorf("count %+v after the clear: found %v, %v; want kept", k, ok, err)
		}
	}
	if more, err := cp.ClearSourceSignIns(ctx); err != nil || more {
		t.Errorf("a clear with no source count left: more %v, %v; want false, nil", more, err)
	}
}
