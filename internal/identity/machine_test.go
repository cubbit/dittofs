package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// switchFaults fails every call of an op while the test has it set, or of
// an op on one key when set as op:key: a fault injector for the domain
// controller's wrapper.
type switchFaults struct {
	mu sync.Mutex
	m  map[string]error
}

func (f *switchFaults) set(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[op] = err
}

func (f *switchFaults) At(op string, key []byte) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.m[op+":"+string(key)]; err != nil {
		return 0, 0, err
	}
	return 0, 0, f.m[op]
}

// rotFixture is a server over the in-memory store and the real secret
// provider, joined as NAS$ to a domain whose controller dc1 holds pw2 at key
// version number 1, reached through a fault wrapper, on a clock at mt0.
type rotFixture struct {
	clock  *sim.TestClock
	cp     kvstore.ControlPlane
	store  *kvstore.Store
	p      *secrets.Provider
	d      *identitytest.DomainController
	faults *switchFaults
	fkv    *kvtest.FaultKV
	reg    *metrics.Registry
	rec    directory.Record
	in     entity.Installation
	s      *identity.Server
	// hold stops the turns delivering the store's State to the server, as a
	// node lagging behind the store.
	hold bool
	// checked receives the end of each of the rotation schedule's checks.
	checked chan struct{}
}

const bindPW = "Svc-Passw0rd"

// rotKeys writes one real key file per role and opens both.
func rotKeys(t *testing.T) map[secrets.Role]keyprovider.KeyProvider {
	t.Helper()
	const passphrase = "rotation test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	keys := map[secrets.Role]keyprovider.KeyProvider{}
	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kp.Close() })
		keys[role] = kp
	}
	return keys
}

