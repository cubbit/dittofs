package kvstoretest

import (
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// A session's refresh token rotated twice is found by each of its three
// hashes, the current one reported as Refresh; deleted, it is found by none,
// nor by its ID, nor in its account's list.
func testSessionRefreshIndex(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := entity.NewUser()
	sess := entity.Session{ID: uuid.New(), Account: alice, Expires: time.Unix(1_900_000_000, 0).UTC()}
	hashes := [][32]byte{{1}, {2}, {3}}
	for _, h := range hashes {
		sess.Refresh = h
		must(t, cp.PutSession(ctx, sess))
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
	if got := all(t, cp.Sessions(ctx, alice)); len(got) != 0 {
		t.Errorf("alice's sessions after the deletion: got %d, want 0", len(got))
	}
	if err := cp.DeleteSession(ctx, sess.ID); err != nil {
		t.Errorf("a deleted session deleted again: %v, want nil", err)
	}
}

// A token is found by its hash and listed under its account until it is
// deleted, and is never rewritten.
func testAPITokens(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := entity.NewUser()
	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{7},
		Created: time.Unix(1_800_000_000, 0).UTC(), Expires: time.Unix(1_900_000_000, 0).UTC()}
	must(t, cp.PutAPIToken(ctx, tok))
	if got, ok, err := cp.APITokenByHash(ctx, tok.Hash); err != nil || !ok || got != tok {
		t.Errorf("token by hash: got %+v, %v, %v; want %+v", got, ok, err, tok)
	}
	if got := all(t, cp.APITokens(ctx, alice)); len(got) != 1 || got[0] != tok {
		t.Errorf("alice's tokens: got %+v, want only %+v", got, tok)
	}
	again := tok
	again.Hash = [32]byte{8}
	if err := cp.PutAPIToken(ctx, again); !isCode(err, merrs.ErrAlreadyExists) {
		t.Errorf("token written again under its ID: %v, want already exists", err)
	}
	if _, ok, _ := cp.APITokenByHash(ctx, again.Hash); ok {
		t.Errorf("refused token's hash: found, want absent")
	}

	must(t, cp.DeleteAPIToken(ctx, tok.ID))
	if _, ok, err := cp.APITokenByHash(ctx, tok.Hash); err != nil || ok {
		t.Errorf("deleted token by hash: found %v, %v; want absent", ok, err)
	}
	if got := all(t, cp.APITokens(ctx, alice)); len(got) != 0 {
		t.Errorf("alice's tokens after the deletion: got %d, want 0", len(got))
	}
}

// A sign-in count is read and raised in one Write, by account, source or
// pair, each its own count; a key naming neither is refused; a deleted count
// reads as absent.
func testSignInFailures(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := entity.NewUser()
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
		if f, ok := read(keys[name]); !ok || f.Count != uint32(i+1) {
			t.Errorf("%s's count: got %d, found %v; want %d", name, f.Count, ok, i+1)
		}
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
