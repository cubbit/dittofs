package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// A hold is readable under its operation's ID until released; the release
// drops it and zeroes the caller's password slice, so the password is left
// in no memory the hold reached.
func TestHoldJoinCredentialRelease(t *testing.T) {
	s := newServer(t)
	id := entity.OperationID{1}
	pw := []byte("Admin-Passw0rd")
	release := s.HoldJoinCredential(id, pid.JoinCredential{User: "Administrator@AD.TEST", Password: pw})

	got, ok := s.HeldCredential(id)
	if !ok || got.User != "Administrator@AD.TEST" || !bytes.Equal(got.Password, []byte("Admin-Passw0rd")) {
		t.Fatalf("HeldCredential before release = %q, %q, %v; want the held user and password", got.User, got.Password, ok)
	}
	if _, ok := s.HeldCredential(entity.OperationID{2}); ok {
		t.Errorf("HeldCredential of another operation = held; want absent")
	}

	release()
	if _, ok := s.HeldCredential(id); ok {
		t.Errorf("HeldCredential after release = held; want absent")
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("caller's password after release = %q; want every byte zero", pw)
	}
	release() // a second call is harmless
}

// A second hold under the same ID replaces the first and zeroes its password,
// and the first's release leaves the second in place.
func TestHoldJoinCredentialReplaced(t *testing.T) {
	s := newServer(t)
	id := entity.OperationID{1}
	pw := []byte("one")
	first := s.HoldJoinCredential(id, pid.JoinCredential{User: "a", Password: pw})
	second := s.HoldJoinCredential(id, pid.JoinCredential{User: "b", Password: []byte("two")})
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("replaced hold's password after the second hold = %q; want every byte zero", pw)
	}
	first()
	if got, ok := s.HeldCredential(id); !ok || got.User != "b" || string(got.Password) != "two" {
		t.Errorf("HeldCredential after the first release = %q, %q, %v; want the second hold", got.User, got.Password, ok)
	}
	second()
}

// A second hold over the caller's same password slice keeps the password: the
// replacement zeroes only a buffer the new hold does not hold.
func TestHoldJoinCredentialReplacedSameSlice(t *testing.T) {
	s := newServer(t)
	id := entity.OperationID{1}
	pw := []byte("one")
	first := s.HoldJoinCredential(id, pid.JoinCredential{User: "a", Password: pw})
	second := s.HoldJoinCredential(id, pid.JoinCredential{User: "a", Password: pw})
	if got, ok := s.HeldCredential(id); !ok || string(got.Password) != "one" {
		t.Errorf("HeldCredential after a hold over the same slice = %q, %v; want %q", got.Password, ok, "one")
	}
	first()
	if got, ok := s.HeldCredential(id); !ok || string(got.Password) != "one" {
		t.Errorf("HeldCredential after the replaced hold's release = %q, %v; want %q, the live hold's buffer kept", got.Password, ok, "one")
	}
	second()
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("password after the live hold's release = %q; want every byte zero", pw)
	}
}

// Stop drops every hold and zeroes its password, so none outlives the server.
func TestStopDropsHolds(t *testing.T) {
	s := newServer(t)
	if err := s.Apply(state(1)); err != nil {
		t.Fatal(err)
	}
	id := entity.OperationID{1}
	pw := []byte("Admin-Passw0rd")
	release := s.HoldJoinCredential(id, pid.JoinCredential{User: "Administrator", Password: pw})
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := s.HeldCredential(id); ok {
		t.Errorf("HeldCredential after Stop = held; want absent")
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("caller's password after Stop = %q; want every byte zero", pw)
	}
	release()
}

// joinOp is a new online join of the installation with spec, the
// administrator's credential held under its ID unless hold is false.
func (fx *rotFixture) joinOp(t *testing.T, spec apiv1.JoinDomainSpec, hold bool) *entity.Operation {
	t.Helper()
	spec.Mode = apiv1.JoinOnline
	params, err := json.Marshal(apiv1.JoinDomainParams{JoinDomainSpec: spec})
	if err != nil {
		t.Fatal(err)
	}
	op := &entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpJoinDomain),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning, Params: params}
	if hold {
		fx.s.HoldJoinCredential(op.ID, fx.d.Admin())
	}
	return op
}

