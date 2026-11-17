package kvstoretest

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

var instScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// audits is every audit record f selects.
func audits(t *testing.T, cp kvstore.ControlPlane, f entity.AuditFilter) []entity.AuditRecord {
	t.Helper()
	return all(t, cp.AuditRecords(ctx, f, nil))
}

// A write's change and its audit record commit together: with the audit
// record's write failed, neither is there; without the fault, both are.
func testWriteAllOrNothing(t *testing.T, kvo Opener) {
	f := kvtest.NewFaultKV(kvo(t))
	must(t, kvstore.Init(ctx, f, uuid.New()))
	s, err := kvstore.Open(ctx, f)
	must(t, err)
	cp := s.ControlPlane()
	u := user("alice")
	write := func() error {
		return cp.Write(ctx, entity.WriteMeta{Audit: &entity.AuditRecord{Source: "10.0.0.1", Target: entity.ResourceRef{Kind: entity.ResourceUser}}},
			func(tx entity.ControlPlaneTx) error { return errOf(tx.PutUser(u, 0, entity.Alloc{})) })
	}
	failed := errors.New("audit write failed")
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "AU‖*", Action: sim.FaultAction{Err: failed}})
	must(t, err)
	f.SetFault(sched)
	if err := write(); !errors.Is(err, failed) {
		t.Fatalf("write with its audit record failed: %v, want %v", err, failed)
	}
	if _, err := s.Principals().User(ctx, u.Principal); err == nil {
		t.Errorf("user after a write whose audit record failed: found, want absent")
	}
	if got := audits(t, cp, entity.AuditFilter{}); len(got) != 0 {
		t.Errorf("audit records after a failed write: got %d, want 0", len(got))
	}
	f.SetFault(nil)
	must(t, write())
	if _, err := s.Principals().User(ctx, u.Principal); err != nil {
		t.Errorf("user after a write that committed: %v, want found", err)
	}
	if got := audits(t, cp, entity.AuditFilter{}); len(got) != 1 || got[0].Source != "10.0.0.1" {
		t.Errorf("audit records after a write that committed: got %+v, want one from 10.0.0.1", got)
	}
}

// What fn sets in meta's audit record is what the record holds, and a field
// set only by an attempt that conflicted is not: each attempt starts from
// the record as it was passed in.
func testWriteRestoresMeta(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte("1")}, 0)))
			meta := entity.WriteMeta{Audit: &entity.AuditRecord{Source: "client", Target: entity.ResourceRef{Kind: entity.ResourceNetgroup}}}
			attempts := 0
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
						attempts++
						if _, _, err := tx.Setting(instScope, "k"); err != nil {
							return err
						}
						if attempts == 1 {
							meta.Audit.Change = []byte("first attempt only")
						}
						meta.Audit.After, meta.Audit.Outcome = 7, []byte("applied")
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte("2")}, settingGen(t, cp)))
				})
			must(t, inner)
			must(t, outer)
			if attempts != 2 {
				t.Fatalf("attempts of a write whose read moved: got %d, want 2", attempts)
			}
			got := audits(t, cp, entity.AuditFilter{})
			if len(got) != 1 {
				t.Fatalf("audit records: got %d, want 1", len(got))
			}
			if got[0].Change != nil {
				t.Errorf("audit change set only by the conflicted attempt: got %q, want unset", got[0].Change)
			}
			if got[0].After != 7 || string(got[0].Outcome) != "applied" || got[0].Source != "client" {
				t.Errorf("audit after, outcome, source: got %d %q %q, want 7 applied client", got[0].After, got[0].Outcome, got[0].Source)
			}
		})
	}
}

func settingGen(t *testing.T, cp kvstore.ControlPlane) uint64 {
	t.Helper()
	for got, err := range cp.Settings(ctx, instScope) {
		must(t, err)
		if got.Key == "k" {
			return got.Generation
		}
	}
	return 0
}

// A setting read through the transaction is tracked: a change of it that
// commits first makes the write run again, and the attempt that commits
// reads the new value.
func testWriteTrackedRead(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte("old")}, 0)))
			var seen string
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						st, _, err := tx.Setting(instScope, "k")
						seen = string(st.Value)
						if err != nil {
							return err
						}
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte("new")}, settingGen(t, cp)))
				})
			must(t, inner)
			must(t, outer)
			if seen != "new" {
				t.Errorf("setting read by the attempt that committed: got %q, want %q", seen, "new")
			}
		})
	}
}

