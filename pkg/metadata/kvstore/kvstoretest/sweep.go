package kvstoretest

import (
	"maps"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// sweepRetention is the audit retention the sweep cases pass.
const sweepRetention = time.Hour

// rowsOf counts the keys of each kind in db.
func rowsOf(t *testing.T, db kv.KV, kinds []byte) map[byte]int {
	t.Helper()
	n := map[byte]int{}
	must(t, db.View(ctx, func(r kv.Reader) error {
		for _, k := range kinds {
			n[k] = 0
			for _, err := range r.Scan([]byte{k}, nil) {
				if err != nil {
					return err
				}
				n[k]++
			}
		}
		return nil
	}))
	return n
}

// sweepAt moves the clock to at, reopens the store so its time follows, and
// runs one Sweep.
func sweepAt(t *testing.T, clk *sim.TestClock, open func() kv.KV, at time.Time) (kv.KV, kvstore.Swept) {
	t.Helper()
	clk.Jump(at.Sub(clk.Now()))
	db := open()
	s, err := kvstore.Open(ctx, db)
	must(t, err)
	sw, err := s.Nodes().Sweep(ctx, sweepRetention)
	must(t, err)
	return db, sw
}

// Each expiring kind outlives a sweep just before its expiry, and one sweep
// just after it deletes the record with every row it had.
func testSweepExpires(t *testing.T, d Durable) {
	alice := entity.NewUser()
	src := netip.MustParsePrefix("192.0.2.1/32")
	for _, c := range []struct {
		name  string
		write func(t *testing.T, cp kvstore.ControlPlane, start time.Time) (expires time.Time)
		kinds []byte
		swept func(kvstore.Swept) int
	}{
		{"idempotency", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			var ran int
			must(t, keyed(cp, alice, "k", "body", "answer", "ng", &ran))
			return start.Add(24 * time.Hour)
		}, []byte{keys.KindIK, keys.KindXP}, func(s kvstore.Swept) int { return s.Idempotency }},
		{"operation", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			op := operation("share.delete", shareRef("a"))
			gen, err := cp.PutOperation(ctx, op, 0)
			must(t, err)
			op.State = entity.OpSucceeded
			must(t, errOf(cp.PutOperation(ctx, op, gen)))
			return start.Add(7 * 24 * time.Hour)
		}, []byte{keys.KindOP, keys.KindXP}, func(s kvstore.Swept) int { return s.Operations }},
		{"session", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: start.Add(time.Hour)}
			must(t, cp.PutSession(ctx, sess))
			for _, h := range [][32]byte{{2}, {3}} {
				sess.Refresh = h
				must(t, cp.UpdateSession(ctx, sess))
			}
			return sess.Expires
		}, []byte{keys.KindSS, keys.KindSSH, keys.KindSSA, keys.KindXP}, func(s kvstore.Swept) int { return s.Sessions }},
		{"token", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			tok := entity.APIToken{ID: uuid.New(), Account: alice, Hash: [32]byte{7}, Expires: start.Add(2 * time.Hour)}
			must(t, cp.PutAPIToken(ctx, tok))
			return tok.Expires
		}, []byte{keys.KindAT, keys.KindATH, keys.KindATA, keys.KindXP}, func(s kvstore.Swept) int { return s.Tokens }},
		{"source count", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			putSignIn(t, cp, entity.SignInKey{Source: src}, entity.SignInFailures{Count: 1, Last: start, Failed: start})
			return start.Add(15 * time.Minute)
		}, []byte{keys.KindSF, keys.KindXP}, func(s kvstore.Swept) int { return s.SignIns }},
		{"pair count", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			putSignIn(t, cp, entity.SignInKey{Account: alice, Source: src}, entity.SignInFailures{Count: 1, Last: start, Failed: start})
			return start.Add(24 * time.Hour)
		}, []byte{keys.KindSF, keys.KindXP}, func(s kvstore.Swept) int { return s.SignIns }},
		{"audit", func(t *testing.T, cp kvstore.ControlPlane, start time.Time) time.Time {
			must(t, cp.Write(ctx, entity.WriteMeta{Audit: &entity.AuditRecord{Actor: alice, Target: shareRef("a")}},
				func(entity.ControlPlaneTx) error { return nil }))
			return start.Add(sweepRetention)
		}, []byte{keys.KindAU, keys.KindAUX}, func(s kvstore.Swept) int { return s.Audit }},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock()
			open := d(t, clk.Now)
			db := open()
			must(t, kvstore.Init(ctx, db, uuid.New()))
			s, err := kvstore.Open(ctx, db)
			must(t, err)
			expires := c.write(t, s.ControlPlane(), clk.Now())
			written := rowsOf(t, db, c.kinds)
			for kind, n := range written {
				if n == 0 {
					t.Fatalf("%s rows of kind %#x written: got 0, want some", c.name, kind)
				}
			}

			db, sw := sweepAt(t, clk, open, expires.Add(-time.Microsecond))
			if sw != (kvstore.Swept{}) {
				t.Errorf("sweep just before the %s expires: got %+v, want nothing swept", c.name, sw)
			}
			if got := rowsOf(t, db, c.kinds); !maps.Equal(got, written) {
				t.Errorf("%s rows after a sweep before its expiry: got %v, want %v", c.name, got, written)
			}

			db, sw = sweepAt(t, clk, open, expires.Add(time.Microsecond))
			if c.swept(sw) != 1 || sw.More {
				t.Errorf("sweep just after the %s expires: got %+v, want one swept and no more", c.name, sw)
			}
			for kind, n := range rowsOf(t, db, c.kinds) {
				if n != 0 {
					t.Errorf("%s rows of kind %#x after its sweep: got %d, want 0", c.name, kind, n)
				}
			}
		})
	}
}

