package kvstore

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// rowsUnder is every key under prefix.
func rowsUnder(t *testing.T, s *Store, prefix []byte) [][]byte {
	t.Helper()
	var rows [][]byte
	if err := s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			rows = append(rows, bytes.Clone(row.Key))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func checkRows(t *testing.T, what string, got [][]byte, want ...[]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d rows %x, want %x", what, len(got), got, want)
		return
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s: got %x, want %x", what, got, want)
			return
		}
	}
}

// A sign-in count's expiry row follows the time its expiry counts from: a
// source's Failed, not its Last; a pair's Last. An account's count has none,
// and a deleted count leaves none.
func TestSignInExpiryRowMoves(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	alice := entity.NewUser()
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
	checkRows(t, "source created", rowsUnder(t, s, xp), expiryKey(t0.Add(15*time.Minute), keys.XPSource, tail))
	put(source, entity.SignInFailures{Count: 2, Last: t0.Add(time.Minute), Failed: t0})
	checkRows(t, "source's Last moved", rowsUnder(t, s, xp), expiryKey(t0.Add(15*time.Minute), keys.XPSource, tail))
	put(source, entity.SignInFailures{Count: 2, Last: t0.Add(time.Minute), Failed: t0.Add(2 * time.Minute)})
	checkRows(t, "source's Failed moved", rowsUnder(t, s, xp), expiryKey(t0.Add(17*time.Minute), keys.XPSource, tail))
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.DeleteSignInFailures(source) }); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "source deleted", rowsUnder(t, s, xp))

	pair := entity.SignInKey{Account: alice, Source: addr}
	pairTail := append(alice.ID[:], tail...)
	put(pair, entity.SignInFailures{Count: 1, Last: t0})
	checkRows(t, "pair created", rowsUnder(t, s, xp), expiryKey(t0.Add(24*time.Hour), keys.XPPair, pairTail))
	put(pair, entity.SignInFailures{Count: 2, Last: t0.Add(5 * time.Minute)})
	checkRows(t, "pair's Last moved", rowsUnder(t, s, xp), expiryKey(t0.Add(24*time.Hour+5*time.Minute), keys.XPPair, pairTail))
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.DeleteSignInFailures(pair) }); err != nil {
		t.Fatal(err)
	}

	put(entity.SignInKey{Account: alice}, entity.SignInFailures{Count: 1, Last: t0, Failed: t0})
	checkRows(t, "account's count and nothing else", rowsUnder(t, s, xp))
}

// A session's and a token's expiry rows sit at their Expires, a session's
// moving with it; deleted, each leaves no row, a session none of its
// refresh hashes either.
func TestSessionAndTokenRows(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	alice := entity.NewUser()
	e1 := time.Unix(1_900_000_000, 0).UTC()
	sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: e1}
	xp := []byte{keys.KindXP}
	if err := cp.PutSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session created", rowsUnder(t, s, xp), expiryKey(e1, keys.XPSession, sess.ID[:]))
	sess.Refresh, sess.Expires = [32]byte{2}, e1.Add(time.Hour)
	if err := cp.PutSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session's expiry moved", rowsUnder(t, s, xp), expiryKey(e1.Add(time.Hour), keys.XPSession, sess.ID[:]))
	checkRows(t, "session's refresh hashes", rowsUnder(t, s, sessionHashesPrefix(sess.ID)),
		sessionHashKey(sess.ID, [32]byte{1}), sessionHashKey(sess.ID, [32]byte{2}))
	if err := cp.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "session deleted", rowsUnder(t, s, xp))
	checkRows(t, "deleted session's own rows", rowsUnder(t, s, sessionKey(sess.ID)))
	checkRows(t, "refresh index after the deletion", rowsUnder(t, s, []byte{keys.KindSSH}))

	tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{3}, Expires: e1}
	if err := cp.PutAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "token created", rowsUnder(t, s, xp), expiryKey(e1, keys.XPToken, tok.ID[:]))
	if err := cp.DeleteAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	checkRows(t, "token deleted", rowsUnder(t, s, xp))
	for _, kind := range []byte{keys.KindAT, keys.KindATH, keys.KindATA} {
		checkRows(t, "deleted token's "+keys.Describe([]byte{kind}), rowsUnder(t, s, []byte{kind}))
	}
}