// The audit is read by store time, and narrowed by actor, the schedule's
// zero actor included, by target and by taking node, each in store-time
// order and holding exactly the records that name it.
func testAuditReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice, bob := entity.NewUser(), entity.NewUser()
	n1, n2 := entity.NodeID{15: 1}, entity.NodeID{15: 2}
	photos := entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("photos")}
	docs := entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("docs")}
	written := []entity.AuditRecord{
		{Actor: alice, ActorName: "alice", Credential: "session 1", Node: n1, Target: photos},
		{Actor: bob, Node: n2, Target: docs},
		{Actor: alice, Node: n2, Target: docs},
		{Node: n1, Target: photos}, // the schedule
		{Actor: bob, Node: n1, Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte("photos")}},
	}
	for i := range written {
		written[i].Source = string(rune('a' + i))
		must(t, cp.Write(ctx, entity.WriteMeta{Audit: &written[i]}, func(entity.ControlPlaneTx) error { return nil }))
	}
	sources := func(rs []entity.AuditRecord) string {
		var b []byte
		for _, r := range rs {
			b = append(b, r.Source...)
		}
		return string(b)
	}
	schedule := entity.Principal{}
	for _, c := range []struct {
		name string
		f    entity.AuditFilter
		want string
	}{
		{"all", entity.AuditFilter{}, "abcde"},
		{"actor", entity.AuditFilter{Actor: &alice}, "ac"},
		{"schedule", entity.AuditFilter{Actor: &schedule}, "d"},
		{"target", entity.AuditFilter{Target: &photos}, "ad"},
		{"node", entity.AuditFilter{Node: &n2}, "bc"},
		{"actor and node", entity.AuditFilter{Actor: &bob, Node: &n1}, "e"},
		{"time range", entity.AuditFilter{From: written[1].Time, To: written[3].Time}, "bc"},
		{"node from a time", entity.AuditFilter{Node: &n1, From: written[1].Time}, "de"},
	} {
		if got := sources(audits(t, cp, c.f)); got != c.want {
			t.Errorf("audit by %s: got records %q, want %q", c.name, got, c.want)
		}
	}
	after := kvstore.AuditPosition(written[2])
	if got := sources(all(t, cp.AuditRecords(ctx, entity.AuditFilter{}, after))); got != "de" {
		t.Errorf("audit after the third record's position: got %q, want %q", got, "de")
	}
	if got := audits(t, cp, entity.AuditFilter{Actor: &alice}); len(got) == 0 || got[0].ActorName != "alice" || got[0].Credential != "session 1" {
		t.Errorf("actor's name and credential read back: got %+v, want alice under session 1 first", got)
	}
	for _, kind := range []entity.ResourceKind{0, entity.ResourceSession + 1} {
		a := &entity.AuditRecord{Target: entity.ResourceRef{Kind: kind}}
		if err := cp.Write(ctx, entity.WriteMeta{Audit: a}, func(entity.ControlPlaneTx) error { return nil }); !isCode(err, merrs.ErrInvalidArgument) {
			t.Errorf("audit record targeting kind %d: %v, want invalid argument", kind, err)
		}
	}
	if got := len(audits(t, cp, entity.AuditFilter{})); got != len(written) {
		t.Errorf("audit records after refused ones: got %d, want %d", got, len(written))
	}
}

