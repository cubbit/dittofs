package kvstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// An expiry row is due at the store time it names and after; an audit record
// only once it is strictly older than the retention.
func TestSweepDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const ret = time.Hour
	for _, c := range []struct {
		name      string
		got, want bool
	}{
		{"expiry a nanosecond before now", xpDue(now.Add(-time.Nanosecond), now), true},
		{"expiry at now", xpDue(now, now), true},
		{"expiry a nanosecond after now", xpDue(now.Add(time.Nanosecond), now), false},
		{"audit a nanosecond older than retention", auditDue(now.Add(-ret-time.Nanosecond), now, ret), true},
		{"audit exactly retention old", auditDue(now.Add(-ret), now, ret), false},
		{"audit a nanosecond younger than retention", auditDue(now.Add(-ret+time.Nanosecond), now, ret), false},
	} {
		if c.got != c.want {
			t.Errorf("%s: got due %v, want %v", c.name, c.got, c.want)
		}
	}
}

// sweepFix is a store whose writers put records already due under the
// store's real clock, the expiry index's 1970.
type sweepFix struct {
	t *testing.T
	s *Store
}

func (f sweepFix) write(fn func(entity.ControlPlaneTx) error) {
	f.t.Helper()
	if err := f.s.ControlPlane().Write(context.Background(), entity.WriteMeta{}, fn); err != nil {
		f.t.Fatal(err)
	}
}

