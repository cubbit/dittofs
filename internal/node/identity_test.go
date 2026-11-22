package node_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node"
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

// startWith starts h's node, failing the test if it does not start, and
// stops it at cleanup.
func startWith(t *testing.T, h *nodetest.Harness) *node.Node {
	t.Helper()
	n := h.Node()
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := n.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return n
}

// putSetting writes an installation row of key holding value, unchecked,
// as a row the store accepted from another binary.
func putSetting(t *testing.T, h *nodetest.Harness, key, value string) {
	t.Helper()
	s := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: key, Value: []byte(value)}
	if _, err := h.Store().ControlPlane().PutSetting(t.Context(), s, 0); err != nil {
		t.Fatalf("put %s = %s: %v", key, value, err)
	}
}

// An identity row the section refuses does not stop the node: identity runs
// the section's defaults, every key of them, and the refusal is logged.
func TestARefusedIdentityRowStartsOnTheDefaults(t *testing.T) {
	logs := make(logLines, 256)
	h := nodetest.New(t, nodetest.InMemory(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
		return node.New(cfg)
	}))
	putSetting(t, h, "identity.netbios_name", `"OTHER"`)
	putSetting(t, h, "identity.ntlm", `123`)
	n := startWith(t, h)
	if got, err := n.ComputerName(); err != nil || got != identitysrv.DefaultSettings().NetBIOSName {
		t.Errorf("NetBIOS name with identity.ntlm refused: got %q, %v, want the default %q", got, err, identitysrv.DefaultSettings().NetBIOSName)
	}
	for {
		select {
		case l := <-logs:
			if strings.Contains(l, "settings section refused") && strings.Contains(l, "identity.ntlm") {
				return
			}
		default:
			t.Fatal("no log line names the refused identity.ntlm row")
		}
	}
}

// A settings write that lands during the walk identity's first State is read
// from discards the walk, and the next one is read: here it fails, as only a
// second walk could.
func TestAWalkTheSettingsMovedUnderIsReadAgain(t *testing.T) {
	h, f := faulted(t)
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held,
		sim.Rule{Op: kvtest.OpScan, Key: settingsScan, Count: 1, Action: sim.FaultAction{Delay: time.Hour}},
		sim.Rule{Op: kvtest.OpScan, Key: settingsScan, From: 2, Count: 1, Action: sim.FaultAction{Err: errInjected}})
	started := make(chan error, 1)
	go func() { started <- h.Node().Start(t.Context()) }()
	wait(t, "the settings walk to be held", func() bool { return held.Pending() == 1 })
	putSetting(t, h, "identity.netbios_name", `"OTHER"`)
	held.Advance(time.Hour)
	err := answered(t, started)
	t.Cleanup(func() { _ = h.Node().Stop(context.Background()) })
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "identity:") {
		t.Fatalf("Start with a setting written during the walk: got %v, want the second walk's injected error", err)
	}
}

// settingsScan is the settings walk's scan, as a FaultKV rule names it.
var settingsScan = keys.Describe([]byte{keys.KindCFG})