// computer is the account name at addr, read as the administrator.
func (fx *rotFixture) computer(t *testing.T, addr, name string) (netlogon.Account, error) {
	t.Helper()
	ctx := context.Background()
	dc, err := fx.d.Dial(ctx, fx.rec, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	return dc.Account(ctx, fx.d.Admin(), name)
}

// accepts reports whether dc1 sets up a channel as name with password.
func (fx *rotFixture) accepts(t *testing.T, name string, password []byte) bool {
	t.Helper()
	ctx := context.Background()
	dc, err := fx.d.Dial(ctx, fx.rec, dc1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	ch, err := dc.Channel(ctx, name, password)
	if err != nil {
		return false
	}
	_ = ch.Close()
	return true
}

// cursorField is op's recorded cursor's field name.
func cursorField[T any](t *testing.T, op *entity.Operation, name string) T {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(op.Step, &m); err != nil {
		t.Fatal(err)
	}
	var v T
	if raw, ok := m[name]; ok {
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// wantJoined wants the installation joined as account at version, the
// account's name written as identity.netbios_name, and the hold released.
func (fx *rotFixture) wantJoined(t *testing.T, op *entity.Operation, account string, version uint64) {
	t.Helper()
	j := fx.join(t)
	if !j.Joined || j.Account != account+"$" || j.LastRotation.Failed || j.LastRotation.Version != version {
		t.Errorf("join %+v, want joined as %s$ at version %d", j, account, version)
	}
	if name, ok := fx.netbiosName(t); !ok || name != account {
		t.Errorf("identity.netbios_name %q (%v), want %q", name, ok, account)
	}
	if _, held := fx.s.HeldCredential(op.ID); held {
		t.Error("the administrator credential is still held after the join ended")
	}
}

// An online join stopped between its seal and its create, and resumed in the
// same server, creates the account once, with its cifs/ and host/ names, the
// default DNS host name and 0x18, in the Computers container, records the
// object's owner and writes the name in upper case; the channel then signs
// in with the sealed version.
func TestOnlineJoin(t *testing.T) {
	fx := newMachine(t)
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "nas-01"}, true)
	fx.until(t, op, "create")
	if _, err := fx.computer(t, dc1, "NAS-01"); !errors.Is(err, netlogon.ErrNoAccount) {
		t.Fatalf("account before the create: %v, want none", err)
	}
	fx.complete(t, op)
	a, err := fx.computer(t, dc1, "NAS-01")
	if err != nil {
		t.Fatal(err)
	}
	if a.DN != "CN=NAS-01,CN=Computers,DC=corp,DC=example" || a.EncTypes != 0x18 || a.KeyVersion != 1 {
		t.Errorf("account %s, enc types %#x, key version %d; want in CN=Computers, 0x18, created once at 1", a.DN, a.EncTypes, a.KeyVersion)
	}
	for _, n := range []string{"host/NAS-01", "host/nas-01.corp.example", "cifs/NAS-01", "cifs/nas-01.corp.example"} {
		if !slices.ContainsFunc(a.Names, func(h string) bool { return strings.EqualFold(h, n) }) {
			t.Errorf("names %v, want %s among them", a.Names, n)
		}
	}
	fx.wantJoined(t, op, "NAS-01", 1)
	vs := fx.versions(t)
	if len(vs) != 1 || vs[0].Meta.KeyVersion != 1 {
		t.Fatalf("versions %v, want one tagged 1", vs)
	}
	j := fx.join(t)
	owner, _, err := sid.DecodeSID(j.Owner)
	if want := fx.d.Domain().SID; err != nil || !strings.HasPrefix(sid.FormatSID(owner), sid.FormatSID(&want)+"-") {
		t.Errorf("owner %x (%v), want a SID of the domain %s", j.Owner, err, sid.FormatSID(&want))
	}
	if len(j.Names) == 0 || j.LastRotation.Tag != 1 || j.LastRotation.Read != 1 {
		t.Errorf("join %+v, want the account's names, tag 1 read 1", j)
	}
	fx.signsIn(t, vs[0].Number)
}

// Stopped between its seal and its create and resumed by a fresh server,
// which holds no credential, the join fails naming the missing credential,
// leaving the installation not joined, identity.netbios_name unset and the
// version it attempted kept; a join run again with a takeover joins.
func TestOnlineJoinResumedWithoutCredential(t *testing.T) {
	fx := newMachine(t)
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "create")
	attempted := cursorVersion(t, op)
	fx.restart(t, fx.p)
	fx.runs(t, op) // records the attempt
	fx.wantFailed(t, op, "administrator credential")
	fx.restart(t, fx.p)
	if j := fx.join(t); j.Joined || !j.LastRotation.Failed {
		t.Errorf("join %+v after a fresh server, want not joined, failed", j)
	}
	if name, ok := fx.netbiosName(t); ok {
		t.Errorf("identity.netbios_name %q, want unset", name)
	}
	if !slices.ContainsFunc(fx.versions(t), func(v secrets.Version) bool { return v.Number == attempted }) {
		t.Errorf("versions %v, want the attempted version %d kept", fx.versions(t), attempted)
	}
	again := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS", Takeover: true}, true)
	fx.complete(t, again)
	fx.wantJoined(t, again, "NAS", cursorVersion(t, again))
}

// Stopped after its create, before the runner records the turn, and resumed
// by a fresh server with no credential, the join finds the account accepting
// the sealed version and joins, creating nothing.
func TestOnlineJoinResumedAfterCreate(t *testing.T) {
	fx := newMachine(t)
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "create")
	fx.runs(t, op) // records the attempt
	before := op.Step
	fx.runs(t, op) // creates the account
	if fx.phase(op) != "confirm" {
		t.Fatalf("after the create: phase %q, want confirm", fx.phase(op))
	}
	op.Step = before // the runner's record of the create's turn is lost
	fx.restart(t, fx.p)
	fx.complete(t, op)
	fx.wantJoined(t, op, "NAS", cursorVersion(t, op))
	if a, err := fx.computer(t, dc1, "NAS"); err != nil || a.KeyVersion != 1 {
		t.Errorf("account %+v (%v), want created once", a, err)
	}
}

