package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/google/uuid"
)

type protocolSealer struct{ p *secrets.Provider }

func (s protocolSealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, handedOver bool) (entity.Secret, error) {
	return s.p.Seal(ctx, secrets.Protocol, kind, owner, ref, value, secrets.Meta{HandedOver: handedOver})
}

// Two offline joins race: each writes its machine password version, the
// second after the first, before either starts its operation. The first's
// start, which reads the version tracked in its own transaction, is refused
// 409, since the version it would join with is no longer current; the
// second's starts.
func TestOfflineJoinStartsOnlyOnItsVersion(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{NodeID: uuid.New()})
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
	cp := st.ControlPlane()
	const passphrase = "race test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	b, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "protocol.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	prov, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Store: cp, Sealer: protocolSealer{prov}, Joins: ids}
	join := func(pw string) opStart {
		rq := request[apiv1.JoinDomain]{Body: apiv1.JoinDomain{Spec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, Credential: apiv1.JoinCredential{Password: []byte(pw)}}, d: d}
		rq.own = func(ctx context.Context, _ entity.ResourceRef, _ []byte, fn func(entity.ControlPlaneTx) error) error {
			return cp.Write(ctx, entity.WriteMeta{}, fn)
		}
		s, err := prepareJoin(t.Context(), rq)
		if err != nil {
			t.Fatalf("prepare the join: %v", err)
		}
		return s
	}
	check := func(s opStart) error {
		return cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			in, err := tx.Installation()
			if err != nil {
				return err
			}
			return s.check(tx, in)
		})
	}
	first, second := join("pw-1"), join("pw-2")
	if p, _ := problemOf(check(first)); p.Code != apiv1.CodeOperationRunning {
		t.Errorf("the first join's start, its version superseded: %+v, want 409 operation_running", p)
	}
	if err := check(second); err != nil {
		t.Errorf("the second join's start on the current version: %v, want admitted", err)
	}
}
