package identity_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// kvStore opens an initialised in-memory metadata store and returns it with
// the KV beneath it, closed at the test's end.
func kvStore(t *testing.T) (*kvstore.Store, kv.KV) {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return st, db
}

// rawRow is the KV key and value of the installation's row key = value, as
// the store writes it, taken from a store of its own.
func rawRow(t *testing.T, key, value string) (k, v []byte) {
	t.Helper()
	ctx := context.Background()
	st, db := kvStore(t)
	if _, err := st.ControlPlane().PutSetting(ctx, entity.Setting{Scope: installationScope, Key: key, Value: []byte(value)}, 0); err != nil {
		t.Fatal(err)
	}
	err := db.View(ctx, func(r kv.Reader) error {
		for row, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			if bytes.Contains(row.Key, []byte(key)) {
				k, v = row.Key, row.Value
			}
		}
		return nil
	})
	if err != nil || k == nil {
		t.Fatalf("raw row of %s = %s: found %t, err %v", key, value, k != nil, err)
	}
	return k, v
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

// A write of the NetBIOS name whose read of the domain is overtaken between
// its check and its commit, by a raw write of the domain row that raises no
// settings generation, retries on that read alone and is refused naming the
// domain: the second attempt's check reads the new domain.
func TestCheckNetBIOSDomainOvertaken(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	st, db := kvStore(t)
	cp := st.ControlPlane()
	mustPut(t, cp, dcAddress, dcAddrJSON)
	mustPut(t, cp, domainKey, `"OLD"`)
	k, v := rawRow(t, domainKey, `"corp"`)

	attempts, err := putSettingWith(cp, entity.RoleAdmin, nameKey, `"CORP"`, func(attempt int) error {
		if attempt > 1 {
			return nil
		}
		return db.Update(ctx, func(tx kv.Txn) error { return tx.Set(k, v) })
	})
	refusedNaming(t, "name write, domain overtaken", err, nameKey, domainKey)
	if attempts != 2 {
		t.Errorf("attempts of the name write = %d, want 2: its check's read of %s must be tracked", attempts, domainKey)
	}
	var found bool
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
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
