package kvstore

import (
	"bytes"
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

func checkRows(t *testing.T, what string, got [][]byte, want ...[]byte) {
	t.Helper()
	if !slices.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("%s: got %x, want %x", what, got, want)
	}
}

// storedUser stores a user for a session, a token or a count to name.
func storedUser(t *testing.T, s *Store) entity.Principal {
	t.Helper()
	u := entity.User{Principal: entity.NewUser(), Name: "alice"}
	if _, err := s.ControlPlane().PutUser(context.Background(), u, 0, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	return u.Principal
}

// A sign-in count's expiry row follows the time its expiry counts from: a
// source's Failed, not its Last; a pair's Last. An account's count has none,
// and a deleted count leaves none.
func TestSignInExpiryRowMoves(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	alice := storedUser(t, s)
	addr := netip.MustParsePrefix("192.0.2.1/32")
	tail := append(addr.Addr().AsSlice(), 32)
	t0 := time.Unix(1_800_000_000, 0).UTC()
	put := func(k entity.SignInKey, f entity.SignInFailures) {
		t.Helper()
		if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.PutSignInFailures(k, f) }); err != nil {
			t.Fatal(err)
		}
	}
	xp := []byte{keys.KindXP}

	source := entity.SignInKey{Source: addr}
	put(source, entity.SignInFailures{Count: 1, Last: t0, Failed: t0})
	checkRows(t, "source created", keysUnder(t, s, xp), expiryKey(t0.Add(15*time.Minute), keys.XPSource, tail))
	put(source, entity.SignInFailures{Count: 2, Last: t0.Add(time.Minute), Failed: t0})
	checkRows(t, "source's Last moved", keysUnder(t, s, xp), expiryKey(t0.Add(15*time.Minute), keys.XPSource, tail))
	put(source, entity.SignInFailures{Count: 2, Last: t0.Add(time.Minute), Failed: t0.Add(2 * time.Minute)})
	checkRows(t, "source's Failed moved", keysUnder(t, s, xp), expiryKey(t0.Add(17*time.Minute), keys.XPSource, tail))
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.DeleteSignInFailures(source) }); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "source deleted", keysUnder(t, s, xp))

	pair := entity.SignInKey{Account: alice, Source: addr}
	pairTail := append(alice.ID[:], tail...)
	put(pair, entity.SignInFailures{Count: 1, Last: t0})
	checkRows(t, "pair created", keysUnder(t, s, xp), expiryKey(t0.Add(24*time.Hour), keys.XPPair, pairTail))
	put(pair, entity.SignInFailures{Count: 2, Last: t0.Add(5 * time.Minute)})
	checkRows(t, "pair's Last moved", keysUnder(t, s, xp), expiryKey(t0.Add(24*time.Hour+5*time.Minute), keys.XPPair, pairTail))
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.DeleteSignInFailures(pair) }); err != nil {
		t.Fatal(err)
	}

	put(entity.SignInKey{Account: alice}, entity.SignInFailures{Count: 1, Last: t0, Failed: t0})
	checkRows(t, "account's count and nothing else", keysUnder(t, s, xp))
}

// A session's and a token's expiry rows sit at their Expires, a session's
// moving with it; deleted, each leaves no row, a session none of its
// refresh hashes either.
func TestSessionAndTokenRows(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	alice := storedUser(t, s)
	e1 := time.Unix(1_900_000_000, 0).UTC()
	sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: e1}
	xp := []byte{keys.KindXP}
	if err := cp.PutSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session created", keysUnder(t, s, xp), expiryKey(e1, keys.XPSession, sess.ID[:]))
	sess.Refresh, sess.Expires = [32]byte{2}, e1.Add(time.Hour)
	if err := cp.UpdateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session's expiry moved", keysUnder(t, s, xp), expiryKey(e1.Add(time.Hour), keys.XPSession, sess.ID[:]))
	checkRows(t, "session's refresh hashes", keysUnder(t, s, sessionHashesPrefix(sess.ID)),
		sessionHashKey(sess.ID, [32]byte{1}), sessionHashKey(sess.ID, [32]byte{2}))
	if err := cp.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session deleted", keysUnder(t, s, xp))
	checkRows(t, "deleted session's own rows", keysUnder(t, s, sessionKey(sess.ID)))
	checkRows(t, "refresh index after the deletion", keysUnder(t, s, []byte{keys.KindSSH}))

	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{3}, Expires: e1}
	if err := cp.PutAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "token created", keysUnder(t, s, xp), expiryKey(e1, keys.XPToken, tok.ID[:]))
	if err := cp.DeleteAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "token deleted", keysUnder(t, s, xp))
	for _, kind := range []byte{keys.KindAT, keys.KindATH, keys.KindATA} {
		checkRows(t, "deleted token's "+keys.Describe([]byte{kind}), keysUnder(t, s, []byte{kind}))
	}
}

// ClearSourceSignIns deletes at most half its key budget of source counts
// per call, each with its expiry row: three counts under a budget of four
// keys take two calls, the first reporting more left and the second none.
func TestClearSourceSignInsBatches(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	s.SetKeyBudget(4)
	cp := s.ControlPlane()
	t0 := time.Unix(1_800_000_000, 0).UTC()
	for i := range 3 {
		k := entity.SignInKey{Source: netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), 32)}
		if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tx.PutSignInFailures(k, entity.SignInFailures{Count: 1, Last: t0, Failed: t0})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []struct {
		more bool
		left int
	}{{true, 1}, {false, 0}} {
		more, err := cp.ClearSourceSignIns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		left := len(keysUnder(t, s, []byte{keys.KindSF, keys.SFSource}))
		rows := len(keysUnder(t, s, []byte{keys.KindXP}))
		if more != want.more || left != want.left || rows != want.left {
			t.Errorf("call %d: more %v, %d counts and %d expiry rows left; want more %v, %d and %d",
				i+1, more, left, rows, want.more, want.left, want.left)
		}
	}
}
