package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// stageNAS creates NAS$ at the controller with pw2 and msDS-SupportedEncryptionTypes
// enc, as an administrator creates it for an offline join.
func (fx *rotFixture) stageNAS(t *testing.T, enc uint32, delegation []byte) {
	t.Helper()
	fx.d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: enc,
		SPNs: []string{"HOST/NAS", "HOST/nas.corp.example"}, Delegation: delegation})
}

// handOver seals password as the machine password's current version with m,
// as the API's hand-over does, and answers its number.
func (fx *rotFixture) handOver(t *testing.T, password string, m secrets.Meta) uint64 {
	t.Helper()
	in, err := fx.store.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fx.in.MachinePassword = in.MachinePassword
	if fx.in.MachinePassword == (entity.SecretRef{}) {
		fx.in.MachinePassword = entity.SecretRef(uuid.New())
	}
	fx.seal(t, password, m, nil)
	return fx.versions(t)[0].Number
}

// offlineOp is a new offline join of account naming version.
func (fx *rotFixture) offlineOp(t *testing.T, account string, version uint64, takeover bool) *entity.Operation {
	t.Helper()
	params, err := json.Marshal(apiv1.JoinDomainParams{JoinDomainSpec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline,
		Account: account, Takeover: takeover}, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	return &entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpJoinDomain),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning, Params: params}
}

// offline hands password over and starts an offline join of NAS naming it.
func (fx *rotFixture) offline(t *testing.T, password string, takeover bool) *entity.Operation {
	t.Helper()
	return fx.offlineOp(t, "NAS$", fx.handOver(t, password, secrets.Meta{HandedOver: true}), takeover)
}

// noPassThrough wants alice's pass-through refused with no channel set up.
func (fx *rotFixture) noPassThrough(t *testing.T, when string) {
	t.Helper()
	cf := &chanFixture{s: fx.s}
	if err := cf.logon(context.Background(), "alice", alicePW); err == nil {
		t.Errorf("%s: alice's pass-through answered, want refused", when)
	}
	if a, v, ok := fx.s.ChannelAt(); ok {
		t.Errorf("%s: channel at %q with version %d, want none", when, a, v)
	}
}

// An account whose msDS-SupportedEncryptionTypes lacks an AES bit, or is
// absent, is refused naming the attribute and 0x18, before any channel is
// set up with its password: a channel that could not be asked would only
// retry the turn, so the refusal shows none was tried.
func TestOfflineJoinRefusesNonAES(t *testing.T) {
	for _, enc := range []uint32{0x10, 0} {
		t.Run(fmt.Sprintf("%#x", enc), func(t *testing.T) {
			fx := newMachine(t)
			fx.stageNAS(t, enc, nil)
			op := fx.offline(t, pw2, false)
			fx.until(t, op, "verify")
			fx.faults.set("channel", errDown)
			fx.wantFailed(t, op, fmt.Sprintf("msDS-SupportedEncryptionTypes is %#x; set it to 0x18", enc))
			if vs := fx.versions(t); len(vs) != 1 {
				t.Errorf("versions %v, want the handed-over one alone", vs)
			}
			if j := fx.join(t); j.Joined || j.LastRotation.Failed {
				t.Errorf("join %+v, want left as it was", j)
			}
		})
	}
}

