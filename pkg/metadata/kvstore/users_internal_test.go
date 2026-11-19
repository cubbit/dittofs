package kvstore

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// A user whose stored name the name rule now refuses, written before the
// rule, is still edited on its other fields, its name kept as stored.
func TestStoredNameTheRuleRefusesStaysWritable(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	cp := f.s.ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "bob"}
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		old, _, err := userIn(tx, u.Principal)
		if err != nil {
			return err
		}
		old.Name = "bob "
		return tx.Set(userKey(u.Principal.ID), encodeUser(old))
	}); err != nil {
		t.Fatal(err)
	}
	u.Name, u.Disabled = "bob ", true
	if _, err := cp.PutUser(ctx, u, gen, entity.Alloc{}); err != nil {
		t.Fatalf("disabling a user stored as %q: %v, want accepted", u.Name, err)
	}
	got, ok, err := cp.User(ctx, u.Principal)
	if err != nil || !ok || got.Name != "bob " || !got.Disabled {
		t.Errorf("user after the edit: %+v, %v, %v; want %q, disabled", got, ok, err, "bob ")
	}
}