// Two writes that each take away one of the last two enabled admins, each
// refusing to leave none, conflict on the count: exactly one commits,
// whether it demotes, disables or deletes.
func testLastAdminRace(t *testing.T, kvo Opener) {
	errLast := errors.New("would leave no enabled admin")
	for _, b := range kvo.backends() {
		for _, c := range []struct {
			name   string
			remove func(tx entity.ControlPlaneTx, u entity.User) error
		}{
			{"demote", func(tx entity.ControlPlaneTx, u entity.User) error {
				gen := u.Generation
				u.Role = entity.RoleOperator
				return errOf(tx.PutUser(u, gen, entity.Alloc{}))
			}},
			{"disable", func(tx entity.ControlPlaneTx, u entity.User) error {
				gen := u.Generation
				u.Disabled = true
				return errOf(tx.PutUser(u, gen, entity.Alloc{}))
			}},
			{"delete", func(tx entity.ControlPlaneTx, u entity.User) error {
				return tx.DeleteUser(u.Principal, u.Generation)
			}},
		} {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				s, h := heldStore(t, b)
				cp := s.ControlPlane()
				admins := []entity.User{user("a"), user("b")}
				for _, u := range admins {
					u.Role = entity.RoleAdmin
					must(t, errOf(cp.PutUser(ctx, u, 0, entity.Alloc{})))
				}
				remove := func(p entity.Principal) error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						n, err := tx.EnabledAdmins()
						if err != nil {
							return err
						}
						if n <= 1 {
							return errLast
						}
						u, _, err := tx.User(p)
						if err != nil {
							return err
						}
						return c.remove(tx, u)
					})
				}
				outer, inner := race(h, func() error { return remove(admins[0].Principal) },
					func() error { return remove(admins[1].Principal) })
				must(t, inner)
				if !errors.Is(outer, errLast) {
					t.Errorf("the second removal of the last two admins: %v, want %v", outer, errLast)
				}
				must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
					if n, err := tx.EnabledAdmins(); err != nil || n != 1 {
						t.Errorf("enabled admins after the race: got %d, %v; want 1", n, err)
					}
					return nil
				}))
			})
		}
	}
}

// A write that keeps a user an enabled admin, a rename, leaves the
// enabled-admin index unwritten, so a concurrent count of it commits on its
// first attempt.
func testAdminRenameKeepsCount(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			a := user("a")
			a.Role = entity.RoleAdmin
			gen, err := cp.PutUser(ctx, a, 0, entity.Alloc{})
			must(t, err)
			attempts := 0
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						attempts++
						if _, err := tx.EnabledAdmins(); err != nil {
							return err
						}
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					a.Name = "a2"
					return errOf(cp.PutUser(ctx, a, gen, entity.Alloc{}))
				})
			must(t, inner)
			must(t, outer)
			if attempts != 1 {
				t.Errorf("attempts of a count raced by an admin's rename: got %d, want 1", attempts)
			}
		})
	}
}

// The installation is read tracked: a concurrent turn-off of NTLM that
// commits first makes the write run again and read it. Turning NTLM off
// clears a completed NT-hash deletion; the machine join and the machine
// password read back as set, neither write clearing the other.
func testInstallationInWrite(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			op := entity.OperationID{15: 9}
			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetNTHashDeleted(op) }))
			var seen entity.Installation
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						var err error
						if seen, err = tx.Installation(); err != nil {
							return err
						}
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetNTLMOff(5) })
				})
			must(t, inner)
			must(t, outer)
			if seen.NTLMOff.Off != 5 || seen.NTLMOff.Deleted != (entity.OperationID{}) {
				t.Errorf("NTLM off read by the attempt that committed: got %+v, want off at 5 with no deletion", seen.NTLMOff)
			}
			j := entity.MachineJoin{Joined: true, Account: "HOST$", Owner: []byte{1, 5}, Delegation: []byte{7},
				Names: []string{"host/a.example", "cifs/a.example"}}
			j.LastRotation.At, j.LastRotation.Failed = time.Unix(1_800_000_100, 0).UTC(), true
			j.LastRotation.Tag, j.LastRotation.Read = 4, 5
			j.LastRotation.Version, j.LastRotation.Confirmed = 3, time.Unix(1_800_000_000, 0).UTC()
			pw := entity.SecretRef{15: 0x3C}
			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachinePassword(pw) }))
			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }))
			in, err := s.Installation(ctx)
			must(t, err)
			if !reflect.DeepEqual(in.Join, j) {
				t.Errorf("machine join read back: got %+v, want %+v", in.Join, j)
			}
			if in.MachinePassword != pw {
				t.Errorf("machine password read back: got %x, want %x", in.MachinePassword, pw)
			}
		})
	}
}