// An offline join of an account advertising AES joins: the handed-over
// password rotated twice, the account's names, owner and delegation reported
// in the result and on Join, and a ticket under a key derived from the
// handed-over password refused before the join and after it. After it the
// handed-over password sets up no channel at the controller, and a
// controller refusing the new versions is not signed in to with it.
func TestOfflineJoin(t *testing.T) {
	fx := newMachine(t)
	delegation := identitytest.DelegationTo(fx.d.Domain().SID)
	fx.stageNAS(t, 0x18, delegation)
	op := fx.offline(t, pw2, false)
	fx.until(t, op, "confirm2")
	oc, err := ticketSignIn(fx.s, fx.clock, machineSvc, 1, machineKey(t, pw2))
	wantSignIn(t, "the handed-over key before the join", oc, err, "unavailable")
	fx.noPassThrough(t, "before the second confirm")

	fx.complete(t, op)
	vs := fx.versions(t)
	if len(vs) != 3 || !vs[2].Meta.HandedOver || vs[1].Meta.KeyVersion != 2 || vs[0].Meta.KeyVersion != 3 {
		t.Fatalf("versions %v, want the handed-over one under two rotations tagged 2 and 3", vs)
	}
	fx.wantJoined(t, op, "NAS", vs[0].Number)
	if got := fx.kvno(t); got != 3 {
		t.Errorf("account's key version number %d, want 3: two sets", got)
	}
	j := fx.join(t)
	if !bytes.Equal(j.Delegation, delegation) || len(j.Owner) == 0 || !slices.Contains(j.Names, "cifs/nas.corp.example") {
		t.Errorf("join %+v, want the account's owner, delegation and names", j)
	}
	var res struct {
		Names      []string
		Owner      string
		Delegation []byte
	}
	if err := json.Unmarshal(op.Result, &res); err != nil || !slices.Contains(res.Names, "cifs/nas.corp.example") ||
		!strings.HasPrefix(res.Owner, "S-1-5-21-") || !bytes.Equal(res.Delegation, delegation) {
		t.Errorf("result %s (%v), want the names, owner and delegation reported", op.Result, err)
	}
	oc, err = ticketSignIn(fx.s, fx.clock, machineSvc, 1, machineKey(t, pw2))
	wantSignIn(t, "the handed-over key after the join", oc, err, "logon_failure")
	fx.wantTicket(t, 3, string(vs[0].Value))
	fx.signsIn(t, vs[0].Number)
	if fx.accepts(t, "NAS", []byte(pw2)) {
		t.Error("the handed-over password sets up a channel after the join")
	}

	// The controller back on the handed-over password refuses both rotations'.
	fx.d.SetComputerPassword(t, "NAS", []byte(pw2))
	fx.restart(t, fx.p)
	fx.noPassThrough(t, "at a controller refusing the new versions")
}

// A version not marked HandedOver, or one no longer current, is refused at
// the first turn, naming it, and nothing is sealed.
func TestOfflineJoinRefusesUnmarkedVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		hand func(*rotFixture) uint64
	}{
		{"unmarked", func(fx *rotFixture) uint64 { return fx.handOver(t, pw2, secrets.Meta{}) }},
		{"none named", func(fx *rotFixture) uint64 {
			fx.handOver(t, pw2, secrets.Meta{HandedOver: true})
			return 0
		}},
		{"superseded", func(fx *rotFixture) uint64 {
			v := fx.handOver(t, pw2, secrets.Meta{HandedOver: true})
			fx.handOver(t, pw3, secrets.Meta{HandedOver: true})
			return v
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMachine(t)
			fx.stageNAS(t, 0x18, nil)
			v := tc.hand(fx)
			before := len(fx.versions(t))
			op := fx.offlineOp(t, "NAS", v, false)
			fx.wantFailed(t, op, "version "+strconv.FormatUint(v, 10)+" is not the current one marked handed over")
			if got := len(fx.versions(t)); got != before {
				t.Errorf("versions %d, want %d: nothing sealed", got, before)
			}
		})
	}
}

// A join stopped after the hand-over is stored and resumed by a fresh server
// still finds the version HandedOver, deriving no key; its rotations resume,
// a seal whose turn went unrecorded is taken rather than sealed again, and
// it is joined only at the second confirm: before that turn neither Join nor
// identity.netbios_name is written, and pass-through is refused.
func TestOfflineJoinResumed(t *testing.T) {
	fx := newMachine(t)
	fx.stageNAS(t, 0x18, nil)
	op := fx.offline(t, pw2, false)
	fx.until(t, op, "seal")
	fx.restart(t, fx.p)
	if vs := fx.versions(t); len(vs) != 1 || !vs[0].Meta.HandedOver {
		t.Fatalf("versions %v after a fresh server, want the handed-over one alone, still marked", vs)
	}
	oc, err := ticketSignIn(fx.s, fx.clock, machineSvc, 1, machineKey(t, pw2))
	wantSignIn(t, "the handed-over key after a fresh server", oc, err, "unavailable")

	before := op.Step
	fx.runs(t, op)   // seals the first rotation's version
	op.Step = before // the runner's record of the seal's turn is lost
	fx.restart(t, fx.p)
	fx.until(t, op, "confirm2")
	if vs := fx.versions(t); len(vs) != 3 {
		t.Errorf("versions %v, want the handed-over one and one per rotation", vs)
	}
	fx.reconcile(t)
	if name, ok := fx.netbiosName(t); ok || fx.join(t).Joined {
		t.Fatalf("before the second confirm: joined %v, identity.netbios_name %q; want neither written", fx.join(t).Joined, name)
	}
	fx.noPassThrough(t, "before the second confirm")
	fx.complete(t, op)
	fx.wantJoined(t, op, "NAS", fx.versions(t)[0].Number)
	if got := fx.kvno(t); got != 3 {
		t.Errorf("account's key version number %d, want 3: each version set once", got)
	}
}

