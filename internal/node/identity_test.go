package node_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	vfs "github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/google/uuid"
)

// A user whose password was derived with NewHashes and sealed as the API
// stores it signs in through the identity the node hands the API, once the
// node has started: the API is handed the identity the node built, over the
// node's secret provider. The filesystem is handed the same identity: it
// finds and spells alice, and passes a sign-in through to it.
func TestIdentityServesTheAPIAndTheFilesystem(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory())
	ctx := t.Context()
	const password = "correct horse battery staple"
	u := entity.User{Principal: entity.NewUser(), Name: "alice", Password: entity.SecretRef(uuid.New())}
	hashes, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{})
	if err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	kp, err := keyprovider.NewProvider(ctx, keyprovider.Config{Kind: keyprovider.KindLocal, File: h.StorageKeyFile()})
	if err != nil {
		t.Fatalf("open the storage key: %v", err)
	}
	defer func() { _ = kp.Close() }()
	cp := h.Store().ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: kp})
	if err != nil {
		t.Fatalf("secret provider: %v", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "password"}
	sec, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password, hashes.Password, secrets.Meta{})
	if err != nil {
		t.Fatalf("seal the password hash: %v", err)
	}
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if _, err := tx.PutUser(u, 0, entity.Alloc{UID: true, RID: true, Range: entity.DefaultIDRange}); err != nil {
			return err
		}
		return tx.PutSecret(sec, 0)
	})
	if err != nil {
		t.Fatalf("write alice: %v", err)
	}

	n := h.Node()
	if err := n.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := n.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	got, err := n.Accounts().Lookup(ctx, "alice")
	if err != nil || got != u.Principal {
		t.Fatalf("Accounts().Lookup(alice): got %v, %v, want %v, nil", got, err, u.Principal)
	}
	if err := n.Accounts().VerifyPassword(ctx, u.Principal, []byte(password)); err != nil {
		t.Fatalf("Accounts().VerifyPassword with alice's password: got %v, want nil", err)
	}
	fs := n.Service()
	if got, err := fs.Find(ctx, vfs.Call{}, vfs.Spelling{Scheme: vfs.SchemeUserName, Text: "alice"}); err != nil || got != u.Principal {
		t.Errorf("Service().Find(alice): got %v, %v, want %v, nil", got, err, u.Principal)
	}
	if got, err := fs.Spell(ctx, u.Principal, vfs.SchemeUserName); err != nil || got.Text != "alice" {
		t.Errorf("Service().Spell(alice's principal): got %+v, %v, want alice, nil", got, err)
	}
	// identity refuses a step carrying no mechanism ErrInvalid; a filesystem
	// with no Auth would answer ErrUnavailable.
	if _, err := fs.SignIn(ctx, vfs.SignInStep{}); !errors.Is(err, vfserr.ErrInvalid) {
		t.Errorf("Service().SignIn with an empty step: got %v, want identity's ErrInvalid", err)
	}
}

// putSetting writes an installation row of key holding value, unchecked,
// as a row the store accepted from another binary, over the row stored.
func putSetting(t *testing.T, h *nodetest.Harness, key, value string) {
	t.Helper()
	s := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: key, Value: []byte(value)}
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		old, _, err := tx.Setting(s.Scope, key)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(s, old.Generation)
		return err
	})
	if err != nil {
		t.Fatalf("put %s = %s: %v", key, value, err)
	}
}

// An identity row the section refuses fails the start, naming the row: no
// default stands in for it.
func TestARefusedIdentityRowFailsTheStart(t *testing.T) {
	h := nodetest.New(t, nodetest.InMemory())
	putSetting(t, h, "identity.ntlm", `123`)
	err := h.Node().Start(t.Context())
	t.Cleanup(func() { _ = h.Node().Stop(context.Background()) })
	if err == nil || !strings.Contains(err.Error(), "identity.ntlm") {
		t.Fatalf("Start with identity.ntlm = 123: got %v, want an error naming identity.ntlm", err)
	}
}

// A settings write that lands during the walk identity's first State is read
// from discards the walk, and identity is applied from the next one, which
// holds the write. The held scan reads the page in a transaction begun before
// the write, so the first walk misses it.
func TestAWalkTheSettingsMovedUnderIsReadAgain(t *testing.T) {
	h, f := faulted(t)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpScan, Key: settingsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	started := make(chan error, 1)
	go func() { started <- h.Node().Start(t.Context()) }()
	wait(t, "the settings walk to be held", func() bool { return held.Pending() == 1 })
	putSetting(t, h, "identity.netbios_name", `"OTHER"`)
	held.Advance(time.Hour)
	if err := answered(t, started); err != nil {
		t.Fatalf("Start with a setting written during the walk: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	if got, err := h.Node().ComputerName(); err != nil || got != "OTHER" {
		t.Errorf("NetBIOS name written during the first walk: got %q, %v, want OTHER", got, err)
	}
}

// settingsScan is the settings walk's scan, as a FaultKV rule names it.
var settingsScan = keys.Describe([]byte{keys.KindCFG})
