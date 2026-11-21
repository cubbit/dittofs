package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// faultStore opens an initialised in-memory metadata store behind a FaultKV,
// closed at the test's end.
func faultStore(t *testing.T) (*kvstore.Store, *kvtest.FaultKV) {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := kvtest.NewFaultKV(db)
	if err := kvstore.Init(ctx, f, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return st, f
}

// mustPut is an admin's putSetting that must commit.
func mustPut(t *testing.T, cp kvstore.ControlPlane, key, value string) {
	t.Helper()
	if err := putSetting(cp, entity.RoleAdmin, key, value); err != nil {
		t.Fatalf("put of %s = %s: %v", key, value, err)
	}
}

// refusedNaming asserts err is the RFC 13 §6 refusal of key, ErrInvalid,
// naming other.
func refusedNaming(t *testing.T, name string, err error, key, other string) {
	t.Helper()
	if !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("%s: put of %s = %v, want ErrInvalid", name, key, err)
		return
	}
	if line := "setting (installation): " + key + ": "; !strings.HasPrefix(err.Error(), line) || !strings.Contains(err.Error(), other) {
		t.Errorf("%s: refusal %q, want it to start %q and name %s", name, err, line, other)
	}
}

const (
	dcAddress  = "identity.directory.address"
	nameKey    = "identity.netbios_name"
	domainKey  = "identity.directory.netbios_domain"
	dcAddrJSON = `"ldaps://dc1.corp.example"`
)

// A write of the installation's NetBIOS name, or of the directory record's
// NetBIOS domain, equal to the other's stored value with ASCII letters folded
// is refused, naming the other; a Unicode letter folding to an ASCII one is
// no match, and the default name counts where none is stored.
func TestCheckNetBIOSNameAgainstDomain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before map[string]string // stored first, in key order
		key    string
		value  string
		other  string // the key the refusal names; empty for accepted
	}{
		{"name as the domain in another case", map[string]string{domainKey: `"corp"`}, nameKey, `"CORP"`, domainKey},
		{"domain as the stored name", map[string]string{nameKey: `"NAS"`}, domainKey, `"Nas"`, nameKey},
		{"domain as the default name", nil, domainKey, `"dittofs"`, nameKey},
		{"Kelvin sign", map[string]string{domainKey: "\"\u212aDC\""}, nameKey, `"KDC"`, ""},
		{"distinct", map[string]string{domainKey: `"CORP"`}, nameKey, `"NAS"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			cp := newStore(t).ControlPlane()
			mustPut(t, cp, dcAddress, dcAddrJSON)
			for _, k := range []string{nameKey, domainKey} {
				if v, ok := tc.before[k]; ok {
					mustPut(t, cp, k, v)
				}
			}
			err := putSetting(cp, entity.RoleAdmin, tc.key, tc.value)
			if tc.other == "" {
				if err != nil {
					t.Errorf("%s: put of %s = %s: %v, want committed", tc.name, tc.key, tc.value, err)
				}
				return
			}
			refusedNaming(t, tc.name, err, tc.key, tc.other)
		})
	}
}

// Two writes, each of one side to a value the other side's old value
// allows, the first held before its commit: the second commits, and the
// first, its read of the other side overtaken, retries and is refused.
func TestCheckNetBIOSConcurrentSides(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st, f := faultStore(t)
	cp := st.ControlPlane()
	mustPut(t, cp, dcAddress, dcAddrJSON)
	mustPut(t, cp, domainKey, `"OLD"`)

	clock := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, clock, sim.Rule{
		Op: kvtest.OpSet, Key: "*", Count: 1, Action: sim.FaultAction{Delay: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.SetFault(sched)

	first := make(chan error, 1)
	go func() { first <- putSetting(cp, entity.RoleAdmin, nameKey, `"CORP"`) }()

	// Polls the condition itself, the first write parked on the clock.
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first write never reached its held set")
		}
		time.Sleep(time.Millisecond)
	}

	if err := putSetting(cp, entity.RoleAdmin, domainKey, `"corp"`); err != nil {
		t.Fatalf("second write, the domain corp while the name is DITTOFS: %v, want committed", err)
	}
	clock.Advance(time.Second)

	refusedNaming(t, "first write, held", <-first, nameKey, domainKey)
	var found bool
	err = cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
		_, found, err = tx.Setting(installationScope, nameKey)
		return err
	})
	if err != nil || found {
		t.Errorf("%s after the refused write: found %t, err %v; want no row", nameKey, found, err)
	}
}

// The directory record is written by admin only: an operator's first put
// of it is refused ErrForbidden, as is a later change of one field; an
// admin's passes.
func TestCheckDirectoryWriteRole(t *testing.T) {
	prof.Watch(t, prof.Options{})
	cp := newStore(t).ControlPlane()
	if err := putSetting(cp, entity.RoleOperator, dcAddress, dcAddrJSON); !errors.Is(err, settings.ErrForbidden) {
		t.Errorf("operator's first put of %s = %v, want ErrForbidden", dcAddress, err)
	}
	mustPut(t, cp, dcAddress, dcAddrJSON)
	base := "identity.directory.base"
	if err := putSetting(cp, entity.RoleOperator, base, `"DC=corp,DC=example"`); !errors.Is(err, settings.ErrForbidden) {
		t.Errorf("operator's put of %s = %v, want ErrForbidden", base, err)
	}
	if err := putSetting(cp, entity.RoleAdmin, base, `"DC=corp,DC=example"`); err != nil {
		t.Errorf("admin's put of %s = %v, want committed", base, err)
	}
}