func putSignIn(t *testing.T, cp kvstore.ControlPlane, k entity.SignInKey, f entity.SignInFailures) {
	t.Helper()
	must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.PutSignInFailures(k, f) }))
}

// A refresh that commits inside a sweep's attempt, after the sweep found its
// session expired, keeps the session, and the counts the sweep reports are
// its committed attempt's alone: the sweep is one transaction, which the
// refresh conflicts, and a retry starts its counts again. The stale-row rule
// itself is not what this guards, since the retry finds the row moved and
// never reads it as due.
func testSweepRacesRefresh(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			sess := entity.Session{ID: uuid.New(), Account: entity.NewUser(), Refresh: [32]byte{1}, Expires: time.Unix(1, 0).UTC()}
			must(t, cp.PutSession(ctx, sess))
			var sw kvstore.Swept
			outer, inner := race(h,
				func() (err error) { sw, err = s.Nodes().Sweep(ctx, sweepRetention); return err },
				func() error {
					sess.Refresh, sess.Expires = [32]byte{2}, time.Unix(4_000_000_000, 0).UTC()
					return cp.UpdateSession(ctx, sess)
				})
			must(t, inner)
			must(t, outer)
			if sw.Sessions != 0 {
				t.Errorf("sessions swept with a refresh racing the sweep: got %d, want 0", sw.Sessions)
			}
			if got, ok, err := cp.SessionByRefresh(ctx, [32]byte{2}); err != nil || !ok || got != sess {
				t.Errorf("refreshed session after the sweep: got %+v, %v, %v; want %+v", got, ok, err, sess)
			}
		})
	}
}

// An expiry row its record no longer expires at, or naming no record, is
// deleted alone, whatever the kind; a sweep refuses a retention that would
// delete every audit record.
func testSweepStaleRows(t *testing.T, kvo Opener) {
	s, db := newStore(t, kvo)
	cp := s.ControlPlane()
	alice := entity.NewUser()
	sess := entity.Session{ID: uuid.New(), Account: alice, Refresh: [32]byte{1}, Expires: time.Unix(4_000_000_000, 0).UTC()}
	must(t, cp.PutSession(ctx, sess))
	pair := entity.SignInKey{Account: alice, Source: netip.MustParsePrefix("192.0.2.1/32")}
	putSignIn(t, cp, pair, entity.SignInFailures{Count: 1, Last: time.Unix(4_000_000_000, 0), Failed: time.Unix(4_000_000_000, 0)})
	var ran int
	must(t, keyed(cp, alice, "k", "body", "answer", "ng", &ran))
	row := func(tag byte, tail []byte) []byte {
		return keys.ID(append(keys.Uint64([]byte{keys.KindXP}, uint64(time.Unix(1, 0).UnixNano())), tag), tail)
	}
	gone := uuid.New()
	moved := [][]byte{
		row(keys.XPSession, sess.ID[:]),
		row(keys.XPToken, gone[:]),
		row(keys.XPPair, append(alice.ID[:], 192, 0, 2, 1, 32)),
		keys.String(row(keys.XPIdempotency, alice.ID[:]), []byte("k")),
	}
	must(t, db.Update(ctx, func(tx kv.Txn) error {
		for _, r := range moved {
			if err := tx.Set(r, nil); err != nil {
				return err
			}
		}
		return nil
	}))
	sw, err := s.Nodes().Sweep(ctx, sweepRetention)
	must(t, err)
	if sw != (kvstore.Swept{}) {
		t.Errorf("sweep of stale rows: got %+v, want nothing counted", sw)
	}
	if got, ok, err := cp.Session(ctx, sess.ID); err != nil || !ok || got != sess {
		t.Errorf("session whose stale row was swept: got %+v, %v, %v; want %+v", got, ok, err, sess)
	}
	if got, ok, err := cp.SignInFailures(ctx, pair); err != nil || !ok || got.Count != 1 {
		t.Errorf("pair count whose stale row was swept: got %+v, %v, %v; want its count of 1", got, ok, err)
	}
	if got := rowsOf(t, db, []byte{keys.KindIK})[keys.KindIK]; got != 1 {
		t.Errorf("idempotency records after a sweep of a stale row: got %d, want 1", got)
	}
	if got := rowsOf(t, db, []byte{keys.KindXP})[keys.KindXP]; got != 3 {
		t.Errorf("expiry rows after the sweep: got %d, want the session's, the pair count's and the idempotency record's own", got)
	}
	if _, err := s.Nodes().Sweep(ctx, 0); !isCode(err, merrs.ErrInvalidArgument) {
		t.Errorf("sweep with no audit retention: %v, want invalid argument", err)
	}
}