// newMachine is a server over the in-memory store, behind kvtest's fault
// wrapper, and the real secret provider, not joined, with a domain whose
// controllers are dc1 and dc2, on a clock at mt0.
func newMachine(t *testing.T) *rotFixture {
	t.Helper()
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	clock := sim.NewTestClock(mt0)
	db, err := badgerkv.Open(badgerkv.Options{Clock: clock.Now, CeilingAhead: 1000 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fkv := kvtest.NewFaultKV(db)
	if err := kvstore.Init(ctx, fkv, uuid.New()); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(ctx, fkv)
	if err != nil {
		t.Fatal(err)
	}

	p, err := secrets.New(st.ControlPlane(), rotKeys(t))
	if err != nil {
		t.Fatal(err)
	}

	fx := &rotFixture{
		clock:  clock,
		cp:     st.ControlPlane(),
		store:  st,
		p:      p,
		fkv:    fkv,
		faults: &switchFaults{m: map[string]error{}},
	}
	fx.d = identitytest.NewDomainController("CORP", machineRealm, "DC=corp,DC=example", dc1, dc2)
	fx.d.AddUser(t, "alice", alicePW)
	fx.d.AddUser(t, "svc", []byte(bindPW))

	bindRef := entity.SecretRef(uuid.New())
	bind, err := p.Seal(ctx, secrets.Protocol, entity.SecretDirectoryCredential,
		entity.SecretOwner{
			Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
			Key:   identity.DirectoryBindCredentialKey,
		}, bindRef, []byte(bindPW), secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.cp.PutSecret(ctx, bind, 0); err != nil {
		t.Fatal(err)
	}

	noPAC := false // a ticket the derived keys verify signs in without one
	fx.rec = directory.Record{Address: "ldaps://dc1.corp.example", Base: "DC=corp,DC=example", Realm: machineRealm, NetBIOSDomain: "CORP", RequirePAC: &noPAC,
		BindName: "CN=svc,CN=Users,DC=corp,DC=example", BindCredential: bindRef, DCAddress: dc1}

	if fx.in, err = st.Installation(ctx); err != nil {
		t.Fatal(err)
	}
	fx.restart(t, p)
	return fx
}

// seal seals password as the machine password's next version with m, in
// one write with also's.
func (fx *rotFixture) seal(t *testing.T, password string, m secrets.Meta, also func(entity.ControlPlaneTx) error) {
	t.Helper()
	ctx := context.Background()
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, Key: "machine_password"}
	sec, err := fx.p.Seal(ctx, secrets.Protocol, entity.SecretMachinePassword, owner, fx.in.MachinePassword, []byte(password), m)
	if err != nil {
		t.Fatal(err)
	}
	err = fx.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(sec, sec.Version-1); err != nil {
			return err
		}
		if err := tx.SetMachinePassword(fx.in.MachinePassword); err != nil {
			return err
		}
		if also != nil {
			return also(tx)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// restart builds a fresh server over the same store and secrets, as a node
// restarts, and delivers it the store's State. Every test passes the real
// provider here as a SecretProvider, so a signature drift fails the build.
func (fx *rotFixture) restart(t *testing.T, secrets identity.SecretProvider) {
	t.Helper()
	if fx.s != nil {
		_ = fx.s.Stop(context.Background())
	}
	fx.reg = metrics.New("test", "test")
	s, err := identity.New(identity.Deps{Store: fx.store, Clock: fx.clock, Secrets: secrets, Metrics: fx.reg,
		DomainController: identitytest.NewFaultyDomainController(fx.d, fx.faults), ControlPlane: fx.cp, Logons: &logonRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	fx.s = s
	fx.reconcile(t)
}

// versions are the machine password's versions in the store, current first.
func (fx *rotFixture) versions(t *testing.T) []secrets.Version {
	t.Helper()
	in, err := fx.store.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.MachinePassword == (entity.SecretRef{}) {
		return nil
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, Key: "machine_password"}
	vs, err := fx.p.Versions(context.Background(), secrets.Protocol, entity.SecretMachinePassword, owner, in.MachinePassword)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

func (fx *rotFixture) join(t *testing.T) entity.MachineJoin {
	t.Helper()
	in, err := fx.store.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return in.Join
}

// netbiosName is identity.netbios_name as the store holds it, if set.
func (fx *rotFixture) netbiosName(t *testing.T) (string, bool) {
	t.Helper()
	for s, err := range fx.cp.Settings(context.Background(), entity.SettingScope{Kind: entity.ScopeInstallation}) {
		if err != nil {
			t.Fatal(err)
		}
		if s.Key == "identity.netbios_name" {
			var name string
			if err := json.Unmarshal(s.Value, &name); err != nil {
				t.Fatal(err)
			}
			return name, true
		}
	}
	return "", false
}

// reconcile applies the store's State, as the node's reconcile does.
func (fx *rotFixture) reconcile(t *testing.T) {
	t.Helper()
	st := state(1)
	rec := fx.rec
	st.Directory = &rec
	st.Machine = fx.versions(t)
	st.Join = fx.join(t)
	if name, ok := fx.netbiosName(t); ok {
		st.Settings.NetBIOSName = name
	}
	if err := fx.s.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func (fx *rotFixture) phase(op *entity.Operation) string {
	if op.Kind == string(apiv1.OpJoinDomain) {
		return fx.s.JoinProgress(context.Background(), *op).Phase
	}
	return fx.s.RotateProgress(context.Background(), *op).Phase
}

// turn runs one turn of op as the runner does, the store's State delivered
// first unless held: a retried error fails the test, a failure ends op, a
// rotation's with RotateFailed written, and is returned, and a done turn's
// End is written.
func (fx *rotFixture) turn(t *testing.T, op *entity.Operation) (done bool, failed error) {
	t.Helper()
	if op.State != entity.OpRunning {
		t.Fatalf("turn of an operation already %v", op.State)
	}
	if !fx.hold {
		fx.reconcile(t)
	}
	run := fx.s.RotateStep
	if op.Kind == string(apiv1.OpJoinDomain) {
		run = fx.s.JoinStep
	}
	res, err := step.Run(context.Background(), run, *op)
	if _, fails := step.Failure(err); fails {
		op.State = entity.OpFailed
		if op.Kind != string(apiv1.OpJoinDomain) {
			if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, fx.s.RotateFailed); err != nil {
				t.Fatalf("the rotation's failed write: %v", err)
			}
		}
		fx.reconcile(t)
		return true, err
	}
	if err != nil {
		t.Fatalf("turn at phase %q: retried error %v", fx.phase(op), err)
	}
	op.Step = res.Next
	if !res.Done {
		return false, nil
	}
	if res.End != nil {
		if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, res.End); err != nil {
			t.Fatalf("the %s's End: %v", op.Kind, err)
		}
	}
	op.State, op.Result = entity.OpSucceeded, res.Result
	fx.reconcile(t)
	return true, nil
}

// runs runs one turn of op and wants op still running.
func (fx *rotFixture) runs(t *testing.T, op *entity.Operation) {
	t.Helper()
	if done, err := fx.turn(t, op); done {
		t.Fatalf("turn ended the %s (%v), want it running", op.Kind, err)
	}
}

// until runs op's turns until it reaches phase, without moving the clock.
func (fx *rotFixture) until(t *testing.T, op *entity.Operation, phase string) {
	t.Helper()
	for range 20 {
		if fx.phase(op) == phase {
			return
		}
		if done, err := fx.turn(t, op); done {
			t.Fatalf("op ended (%v) before phase %q", err, phase)
		}
	}
	t.Fatalf("op never reached phase %q; at %q", phase, fx.phase(op))
}

// complete runs op's turns to the end, a minute apart, and wants it done.
func (fx *rotFixture) complete(t *testing.T, op *entity.Operation) {
	t.Helper()
	for range 50 {
		done, err := fx.turn(t, op)
		if err != nil {
			t.Fatalf("%s failed at phase %q: %v", op.Kind, fx.phase(op), err)
		}
		if done {
			return
		}
		fx.clock.Advance(time.Minute)
	}
	t.Fatalf("%s not done after 50 turns; at phase %q", op.Kind, fx.phase(op))
}

// wantFailed runs a turn of op and wants it to fail as a problem naming what.
func (fx *rotFixture) wantFailed(t *testing.T, op *entity.Operation, what string) {
	t.Helper()
	done, err := fx.turn(t, op)
	p, ok := errors.AsType[*apiv1.Problem](err)
	if !done || !ok || !strings.Contains(p.Detail, what) {
		t.Fatalf("turn: done %v, err %v; want a problem naming %q", done, err, what)
	}
}

// cursorVersion is op's recorded cursor's sealed version.
func cursorVersion(t *testing.T, op *entity.Operation) uint64 {
	t.Helper()
	var c struct{ Version uint64 }
	if err := json.Unmarshal(op.Step, &c); err != nil {
		t.Fatal(err)
	}
	return c.Version
}

// signsIn wants alice's pass-through answered over the node's channel, set
// up with version.
func (fx *rotFixture) signsIn(t *testing.T, version uint64) {
	t.Helper()
	cf := &chanFixture{s: fx.s}
	if err := cf.logon(context.Background(), "alice", alicePW); err != nil {
		t.Fatalf("alice's logon: %v, want answered", err)
	}
	if _, v, ok := fx.s.ChannelAt(); !ok || v != version {
		t.Errorf("channel set up with version %d (%v), want %d", v, ok, version)
	}
}