// A join under the name of an existing account is refused, naming it, with
// its password unchanged; with a takeover it joins, the version sealed with
// the controller's number plus one before the password is set, recording the
// account's delegation value as read.
func TestOnlineJoinExistingAccount(t *testing.T) {
	fx := newMachine(t)
	delegation := identitytest.DelegationTo(fx.d.Domain().SID)
	fx.d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: 0x18,
		SPNs: []string{"HOST/NAS", "HOST/nas.corp.example"}, Delegation: delegation})
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "seal")
	fx.wantFailed(t, op, "NAS$ already exists")
	if !fx.accepts(t, "NAS", []byte(pw2)) {
		t.Error("the existing account's password changed by a refused join")
	}
	if vs := fx.versions(t); len(vs) != 0 {
		t.Errorf("versions %v, want nothing sealed", vs)
	}

	op = fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS", Takeover: true}, true)
	fx.until(t, op, "create")
	vs := fx.versions(t)
	if len(vs) != 1 || vs[0].Meta.KeyVersion != 2 || !fx.accepts(t, "NAS", []byte(pw2)) {
		t.Fatalf("at the create: versions %v, old password accepted %v; want one sealed tagged 2, nothing set",
			vs, fx.accepts(t, "NAS", []byte(pw2)))
	}
	fx.complete(t, op)
	fx.wantJoined(t, op, "NAS", vs[0].Number)
	if got := fx.join(t).Delegation; !bytes.Equal(got, delegation) {
		t.Errorf("the join's delegation value %x, want the account's %x", got, delegation)
	}
	if a, err := fx.computer(t, dc1, "NAS"); err != nil || a.KeyVersion != 2 {
		t.Errorf("account %+v (%v), want set once, key version 2", a, err)
	}
	if fx.accepts(t, "NAS", []byte(pw2)) {
		t.Error("the taken-over account still accepts its old password")
	}
}

// A join whose pinned controller's dials are refused for an hour fails
// naming it; a join run again with a takeover pins the other.
func TestOnlineJoinUnansweredPinsAnother(t *testing.T) {
	fx := newMachine(t)
	fx.rec.DCAddress = "" // the domain's controllers, from its SRV records
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "seal")
	if got := cursorField[string](t, op, "dc"); got != dc1 {
		t.Fatalf("pinned %q, want %q", got, dc1)
	}
	fx.faults.set("dial:"+dc1, errDown)
	fx.runs(t, op)
	fx.clock.Advance(59 * time.Minute)
	fx.runs(t, op)
	fx.clock.Advance(time.Minute)
	fx.wantFailed(t, op, "domain controller "+dc1)

	again := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS", Takeover: true}, true)
	fx.complete(t, again)
	var res struct{ DC string }
	if err := json.Unmarshal(again.Result, &res); err != nil || res.DC != dc2 {
		t.Errorf("result %s (%v), want joined at %s", again.Result, err, dc2)
	}
	fx.wantJoined(t, again, "NAS", cursorVersion(t, again))
}

