package node

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// tableStoreForCheck is a store whose writes a test runs through the node's
// table's Check, as the API's settings route does.
func tableStoreForCheck(t *testing.T) (*kvstore.Store, entity.NodeID) {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}

	return st, probeNode
}

// The node's table judges identity's keys: an admin's put of a key of the
// identity section and of the directory record passes its Check, where a
// table without identity's sections refuses each as an unknown key.
func TestTableChecksIdentity(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st, _ := tableStoreForCheck(t)
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	for key, value := range map[string]string{
		"identity.netbios_name":      `"NAS-01"`,
		"identity.directory.address": `"ldaps://dc1.corp.example"`,
	} {
		w := settings.Put(entity.Setting{Scope: installation, Key: key, Value: []byte(value)})

		err := st.ControlPlane().
			Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return newTable(probeNode).Check(tx, entity.RoleAdmin, w, nil)
			})
		if err != nil {
			t.Errorf("Check of an admin's put of %s = %s: %v, want accepted", key, value, err)
		}
	}
}

// The port rule runs on the node's settings table's Check, as the settings
// route calls it: a write that leaves the two adapters' effective ports
// equal on the written node is refused, naming both keys; a write that keeps
// them apart passes, and a value equal at the installation's, written while
// the node's own override of the other port holds the two apart there,
// reaches no node that overrides it (RFC 13 section 6).
func TestPortRuleRefusesEqualPortsThroughCheck(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st, node := tableStoreForCheck(t)
	own := entity.SettingScope{Kind: entity.ScopeNode, ID: node}
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	write := func(w settings.Write) error {
		return st.ControlPlane().
			Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return newTable(node).Check(tx, entity.RoleOperator, w, nil)
			})
	}
	store := func(row entity.Setting) {
		t.Helper()

		err := st.ControlPlane().
			Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				cur, _, err := tx.Setting(row.Scope, row.Key)
				if err != nil {
					return err
				}

				_, err = tx.PutSetting(row, cur.Generation)

				return err
			})
		if err != nil {
			t.Fatalf("store the %s row: %v", row.Key, err)
		}
	}

	// The node's two ports start at the protocols' defaults, apart; a write
	// either way of one onto the other carries the two keys' equal values.
	for key, onto := range map[string]string{
		"adapters.nfs.port": "12445",
		"adapters.smb.port": "12049",
	} {
		w := settings.Put(entity.Setting{Scope: own, Key: key, Value: []byte(onto)})
		if err := write(w); err == nil || !strings.Contains(err.Error(), "adapters.nfs.port") ||
			!strings.Contains(err.Error(), "adapters.smb.port") {
			t.Fatalf(
				"Check of %s on the other's port: %v, want the write refused naming both keys",
				key,
				err,
			)
		}
	}

	apart := settings.Put(
		entity.Setting{Scope: own, Key: "adapters.nfs.port", Value: []byte("2049")},
	)
	if err := write(apart); err != nil {
		t.Fatalf("Check of the ports kept apart: %v, want the write accepted", err)
	}

	store(entity.Setting{Scope: own, Key: "adapters.nfs.port", Value: []byte("2049")})

	// The SMB port written equal to NFS's default while the node's own
	// override of its NFS port holds the two apart there: an equal
	// installation value reaches no node that overrides it.
	equalInstalled := settings.Put(entity.Setting{
		Scope: installation, Key: "adapters.smb.port", Value: []byte("12049"),
	})
	if err := write(equalInstalled); err != nil {
		t.Errorf(
			"Check of an SMB port the node's override shadows: %v, want the write accepted",
			err,
		)
	}
}
