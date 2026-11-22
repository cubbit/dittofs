package api

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

// netbiosChange is a write of the installation's identity.netbios_name in
// one transaction of cp, the rule first, as the settings route runs it,
// counting in attempts each run of the transaction.
func netbiosChange(t *testing.T, cp kvstore.ControlPlane, key, value string, attempts *int) error {
	scope := entity.SettingScope{Kind: entity.ScopeInstallation}
	return cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		*attempts++
		if err := netbiosRule(tx, scope, key); err != nil {
			return err
		}
		old, _, err := tx.Setting(scope, key)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(entity.Setting{Scope: scope, Key: key, Value: []byte(value)}, old.Generation)
		return err
	})
}

// settingValue reads the installation's key back from cp.
func settingValue(t *testing.T, cp kvstore.ControlPlane, key string) string {
	t.Helper()
	var s entity.Setting
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
		s, _, err = tx.Setting(entity.SettingScope{Kind: entity.ScopeInstallation}, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return string(s.Value)
}

// joinAs records the installation joined as account, as a join's last step
// does.
func joinAs(t *testing.T, cp kvstore.ControlPlane, account string) {
	t.Helper()
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tx.SetMachineJoin(entity.MachineJoin{Joined: true, Account: account})
	}); err != nil {
		t.Fatalf("join as %s: %v", account, err)
	}
}

// refusedJoined fails the test unless err is the rule's refusal, 409
// joined, naming the account.
func refusedJoined(t *testing.T, what string, err error, account string) {
	t.Helper()
	p, _ := problemOf(err)
	if p.Status != 409 || p.Code != apiv1.CodeJoined {
		t.Fatalf("%s: %v, answered %d %q, want 409 joined", what, err, p.Status, p.Code)
	}
	if r, _ := err.(*refusal); r == nil || !strings.Contains(r.detail, account) {
		t.Errorf("%s: refusal %v, want it naming the join as %s", what, err, account)
	}
}

// The identity.netbios_name rule, as a function of a transaction over the
// store: joined, a change is refused naming the join and the name reads back
// unchanged; not joined, it is accepted; another key, or the name at another
// scope, is not the rule's.
func TestNetBIOSNameRule(t *testing.T) {
	t.Run("joined", func(t *testing.T) {
		cp := ntlmStore(t)
		var n int
		if err := netbiosChange(t, cp, netbiosNameKey, `"NAS"`, &n); err != nil {
			t.Fatalf("set the name before the join: %v", err)
		}
		joinAs(t, cp, "NAS$")
		refusedJoined(t, "a change while joined", netbiosChange(t, cp, netbiosNameKey, `"OTHER"`, &n), "NAS$")
		if got := settingValue(t, cp, netbiosNameKey); got != `"NAS"` {
			t.Errorf("name after the refused change: got %s, want \"NAS\"", got)
		}
		if err := netbiosChange(t, cp, "identity.nfs4_domain", `"example.com"`, &n); err != nil {
			t.Errorf("another key while joined: %v, want accepted", err)
		}
		err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return netbiosRule(tx, entity.SettingScope{Kind: entity.ScopeNode, ID: uuid.New()}, netbiosNameKey)
		})
		if err != nil {
			t.Errorf("the name at a node's scope while joined: %v, want not the rule's", err)
		}
	})
	t.Run("not joined", func(t *testing.T) {
		cp := ntlmStore(t)
		var n int
		if err := netbiosChange(t, cp, netbiosNameKey, `"OTHER"`, &n); err != nil {
			t.Fatalf("a change while not joined: %v, want accepted", err)
		}
		if got := settingValue(t, cp, netbiosNameKey); got != `"OTHER"` {
			t.Errorf("name after the accepted change: got %s, want \"OTHER\"", got)
		}
	})
}

// A join committing between the change's read of the installation and its
// commit aborts the change on that tracked read: the change's first Set is
// held on the test clock while the join commits, then released. Its
// transaction runs again, now reads joined, and is refused; the name is
// unchanged. The join here writes the installation record alone, not the
// name as the real one also does, so the change's own tracked read of the
// name's record cannot be what aborts it.
func TestNetBIOSNameRuleRacingAJoin(t *testing.T) {
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	db, err := badgerkv.Open(badgerkv.Options{NodeID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := kvtest.NewFaultKV(db)
	if err := kvstore.Init(t.Context(), f, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	cp := st.ControlPlane()
	var n int
	if err := netbiosChange(t, cp, netbiosNameKey, `"NAS"`, &n); err != nil {
		t.Fatal(err)
	}

	sched, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpSet, Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)
	attempts := 0
	done := make(chan error, 1)
	go func() { done <- netbiosChange(t, cp, netbiosNameKey, `"OTHER"`, &attempts) }()
	for deadline := time.Now().Add(10 * time.Second); clock.Pending() < 1; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the change's write never reached the delay")
		}
	}
	joinAs(t, cp, "NAS$")
	clock.Advance(time.Second)
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the held change never returned")
	}
	refusedJoined(t, "a change racing a join", err, "NAS$")
	if attempts != 2 {
		t.Errorf("the change's transaction ran %d times, want 2: aborted once by the join", attempts)
	}
	if got := settingValue(t, cp, netbiosNameKey); got != `"NAS"` {
		t.Errorf("name after the change raced the join: got %s, want \"NAS\"", got)
	}
}