// A join is refused at its first turn, nothing sealed or written, for an
// rfc2307 record, an organisational unit outside the base, a host name of
// one label, an account name outside [A-Z0-9-] or named as the domain, and
// while a rotation runs, naming it.
func TestOnlineJoinRefusedAtFirstTurn(t *testing.T) {
	rot := entity.OperationID(uuid.New())
	for _, tc := range []struct {
		name string
		spec apiv1.JoinDomainSpec
		set  func(*rotFixture)
		want string
	}{
		{"rfc2307", apiv1.JoinDomainSpec{Account: "NAS"}, func(fx *rotFixture) { fx.rec.Schema = directory.SchemaRFC2307 }, "rfc2307"},
		{"ou outside the base", apiv1.JoinDomainSpec{Account: "NAS", OU: "OU=Servers,DC=other,DC=example"}, nil, "not under the directory's base"},
		{"the base itself", apiv1.JoinDomainSpec{Account: "NAS", OU: "DC=corp,DC=example"}, nil, "not under the directory's base"},
		{"one-label host", apiv1.JoinDomainSpec{Account: "NAS", DNSHostName: "nas"}, nil, "not a DNS name"},
		{"underscore", apiv1.JoinDomainSpec{Account: "NAS_01"}, nil, "NAS_01"},
		{"16 bytes", apiv1.JoinDomainSpec{Account: strings.Repeat("N", 16)}, nil, "not 1 to 15 bytes"},
		{"the NetBIOS domain", apiv1.JoinDomainSpec{Account: "corp"}, nil, "named as the domain CORP"},
		{"a rotation running", apiv1.JoinDomainSpec{Account: "NAS"}, func(fx *rotFixture) {
			if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				_, err := tx.PutOperation(entity.Operation{ID: rot, Kind: string(apiv1.OpRotateMachinePassword),
					Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning}, 0)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}, uuid.UUID(rot).String()[:8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMachine(t)
			if tc.set != nil {
				tc.set(fx)
			}
			before := fx.join(t)
			op := fx.joinOp(t, tc.spec, true)
			fx.wantFailed(t, op, tc.want)
			if vs := fx.versions(t); len(vs) != 0 {
				t.Errorf("versions %v, want nothing sealed", vs)
			}
			if after := fx.join(t); after.Joined != before.Joined || after.LastRotation != before.LastRotation {
				t.Errorf("join %+v, want unchanged %+v", after, before)
			}
			if _, held := fx.s.HeldCredential(op.ID); held {
				t.Error("the credential is still held after the refused join")
			}
		})
	}
}

// The join's last turn writes Join and identity.netbios_name in its End, one
// transaction: a commit that fails writes neither.
func TestOnlineJoinEndWritesBothOrNeither(t *testing.T) {
	fx := newMachine(t)
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "confirm")
	fx.reconcile(t)
	if name, ok := fx.netbiosName(t); ok || fx.join(t).Joined {
		t.Fatalf("before the last turn: joined %v, identity.netbios_name %q; want neither written", fx.join(t).Joined, name)
	}
	res, err := step.Run(context.Background(), fx.s.JoinStep, *op)
	if err != nil || !res.Done || res.End == nil {
		t.Fatalf("last turn: done %v, err %v; want done with an End", res.Done, err)
	}
	if name, ok := fx.netbiosName(t); ok || fx.join(t).Joined {
		t.Fatalf("after the last turn, before its End: joined %v, identity.netbios_name %q; want neither written", fx.join(t).Joined, name)
	}
	errCommit := errors.New("commit failed")
	fault, err := sim.NewSchedule(1, fx.clock, sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Err: errCommit}})
	if err != nil {
		t.Fatal(err)
	}
	fx.fkv.SetFault(fault)
	if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, res.End); !errors.Is(err, errCommit) {
		t.Fatalf("End's write: %v, want the injected failure", err)
	}
	fx.fkv.SetFault(nil)
	if name, ok := fx.netbiosName(t); ok || fx.join(t).Joined {
		t.Errorf("after a failed End: joined %v, identity.netbios_name %q; want neither written", fx.join(t).Joined, name)
	}
}

// On a joined installation, a join refused before it sealed anything leaves
// it joined; one that fails after its seal leaves it not joined, with
// identity.netbios_name as it was.
func TestOnlineJoinFailureOnJoinedInstallation(t *testing.T) {
	fx := newRotation(t)
	op := fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS"}, true)
	fx.until(t, op, "seal")
	fx.wantFailed(t, op, "NAS$ already exists")
	if j := fx.join(t); !j.Joined || j.LastRotation.Failed {
		t.Errorf("join %+v after a refused join, want still joined", j)
	}

	op = fx.joinOp(t, apiv1.JoinDomainSpec{Account: "NAS", Takeover: true}, false)
	fx.until(t, op, "create")
	fx.runs(t, op) // records the attempt
	fx.wantFailed(t, op, "administrator credential")
	fx.restart(t, fx.p)
	if j := fx.join(t); j.Joined || !j.LastRotation.Failed {
		t.Errorf("join %+v after a join failed past its seal, want not joined, failed", j)
	}
	if name, ok := fx.netbiosName(t); ok {
		t.Errorf("identity.netbios_name %q, want unset", name)
	}
}