// Now is one value for a transaction's life, at or after the store time of
// a transaction committed before it, and a retried attempt reads a later
// one.
func testWriteNow(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k"}, 0)))
			var before time.Time
			must(t, cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { before = tx.Now(); return nil }))
			var nows []time.Time
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						first := tx.Now()
						if _, _, err := tx.Setting(instScope, "k"); err != nil {
							return err
						}
						if again := tx.Now(); !again.Equal(first) {
							t.Errorf("Now read twice in one attempt: got %v then %v, want one value", first, again)
						}
						nows = append(nows, first)
						return errOf(tx.PutNetgroup(entity.Netgroup{Name: "lab"}, 0))
					})
				},
				func() error {
					return errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k"}, settingGen(t, cp)))
				})
			must(t, inner)
			must(t, outer)
			if len(nows) != 2 {
				t.Fatalf("attempts: got %d, want 2", len(nows))
			}
			if nows[0].Before(before) {
				t.Errorf("Now of a later write: got %v, want at or after %v", nows[0], before)
			}
			if !nows[1].After(nows[0]) {
				t.Errorf("Now of the retried attempt: got %v, want after the first attempt's %v", nows[1], nows[0])
			}
		})
	}
}

// keyed is a write under an idempotency key whose function writes the
// netgroup name and records answer.
func keyed(cp kvstore.ControlPlane, account entity.Principal, key, body, answer, name string, ran *int) error {
	i := &entity.IdempotencyRecord{Account: account, Key: key, Digest: sha256.Sum256([]byte(body))}
	return cp.Write(ctx, entity.WriteMeta{Idempotency: i}, func(tx entity.ControlPlaneTx) error {
		*ran++
		if _, err := tx.PutNetgroup(entity.Netgroup{Name: name}, 0); err != nil {
			return err
		}
		i.Answer = []byte(answer)
		return nil
	})
}

func replayed(err error) ([]byte, bool) {
	var r *entity.IdempotentReplay
	if errors.As(err, &r) {
		return r.Answer, true
	}
	return nil, false
}

// A request repeated under its key gets the answer its function set and
// changes nothing; under the key with another digest it is refused; a
// refused request leaves its key free; each account's keys are its own.
func testIdempotency(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	alice, bob := entity.NewUser(), entity.NewUser()
	ran := 0
	must(t, keyed(cp, alice, "k1", "create lab", "201 lab", "lab", &ran))
	answer, ok := replayed(keyed(cp, alice, "k1", "create lab", "201 other", "lab2", &ran))
	if !ok || string(answer) != "201 lab" {
		t.Errorf("request repeated under its key: answer %q, replay %v; want the first answer %q", answer, ok, "201 lab")
	}
	if ran != 1 {
		t.Errorf("function runs after a replay: got %d, want 1", ran)
	}
	if err := keyed(cp, alice, "k1", "create lab2", "201", "lab2", &ran); !errors.Is(err, entity.ErrIdempotencyMismatch) {
		t.Errorf("key reused with another digest: %v, want %v", err, entity.ErrIdempotencyMismatch)
	}
	if err := keyed(cp, bob, "k1", "create lab3", "201 lab3", "lab3", &ran); err != nil {
		t.Errorf("another account's request under the same key: %v, want it applied", err)
	}
	if err := keyed(cp, alice, "k2", "create lab again", "201", "lab", &ran); !errors.Is(err, entity.ErrGenerationMismatch) {
		t.Fatalf("request refused by its function: %v, want %v", err, entity.ErrGenerationMismatch)
	}
	if err := keyed(cp, alice, "k2", "create lab4", "201 lab4", "lab4", &ran); err != nil {
		t.Errorf("key of a refused request used again: %v, want it free", err)
	}
	if got := len(all(t, cp.Netgroups(ctx, ""))); got != 3 {
		t.Errorf("netgroups written: got %d, want 3 (lab, lab3, lab4)", got)
	}
}

// Two requests under one key that both miss its record conflict on it: one
// commits, and the other then replays its answer. They write netgroups of
// different names, so the key's record is all they share.
func testIdempotencyRace(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			alice := entity.NewUser()
			var outerRan, innerRan int
			outer, inner := race(h,
				func() error { return keyed(cp, alice, "k", "create lab", "outer", "lab2", &outerRan) },
				func() error { return keyed(cp, alice, "k", "create lab", "inner", "lab", &innerRan) })
			must(t, inner)
			if answer, ok := replayed(outer); !ok || string(answer) != "inner" {
				t.Errorf("the request committing second: %v, answer %q; want a replay of %q", outer, answer, "inner")
			}
			if got := len(all(t, cp.Netgroups(ctx, ""))); got != 1 {
				t.Errorf("netgroups written: got %d, want 1", got)
			}
		})
	}
}
