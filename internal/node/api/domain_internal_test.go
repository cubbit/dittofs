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
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"
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

// A computer account's names written after the keytab's pre-write check
// passed are caught by its put, which runs the check again on the
// installation read tracked in the write's transaction.
func TestKeytabNamesCheckedInTheWrite(t *testing.T) {
	cp := ntlmStore(t)
	kt := keytab.New()
	if err := kt.AddEntry("host/dfs.ad.example", "AD.EXAMPLE", "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	inTx, err := keytabs.check(t.Context(), Deps{Store: cp}, b)
	if err != nil {
		t.Fatalf("the check before any join: %v, want admitted", err)
	}
	j := entity.MachineJoin{Joined: true, Account: "DFS$", Names: []string{"host/dfs.ad.example"}}
	if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetMachineJoin(j) }); err != nil {
		t.Fatal(err)
	}
	err = cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, _, err := keytabs.put(t.Context(), request[apiv1.Credential]{d: Deps{Store: cp}}, sealedSecret{inTx: inTx}, tx)
		return err
	})
	if p, _ := problemOf(err); p.Status != 400 || p.Field != "spec.value" {
		t.Errorf("the keytab's put, the join since naming the key: %v, want 400 naming spec.value, ahead of the settings table", err)
	}
}

// Every refusal of a join before its hold or its seal clears the password
// the request carried.
func TestJoinRefusalClearsThePassword(t *testing.T) {
	cp := ntlmStore(t)
	for _, tc := range []struct {
		name string
		spec apiv1.JoinDomainSpec
		user string
	}{
		{"online, an OU outside the base", apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline, OU: "OU=x,DC=other"}, "Administrator"},
		{"online, no identity", apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline}, "Administrator"},
		{"offline, no identity", apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: "DFS$"}, ""},
	} {
		pw := []byte("s3cret")
		rq := request[apiv1.JoinDomain]{Body: apiv1.JoinDomain{Spec: tc.spec, Credential: apiv1.JoinCredential{User: tc.user, Password: pw}}, d: Deps{Store: cp}}
		if _, err := prepareJoin(t.Context(), rq); err == nil {
			t.Fatalf("%s: prepared, want refused", tc.name)
		}
		if string(pw) != "\x00\x00\x00\x00\x00\x00" {
			t.Errorf("%s: password after the refusal %q, want zeroed", tc.name, pw)
		}
	}
}