// counts writes n source counts due at 1970-01-01 00:15:01.
func (f sweepFix) counts(n int) {
	f.write(func(tx entity.ControlPlaneTx) error {
		for i := range n {
			k := entity.SignInKey{Source: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), 32)}
			if err := tx.PutSignInFailures(k, entity.SignInFailures{Count: 1, Last: time.Unix(1, 0), Failed: time.Unix(1, 0)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// tokens writes n tokens due at 1970-01-01 01:00.
func (f sweepFix) tokens(n int) {
	account := storedUser(f.t, f.s)
	f.write(func(tx entity.ControlPlaneTx) error {
		for i := range n {
			tok := entity.APIToken{ID: uuid.New(), Account: account, Hash: [32]byte{byte(i + 1)}, Expires: time.Unix(3600, 0)}
			if err := tx.PutAPIToken(tok); err != nil {
				return err
			}
		}
		return nil
	})
}

// audit writes an audit record at 1970-01-01 00:00:01, past any retention.
func (f sweepFix) audit() {
	a := entity.AuditRecord{Time: time.Unix(1, 0), ID: uuid.New(), Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("a")}}
	pos := auditPos(a.Time, a.ID)
	kvs := [][]byte{auditKey(a.Time, a.ID), encodeAudit(a)}
	for _, k := range auditIndexKeys(a, pos) {
		kvs = append(kvs, k, nil)
	}
	rows(f.t, f.s, kvs...)
}

// session writes a session due at 1970-01-01 02:00 that has had n refresh
// hashes.
func (f sweepFix) session(n int) entity.Session {
	s := entity.Session{ID: uuid.New(), Account: storedUser(f.t, f.s), Expires: time.Unix(7200, 0).UTC()}
	f.write(func(tx entity.ControlPlaneTx) error {
		s.Refresh = [32]byte{1}
		if err := tx.PutSession(s); err != nil {
			return err
		}
		for i := 2; i <= n; i++ {
			s.Refresh = [32]byte{byte(i)}
			if err := tx.UpdateSession(s); err != nil {
				return err
			}
		}
		return nil
	})
	return s
}

func (f sweepFix) rows(kinds ...byte) int {
	f.t.Helper()
	n := 0
	err := f.s.kv.View(context.Background(), func(r kv.Reader) error {
		for _, k := range kinds {
			for _, err := range r.Scan([]byte{k}, nil) {
				if err != nil {
					return err
				}
				n++
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// A call deletes due records while their keys fit the batch's budget, the
// first always, and leaves the rest, More set, to the next; a session larger
// than a whole batch loses refresh hashes first and is deleted once the rest
// fit, its record last.
func TestSweepKeyBudget(t *testing.T) {
	const k = 8
	kinds := []byte{keys.KindXP, keys.KindSF, keys.KindAT, keys.KindATH, keys.KindATA,
		keys.KindSS, keys.KindSSH, keys.KindSSA, keys.KindAU, keys.KindAUX}
	for _, c := range []struct {
		name  string
		write func(sweepFix)
		calls []Swept
	}{
		{"counts filling the budget", func(f sweepFix) { f.counts(k / countCost) },
			[]Swept{{SignIns: k / countCost}}},
		{"a count past the budget", func(f sweepFix) { f.counts(k/countCost + 1) },
			[]Swept{{SignIns: k / countCost, More: true}, {SignIns: 1}}},
		{"an audit record past the budget", func(f sweepFix) { f.counts(k / countCost); f.audit() },
			[]Swept{{SignIns: k / countCost, More: true}, {Audit: 1}}},
		{"a token past what is left", func(f sweepFix) { f.counts(1); f.tokens(2) },
			[]Swept{{SignIns: 1, Tokens: 1, More: true}, {Tokens: 1}}},
		{"a session past the whole budget", func(f sweepFix) { f.session(6) },
			[]Swept{{More: true}, {Sessions: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := sweepFix{t, openStore(t)}
			f.s.SetKeyBudget(k)
			c.write(f)
			for i, want := range c.calls {
				got, err := f.s.Nodes().Sweep(context.Background(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("sweep %d: got %+v, want %+v", i+1, got, want)
				}
			}
			if n := f.rows(kinds...); n != 0 {
				t.Errorf("rows left after %d sweeps: got %d, want 0", len(c.calls), n)
			}
		})
	}
}

// A session trimmed by a call keeps its record and its expiry row, so the
// next call still finds and finishes it.
func TestSweepTrimKeepsSession(t *testing.T) {
	f := sweepFix{t, openStore(t)}
	f.s.SetKeyBudget(8)
	s := f.session(6)
	if _, err := f.s.Nodes().Sweep(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	got, found, err := f.s.ControlPlane().Session(context.Background(), s.ID)
	if err != nil || !found || got != s {
		t.Errorf("session after a call trimmed it: got %+v, %v, %v; want %+v", got, found, err, s)
	}
	if n := f.rows(keys.KindXP); n != 1 {
		t.Errorf("expiry rows after a call trimmed the session: got %d, want its own", n)
	}
}

// An expiry row naming a running operation at the time its end would expire
// it is stale: the row goes, the operation stays.
func TestSweepRunningOperation(t *testing.T) {
	f := sweepFix{t, openStore(t)}
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: "share.delete", State: entity.OpRunning,
		Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("a")}, Started: time.Unix(1, 0), Updated: time.Unix(1, 0)}
	rows(t, f.s, operationKey(op.ID), encodeOperation(op), operationExpiryKey(op), nil)
	sw, err := f.s.Nodes().Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sw != (Swept{}) {
		t.Errorf("sweep of a running operation's row: got %+v, want nothing counted", sw)
	}
	var found bool
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		_, found, err = operationIn(r, op.ID)
		return err
	}); err != nil || !found {
		t.Errorf("running operation after the sweep: found %v, %v; want found", found, err)
	}
	if n := f.rows(keys.KindXP); n != 0 {
		t.Errorf("expiry rows after the sweep: got %d, want the stale one gone", n)
	}
}

// An expiry row of no known tag stops every call on it, reported as
// SweepRowUndecodable, and a call that stops reports none of the deletions
// its attempt made before.
func TestSweepRowUndecodable(t *testing.T) {
	f := sweepFix{t, openStore(t)}
	f.counts(1)
	id := uuid.New()
	rows(t, f.s, keys.ID(append(keys.Uint64([]byte{keys.KindXP}, uint64(time.Unix(3600, 0).UnixNano())), 0xFF), id[:]), nil)
	for i := range 2 {
		sw, err := f.s.Nodes().Sweep(context.Background(), time.Hour)
		if !SweepRowUndecodable(err) {
			t.Errorf("sweep %d over a row tagged 0xFF: got %v, want SweepRowUndecodable", i+1, err)
		}
		if sw != (Swept{}) {
			t.Errorf("sweep %d that stopped: got %+v, want nothing reported", i+1, sw)
		}
	}
	if n := f.rows(keys.KindSF); n != 1 {
		t.Errorf("counts after sweeps that stopped: got %d, want the one rolled back", n)
	}
}
