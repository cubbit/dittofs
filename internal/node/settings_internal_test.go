package node

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The node's table judges identity's keys: an admin's put of a key of the
// identity section and of the directory record passes its Check, where a
// table without identity's sections refuses each as an unknown key.
func TestTableChecksIdentity(t *testing.T) {
	prof.Watch(t, prof.Options{})
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
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}
	for key, value := range map[string]string{
		"identity.netbios_name":      `"NAS-01"`,
		"identity.directory.address": `"ldaps://dc1.corp.example"`,
	} {
		w := settings.Put(entity.Setting{Scope: installation, Key: key, Value: []byte(value)})
		err := st.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return newTable().Check(tx, entity.RoleAdmin, w, nil)
		})
		if err != nil {
			t.Errorf("Check of an admin's put of %s = %s: %v, want accepted", key, value, err)
		}
	}
}
