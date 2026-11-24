package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// TestBindPasswordOpensTheSettingKeyOwner seals the directory record's bind
// credential under the owner the write check accepts, writes the setting's
// row through the check as the route does, and reads the credential back
// through the server: RFC 16 §2.3, a setting's secret is owned by its scope
// and key, and the server opens it by the same.
func TestBindPasswordOpensTheSettingKeyOwner(t *testing.T) {
	prof.Watch(t, prof.Options{})

	st, _ := dirStore(t)
	p := protocolSecrets(t, st)

	// The key of the row being checked, spelled as the wires spell it and
	// held apart from the reader's constant, so the check derives the owner
	// from the row and the reader has to match whatever the section
	// declares. A rename of the field's json tag, or a drift of the
	// reader's key, fails below.
	bindKey := "identity.directory.bind_credential"

	owner := entity.SecretOwner{
		Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key:   bindKey,
	}
	ref := entity.SecretRef(uuid.New())
	pw := []byte("the bind password")

	sec, err := p.Seal(context.Background(), secrets.Protocol, entity.SecretDirectoryCredential,
		owner, ref, pw, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}

	meta := entity.WriteMeta{Audit: &entity.AuditRecord{
		Target: entity.ResourceRef{Kind: entity.ResourceSecret, ID: sec.ID[:]},
		Change: []byte("set"),
	}}
	if err := st.ControlPlane().Write(context.Background(), meta,
		func(tx entity.ControlPlaneTx) error {
			return tx.PutSecret(sec, sec.Version-1)
		}); err != nil {
		t.Fatal(err)
	}

	if err := putSetting(st.ControlPlane(), entity.RoleAdmin,
		"identity.directory.address", `"ldaps://dc.corp.example"`); err != nil {
		t.Fatalf("write the address row through the check: %v", err)
	}

	row, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}

	if err := putSetting(st.ControlPlane(), entity.RoleAdmin, bindKey, string(row)); err != nil {
		t.Fatalf("write the %q row through the check: %v", bindKey, err)
	}

	s, err := identity.New(
		identity.Deps{Store: st, Clock: sim.NewTestClock(time.Unix(0, 0)), Secrets: p},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := s.BindPassword(context.Background(), ref)
	if err != nil {
		t.Fatalf("BindPassword through the accepted owner: %v", err)
	}

	if !bytes.Equal(got, pw) {
		t.Errorf("BindPassword = %q, want %q", got, pw)
	}
}