// The second closing rotation failing after its set leaves the installation
// not joined, after a fresh server too, with both rotations' versions kept;
// an offline join run again once the account's password is reset completes.
func TestOfflineJoinFailedAfterSecondSet(t *testing.T) {
	fx := newMachine(t)
	fx.stageNAS(t, 0x18, nil)
	op := fx.offline(t, pw2, false)
	fx.until(t, op, "confirm2")
	sent := []uint64{cursorField[uint64](t, op, "first"), cursorVersion(t, op)}
	fx.unanswered(t, op)
	fx.restart(t, fx.p)
	if j := fx.join(t); j.Joined || !j.LastRotation.Failed {
		t.Errorf("join %+v after a fresh server, want not joined, failed", j)
	}
	if name, ok := fx.netbiosName(t); ok {
		t.Errorf("identity.netbios_name %q, want unset", name)
	}
	for _, n := range sent {
		if !slices.ContainsFunc(fx.versions(t), func(v secrets.Version) bool { return v.Number == n }) {
			t.Errorf("versions %v, want the sent version %d kept", fx.versions(t), n)
		}
	}

	fx.d.SetComputerPassword(t, "NAS", []byte(pw4)) // an administrator's reset
	again := fx.offline(t, pw4, false)
	fx.complete(t, again)
	fx.wantJoined(t, again, "NAS", fx.versions(t)[0].Number)
}

// A set by the account's creator between the closing rotations, before the
// second seals its version or after, raises the number a third time: the
// second confirm fails the join naming the number read, and it is not joined.
func TestOfflineJoinThirdSet(t *testing.T) {
	for _, at := range []string{"seal2", "set2"} {
		t.Run(at, func(t *testing.T) {
			fx := newMachine(t)
			fx.stageNAS(t, 0x18, nil)
			op := fx.offline(t, pw2, false)
			fx.until(t, op, at)
			first := fx.versions(t)[0]
			if at == "set2" {
				first = fx.versions(t)[1]
			}
			// The creator sets the first rotation's password again over a
			// channel it authenticates, as one that learned it would.
			fx.d.SetComputerPassword(t, "NAS", first.Value)
			fx.until(t, op, "confirm2")
			fx.wantFailed(t, op, "key version number 4, not 3")
			if j := fx.join(t); j.Joined {
				t.Errorf("join %+v, want not joined", j)
			}
			if name, ok := fx.netbiosName(t); ok {
				t.Errorf("identity.netbios_name %q, want unset", name)
			}
		})
	}
}

// An offline join started while a rotation runs is refused at its first
// turn naming it, nothing sealed and the installation not joined.
func TestOfflineJoinRefusedDuringRotation(t *testing.T) {
	fx := newMachine(t)
	fx.stageNAS(t, 0x18, nil)
	rot := entity.OperationID(uuid.New())
	if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, err := tx.PutOperation(entity.Operation{ID: rot, Kind: string(apiv1.OpRotateMachinePassword),
			Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning}, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	op := fx.offline(t, pw2, false)
	fx.wantFailed(t, op, uuid.UUID(rot).String()[:8])
	if vs := fx.versions(t); len(vs) != 1 {
		t.Errorf("versions %v, want the handed-over one alone", vs)
	}
	if fx.join(t).Joined {
		t.Error("the installation reads joined")
	}
}

// On an installation joined as NAS, an offline join of another account with
// no takeover is refused at its first turn, still joined as NAS; one of NAS$
// taking it over after its password is reset joins. While it runs, the old
// join still applied, the handed-over version derives no key and sets up no
// channel, though the controller accepts it.
func TestOfflineJoinOfAJoinedInstallation(t *testing.T) {
	fx := newRotation(t)
	op := fx.offlineOp(t, "OTHER", fx.handOver(t, pw3, secrets.Meta{HandedOver: true}), false)
	fx.wantFailed(t, op, "a new join needs the takeover")
	if j := fx.join(t); !j.Joined || j.Account != "NAS$" || j.LastRotation.Failed {
		t.Errorf("join %+v, want still joined as NAS$", j)
	}

	fx.d.SetComputerPassword(t, "NAS", []byte(pw4)) // an administrator's reset
	op = fx.offline(t, pw4, true)
	fx.until(t, op, "seal")
	fx.reconcile(t)
	oc, err := ticketSignIn(fx.s, fx.clock, machineSvc, 2, machineKey(t, pw4))
	wantSignIn(t, "the handed-over key during the takeover", oc, err, "logon_failure")
	fx.noPassThrough(t, "during the takeover, the controller on the handed-over password")
	fx.complete(t, op)
	fx.wantJoined(t, op, "NAS", fx.versions(t)[0].Number)
}
