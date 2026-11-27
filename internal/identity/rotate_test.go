package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// failingPrune is the real provider with Prune failing as err: a fault
// injected around it.
type failingPrune struct {
	*secrets.Provider
	err error
}

func (f failingPrune) Prune(
	context.Context,
	entity.ControlPlaneTx,
	entity.SecretKind,
	entity.SecretOwner,
	entity.SecretRef,
) error {
	return f.err
}

// droppedVersion is the real provider with Versions no longer listing
// version drop: a fault injected around it, as a prune that dropped the
// version behind a running rotation.
type droppedVersion struct {
	*secrets.Provider
	drop uint64
}

func (f droppedVersion) Versions(ctx context.Context, role secrets.Role, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]secrets.Version, error) {
	vs, err := f.Provider.Versions(ctx, role, kind, owner, ref)
	return slices.DeleteFunc(vs, func(v secrets.Version) bool { return v.Number == f.drop }), err
}

func newRotation(t *testing.T) *rotFixture {
	t.Helper()
	fx := newMachine(t)
	fx.d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: 0x18,
		SPNs: []string{"HOST/NAS", "HOST/nas.corp.example"}})
	fx.in.MachinePassword = entity.SecretRef(uuid.New())
	fx.seal(t, pw2, secrets.Meta{KeyVersion: 1}, func(tx entity.ControlPlaneTx) error {
		j := entity.MachineJoin{Joined: true, Account: "NAS$", Names: []string{"cifs/NAS.corp.example", "host/nas.corp.example"}}
		j.LastRotation.At, j.LastRotation.Confirmed = tx.Now(), tx.Now()
		j.LastRotation.Tag, j.LastRotation.Read, j.LastRotation.Version = 1, 1, 1
		return tx.SetMachineJoin(j)
	})
	fx.restart(t, fx.p)
	return fx
}

// op is a new rotation of the installation.
func (fx *rotFixture) op() *entity.Operation {
	return &entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpRotateMachinePassword),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning}
}

// unanswered fails dc1 from op's next turn and runs turns an hour apart until
// op fails: it wants the failure to name dc1 at the hour, and not before.
func (fx *rotFixture) unanswered(t *testing.T, op *entity.Operation) {
	t.Helper()
	fx.faults.set("dial", errDown)
	defer fx.faults.set("dial", nil)
	if done, err := fx.turn(t, op); done {
		t.Fatalf("first unanswered turn ended the rotation: %v", err)
	}
	fx.clock.Advance(59 * time.Minute)
	if done, err := fx.turn(t, op); done {
		t.Fatalf("unanswered for 59 minutes: rotation ended (%v), want still running", err)
	}
	fx.clock.Advance(time.Minute)
	fx.wantFailed(t, op, "domain controller "+dc1)
}

// kvno is the account's key version number at dc1.
func (fx *rotFixture) kvno(t *testing.T) uint32 {
	t.Helper()
	ctx := context.Background()
	dc, err := fx.d.Dial(ctx, fx.rec, dc1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	a, err := dc.Account(ctx, fx.d.Admin(), "NAS")
	if err != nil {
		t.Fatal(err)
	}
	return a.KeyVersion
}

// mismatches is identity_machine_key_version_mismatches_total's value.
func (fx *rotFixture) mismatches(t *testing.T) float64 {
	t.Helper()
	mfs, err := fx.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == metrics.Namespace+"_identity_machine_key_version_mismatches_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// atStore reports whether the store time got is want's: the store moves its
// time on by a tick per transaction when the clock has not moved.
func atStore(got, want time.Time) bool {
	return !got.Before(want) && got.Sub(want) < time.Second
}

// rotationFailed reports machine_password_rotation_failed on the installation.
func rotationFailed(s *identity.Server) bool {
	for _, c := range s.Conditions(nil) {
		if c.Name == "machine_password_rotation_failed" && c.Subject.Scope == health.ScopeInstallation {
			return true
		}
	}
	return false
}

// wantTicket wants a ticket for cifs/ under kvno and password's key signed in.
func (fx *rotFixture) wantTicket(t *testing.T, kvno int, password string) {
	t.Helper()
	oc, err := ticketSignIn(fx.s, fx.clock, machineSvc, kvno, machineKey(t, password))
	wantSignIn(t, "ticket under kvno "+strconv.Itoa(kvno), oc, err, "ok")
}

// A rotation seals a version tagged with the controller's number plus one,
// sets it, confirms it and records it; a fresh server over the same store
// accepts tickets under both numbers and reads the last rotation back.
func TestRotation(t *testing.T) {
	fx := newRotation(t)
	// Past the retention, so the first turn calls the provider's Prune.
	fx.clock.Advance(11 * time.Hour)
	op := fx.op()
	fx.complete(t, op)
	vs := fx.versions(t)
	if len(vs) != 2 || vs[0].Number != 2 || vs[0].Meta.KeyVersion != 2 {
		t.Fatalf("versions %v, want 2 tagged 2 over 1", vs)
	}
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2", got)
	}
	confirmed := fx.clock.Now()
	fx.restart(t, fx.p)
	lr := fx.s.Applied().Join.LastRotation
	if lr.Failed || lr.Tag != 2 || lr.Read != 2 || lr.Version != 2 || !atStore(lr.Confirmed, confirmed) || !atStore(lr.At, confirmed) {
		t.Errorf("last rotation %+v, want completed at %v, tag 2, read 2, version 2", lr, confirmed)
	}
	if names := fx.s.Applied().Join.Names; !strings.Contains(strings.Join(names, " "), "cifs/nas.corp.example") {
		t.Errorf("names %v, want the account's, sPNMappings applied", names)
	}
	fx.wantTicket(t, 1, pw2)
	fx.wantTicket(t, 2, string(vs[0].Value))
	fx.signsIn(t, 2)
	if rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed raised after a completed rotation")
	}
	var res struct {
		Version uint64
		Tag     uint32
	}
	if err := json.Unmarshal(op.Result, &res); err != nil || res.Version != 2 || res.Tag != 2 {
		t.Errorf("result %s (%v), want version 2, tag 2", op.Result, err)
	}
}

// A crash after the seal commits and before the runner records the turn
// runs the seal again: it takes the version it sealed, so the password is
// set once and the number rises by one.
func TestRotationResumedSeal(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "seal")
	before := op.Step
	fx.runs(t, op)
	op.Step = before // the runner's record of the seal's turn is lost
	fx.complete(t, op)
	if vs := fx.versions(t); len(vs) != 2 {
		t.Errorf("versions %v, want the one sealed over the first", vs)
	}
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2: set once", got)
	}
	fx.signsIn(t, 2)
}

// A controller unanswered for an hour after the set fails the rotation,
// naming it; the condition is raised, still raised after a restart, and
// tickets under both versions verify.
func TestRotationUnansweredAfterSet(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "confirm")
	fx.unanswered(t, op)
	if !rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed not raised by the failed rotation")
	}
	if j := fx.join(t); !j.LastRotation.Failed || j.LastRotation.Version != 1 || !atStore(j.LastRotation.At, fx.clock.Now()) {
		t.Errorf("last rotation %+v, want failed now, version still 1", j.LastRotation)
	}
	fx.restart(t, fx.p)
	if !rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed cleared by a restart")
	}
	vs := fx.versions(t)
	fx.wantTicket(t, 1, pw2)
	fx.wantTicket(t, 2, string(vs[0].Value))
}

// Two rotations failing in a row before their set leave one unconfirmed
// version: the second takes the first's, and the third sets it.
func TestRotationFailuresKeepOneVersion(t *testing.T) {
	fx := newRotation(t)
	for i := range 2 {
		op := fx.op()
		fx.until(t, op, "set")
		if got := cursorVersion(t, op); got != 2 {
			t.Fatalf("rotation %d took version %d, want 2", i+1, got)
		}
		fx.unanswered(t, op)
		if vs := fx.versions(t); len(vs) != 2 {
			t.Fatalf("after failure %d: versions %v, want one unconfirmed over the first", i+1, vs)
		}
		fx.signsIn(t, 1)
	}
	op := fx.op()
	fx.complete(t, op)
	if vs := fx.versions(t); len(vs) != 2 || cursorVersion(t, op) != 2 {
		t.Errorf("versions %v, completed version %d, want 2 set over 1", vs, cursorVersion(t, op))
	}
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2", got)
	}
}

// A set the controller took, its answer lost, then a controller gone for an
// hour: the next rotation probes, finds the version accepted and does not
// set it again.
func TestRotationSetTakenNotRepeated(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "set")
	fx.faults.set("setpassword", errDown)
	fx.runs(t, op)
	if got := fx.kvno(t); got != 1 || !strings.Contains(string(op.Step), `"attempted":true`) {
		t.Fatalf("first set turn: number %d, cursor %s; want the attempt recorded and nothing sent", got, op.Step)
	}
	fx.runs(t, op) // the set sent and its answer lost
	fx.faults.set("setpassword", nil)
	if got := fx.kvno(t); got != 2 {
		t.Fatalf("account's key version number %d after the lost answer, want 2", got)
	}
	fx.unanswered(t, op)
	fx.restart(t, fx.p)
	fx.complete(t, fx.op())
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2: the version set once", got)
	}
	if !atStore(fx.join(t).LastRotation.Confirmed, fx.clock.Now()) || rotationFailed(fx.s) {
		t.Errorf("last rotation %+v, want completed now", fx.join(t).LastRotation)
	}
}

// A confirm reading a number other than the tag completes the rotation,
// counts the mismatch once and keeps the tag; a ticket under the number read
// verifies.
func TestRotationConfirmMismatch(t *testing.T) {
	fx := newRotation(t)
	for range 5 {
		fx.d.SetComputerPassword(t, "NAS", []byte(pw2)) // the number to 6
	}
	op := fx.op()
	fx.until(t, op, "confirm")
	vs := fx.versions(t)
	fx.d.SetComputerPassword(t, "NAS", vs[0].Value) // another set of the same password: 8
	fx.complete(t, op)
	lr := fx.join(t).LastRotation
	if lr.Tag != 7 || lr.Read != 8 || fx.versions(t)[0].Meta.KeyVersion != 7 {
		t.Errorf("last rotation tag %d read %d, version tagged %d; want tag 7 kept, 8 read", lr.Tag, lr.Read, fx.versions(t)[0].Meta.KeyVersion)
	}
	if got := fx.mismatches(t); got != 1 {
		t.Errorf("mismatches counted %v, want 1", got)
	}
	fx.wantTicket(t, 8, string(vs[0].Value))
}

// A confirm at a controller that no longer accepts the version fails the
// rotation, naming it.
func TestRotationConfirmNotAccepted(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "confirm")
	fx.d.SetComputerPassword(t, "NAS", []byte(pw3))
	fx.wantFailed(t, op, "domain controller "+dc1+" no longer accepts")
	if !rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed not raised")
	}
}

// A sealed version no longer held when the set reads it fails the rotation,
// recorded and raised.
func TestRotationSealedNoLongerHeld(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "set")
	fx.restart(t, droppedVersion{fx.p, cursorVersion(t, op)})
	fx.wantFailed(t, op, "version 2 is no longer held")
	if j := fx.join(t); !j.LastRotation.Failed || !atStore(j.LastRotation.At, fx.clock.Now()) {
		t.Errorf("last rotation %+v, want failed now", j.LastRotation)
	}
	if !rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed not raised by a sealed version no longer held")
	}
}

// A node that does not hold the previous version holds the rotation back:
// nothing is sealed, it still waits at 59 minutes and fails at the hour,
// raising the condition, which a restart keeps.
func TestRotationWaitsForPrevious(t *testing.T) {
	fx := newRotation(t)
	st := *fx.s.Applied()
	st.Machine = nil // this node has not applied the store's version
	if err := fx.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	fx.hold = true
	op := fx.op()
	fx.until(t, op, "previous")
	fx.runs(t, op) // the wait starts
	fx.clock.Advance(59 * time.Minute)
	if done, err := fx.turn(t, op); done {
		t.Fatalf("at 59 minutes: rotation ended (%v), want still waiting", err)
	}
	fx.clock.Advance(time.Minute)
	fx.wantFailed(t, op, "does not hold the machine password's version 1")
	if vs := fx.versions(t); len(vs) != 1 {
		t.Errorf("versions %v, want nothing sealed", vs)
	}
	if j := fx.join(t); !j.LastRotation.Failed || !atStore(j.LastRotation.At, fx.clock.Now()) {
		t.Errorf("last rotation %+v, want failed now", j.LastRotation)
	}
	fx.hold = false
	fx.restart(t, fx.p)
	if !rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed not raised after a restart")
	}
}

// The set waits until this node holds the sealed version, however long.
func TestRotationSetWaitsForSealed(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "seal")
	fx.hold = true
	fx.runs(t, op)
	for range 3 {
		fx.clock.Advance(time.Hour)
		if done, err := fx.turn(t, op); done {
			t.Fatalf("rotation ended (%v) while this node lacks the sealed version", err)
		}
	}
	if got := fx.kvno(t); got != 1 || fx.phase(op) != "applied" {
		t.Fatalf("number %d at phase %q, want 1, waiting at applied", got, fx.phase(op))
	}
	fx.hold = false
	fx.complete(t, op)
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2", got)
	}
}

// A version handed over and current, left by a crash, is neither taken nor
// set: the rotation seals a new one.
func TestRotationSkipsHandedOver(t *testing.T) {
	fx := newRotation(t)
	fx.seal(t, pw3, secrets.Meta{KeyVersion: 2, HandedOver: true}, nil)
	op := fx.op()
	fx.complete(t, op)
	if got := cursorVersion(t, op); got != 3 {
		t.Errorf("rotation set version %d, want a new 3", got)
	}
	if got := fx.kvno(t); got != 2 {
		t.Errorf("account's key version number %d, want 2: one set", got)
	}
	fx.signsIn(t, 3)
}

// The condition is cleared by a completed rotation, never by a channel set
// up with the previous version.
func TestRotationConditionClearedByCompletion(t *testing.T) {
	fx := newRotation(t)
	op := fx.op()
	fx.until(t, op, "confirm")
	fx.d.SetComputerPassword(t, "NAS", []byte(pw2))
	fx.wantFailed(t, op, "no longer accepts")
	fx.signsIn(t, 1)
	if !rotationFailed(fx.s) {
		t.Fatal("machine_password_rotation_failed cleared by a channel set up with the previous version")
	}
	fx.complete(t, fx.op())
	if rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed still raised after a completed rotation")
	}
}

// The prune runs at the first turn only once the current version is the
// confirmed one, the last rotation did not fail and the retention since the
// confirm has passed: a Prune failing with a marker shows where it ran. Where
// it runs, its error fails the rotation, recorded and raised, nothing sealed;
// elsewhere the turn moves on to pin.
func TestRotationPruneGate(t *testing.T) {
	const pruned = "PRUNE-CALLED"
	for _, tc := range []struct {
		name    string
		advance time.Duration
		setup   func(t *testing.T, fx *rotFixture)
		prunes  bool
	}{
		{name: "past the retention", advance: 11 * time.Hour, prunes: true},
		{name: "before the retention", advance: 10*time.Hour + 4*time.Minute},
		{name: "current version unconfirmed", advance: 11 * time.Hour, setup: func(t *testing.T, fx *rotFixture) {
			fx.seal(t, pw3, secrets.Meta{KeyVersion: 2}, nil)
		}},
		{name: "after a failed rotation", advance: 11 * time.Hour, setup: func(t *testing.T, fx *rotFixture) {
			// A rotation failed at the previous version's wait: nothing
			// sealed, so the current version is still the confirmed one.
			if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				in, err := tx.Installation()
				if err != nil {
					return err
				}
				j := in.Join
				j.LastRotation.At, j.LastRotation.Failed = tx.Now(), true
				return tx.SetMachineJoin(j)
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRotation(t)
			if tc.setup != nil {
				tc.setup(t, fx)
			}
			fx.restart(t, failingPrune{fx.p, errors.New(pruned)})
			fx.clock.Advance(tc.advance)
			before := fx.versions(t)
			op := fx.op()
			if !tc.prunes {
				fx.runs(t, op)
				if got := fx.phase(op); got != "pin" {
					t.Errorf("first turn: phase %q, want pin with nothing pruned", got)
				}

				if recs := fx.auditRecords(t); len(recs) != 0 {
					t.Errorf("audit records on the machine password: got %d, want none", len(recs))
				}

				return
			}
			fx.wantFailed(t, op, pruned)
			if vs := fx.versions(t); len(vs) != len(before) {
				t.Errorf("versions %v, want nothing sealed", vs)
			}
			if j := fx.join(t); !j.LastRotation.Failed || !atStore(j.LastRotation.At, fx.clock.Now()) {
				t.Errorf("last rotation %+v, want failed now", j.LastRotation)
			}
			if !rotationFailed(fx.s) {
				t.Error("machine_password_rotation_failed not raised by the prune's failure")
			}
		})
	}

	// A start with only the current version to keep reaches pin and
	// records nothing: the prune has no floor below the current version.
	t.Run("a start below a single version", func(t *testing.T) {
		fx := newRotation(t)
		fx.clock.Advance(11 * time.Hour)
		op := fx.op()
		fx.runs(t, op)

		if got := fx.phase(op); got != "pin" {
			t.Errorf("first turn: phase %q, want pin with nothing to prune", got)
		}

		if recs := fx.auditRecords(t); len(recs) != 0 {
			t.Errorf("audit records on the machine password: got %d, want none", len(recs))
		}
	})
}

// racingPrune is the real provider with race run once, right after its first
// Prune: a seal another write commits while the rotation's prune runs.
type racingPrune struct {
	*secrets.Provider
	race  func()
	raced bool
}

func (f *racingPrune) Prune(
	ctx context.Context,
	tx entity.ControlPlaneTx,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	err := f.Provider.Prune(ctx, tx, kind, owner, ref)
	if err != nil {
		return err
	}

	if !f.raced {
		f.raced = true
		f.race()
	}

	return nil
}

// The prune commits with the rotation's first write or not at all: a seal
// that commits while the prune runs makes that write run again, its prune
// dropped with it, and the rerun finds the current version unconfirmed and
// prunes nothing. A prune through a write of its own would commit and drop
// the confirmed version.
func TestRotationPruneDroppedWithItsWrite(t *testing.T) {
	fx := newRotation(t)
	fx.seal(t, pw3, secrets.Meta{KeyVersion: 2}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}

		j := in.Join
		j.LastRotation.At, j.LastRotation.Confirmed = tx.Now(), tx.Now()
		j.LastRotation.Tag, j.LastRotation.Read, j.LastRotation.Version = 2, 2, 2

		return tx.SetMachineJoin(j)
	})
	p := &racingPrune{
		Provider: fx.p,
		race: func() {
			fx.seal(t, pw4, secrets.Meta{KeyVersion: 3}, nil)
		},
	}
	fx.restart(t, p)
	fx.clock.Advance(11 * time.Hour)
	op := fx.op()
	fx.runs(t, op)

	if !p.raced {
		t.Fatal("the rotation's first turn never called Prune")
	}

	if got := fx.phase(op); got != "pin" {
		t.Errorf("first turn: phase %q, want pin", got)
	}

	var got []uint64
	for _, v := range fx.versions(t) {
		got = append(got, v.Number)
	}

	if !slices.Equal(got, []uint64{3, 2, 1}) {
		t.Errorf("versions %v, want [3 2 1]: the prune dropped with its write", got)
	}

	if rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed raised by a prune that reran")
	}

	if recs := fx.auditRecords(t); len(recs) != 0 {
		t.Errorf("audit records on the machine password: got %d, want none", len(recs))
	}
}

// A rotation started while a join runs on the installation is refused at its
// first turn, naming the join, with nothing written by the step.
func TestRotationRefusedDuringJoin(t *testing.T) {
	fx := newRotation(t)
	join := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpJoinDomain),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning}
	if err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		_, err := tx.PutOperation(join, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := fx.join(t)
	op := fx.op()
	fx.reconcile(t)
	// The step alone, without the runner's failed write.
	_, err := step.Run(context.Background(), fx.s.RotateStep, *op)
	p, ok := errors.AsType[*apiv1.Problem](err)
	if !ok || p.Code != apiv1.CodeOperationRunning || !strings.Contains(p.Detail, uuid.UUID(join.ID).String()[:8]) {
		t.Fatalf("first turn: err %v; want refused naming the join", err)
	}
	if vs := fx.versions(t); len(vs) != 1 {
		t.Errorf("versions %v, want nothing sealed", vs)
	}
	if after := fx.join(t); after.LastRotation != before.LastRotation {
		t.Errorf("last rotation %+v, want unchanged %+v", after.LastRotation, before.LastRotation)
	}
}

// erroringPrune is the real provider with Prune run and then failed as err,
// as a cut that stopped before its caller could record it.
type erroringPrune struct {
	*secrets.Provider
	err error
}

func (f erroringPrune) Prune(
	ctx context.Context,
	tx entity.ControlPlaneTx,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	_ = f.Provider.Prune(ctx, tx, kind, owner, ref)

	return f.err
}

// auditRefusal is a store fault that fails the Set of an audit key, matched
// on the audit's own key prefix so nothing else of the turn fails. It is the
// same fault the shared kvstore test suite (pkg/metadata/kvstore/kvstoretest)
// uses in its case for a prune in the caller's write,
// testPruneSecretInTheCallersWrite.
type auditRefusal struct{}

var errAuditRefused = errors.New("the audit record's write was refused")

func (f *auditRefusal) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpSet && strings.HasPrefix(string(key), "AU‖") {
		return 0, 0, errAuditRefused
	}

	return 0, 0, nil
}

// auditRecords are the audit records that name the machine password: the
// prune's own records.
func (fx *rotFixture) auditRecords(t *testing.T) []entity.AuditRecord {
	t.Helper()

	target := entity.ResourceRef{
		Kind: entity.ResourceSecret,
		ID:   fx.in.MachinePassword[:],
	}

	var recs []entity.AuditRecord

	for r, err := range fx.cp.AuditRecords(
		context.Background(), entity.AuditFilter{Target: &target}, nil) {
		if err != nil {
			t.Fatal(err)
		}

		recs = append(recs, r)
	}

	return recs
}

// versionNumbers are the machine password's versions by number.
func (fx *rotFixture) versionNumbers(t *testing.T) []uint64 {
	t.Helper()

	var got []uint64
	for _, v := range fx.versions(t) {
		got = append(got, v.Number)
	}

	return got
}

// confirm seals one more version of the machine password and records the
// join as confirmed on it, as a rotation that succeeded.
func (fx *rotFixture) confirm(t *testing.T, password string, keyVersion, rotation uint32) {
	t.Helper()

	m := secrets.Meta{KeyVersion: keyVersion}

	fx.seal(t, password, m, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}

		j := in.Join
		j.LastRotation.At, j.LastRotation.Confirmed = tx.Now(), tx.Now()
		j.LastRotation.Tag, j.LastRotation.Read = rotation, rotation
		j.LastRotation.Version = uint64(rotation)

		return tx.SetMachineJoin(j)
	})
}

// The prune's record commits with its write: one audit record names the
// machine password, the exact versions the prune dropped and nothing else,
// never a sealed value, and names the rotation that ran it and the account
// that started it.
func TestRotationPruneAuditsItsVersions(t *testing.T) {
	fx := newRotation(t)
	fx.confirm(t, pw3, 2, 2)
	fx.confirm(t, pw3, 3, 3)
	fx.clock.Advance(11 * time.Hour)
	op := fx.op()
	op.Account = entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID(uuid.New())}
	fx.runs(t, op)

	if got := fx.phase(op); got != "pin" {
		t.Errorf("first turn: phase %q, want pin", got)
	}

	if got := fx.versionNumbers(t); !slices.Equal(got, []uint64{3}) {
		t.Errorf("versions %v, want [3]: the prune dropped the versions below the current one", got)
	}

	recs := fx.auditRecords(t)
	if len(recs) != 1 {
		t.Fatalf("audit records on the machine password: got %d, want one", len(recs))
	}

	rec := recs[0]
	if want := `{"pruned":[1,2]}`; string(rec.Change) != want {
		t.Errorf("the prune's change %q, want %q", rec.Change, want)
	}

	if want := uuid.UUID(op.ID).String(); string(rec.Outcome) != want {
		t.Errorf("the prune's outcome %q, want the rotation's operation id %q", rec.Outcome, want)
	}

	want := entity.ResourceRef{Kind: entity.ResourceSecret, ID: fx.in.MachinePassword[:]}

	if rec.Target.Kind != want.Kind || string(rec.Target.ID) != string(want.ID) {
		t.Errorf("the prune's target kind %d, id %x, want the machine password secret",
			rec.Target.Kind, rec.Target.ID)
	}

	if rec.Actor != op.Account {
		t.Errorf("the prune's actor %+v, want the rotation's account %+v", rec.Actor, op.Account)
	}
}

// The prune commits with its record or not at all: a store that refuses
// the record's write rolls the prune back with it, leaving every version
// and no record.
func TestRotationPruneRecordFailsWithItsWrite(t *testing.T) {
	fx := newRotation(t)
	fx.confirm(t, pw3, 2, 2)
	fx.restart(t, fx.p)
	fx.fkv.SetFault(&auditRefusal{})
	fx.clock.Advance(11 * time.Hour)
	op := fx.op()
	_, err := step.Run(context.Background(), fx.s.RotateStep, *op)
	fx.fkv.SetFault(nil)

	if !errors.Is(err, errAuditRefused) {
		t.Fatalf("first turn: err %v, want the refused write of the audit record", err)
	}

	if rotationFailed(fx.s) {
		t.Error("machine_password_rotation_failed raised by the refused audit record")
	}

	if got := fx.versionNumbers(t); !slices.Equal(got, []uint64{2, 1}) {
		t.Errorf("versions %v, want [2 1]: the prune dropped with its record", got)
	}

	if recs := fx.auditRecords(t); len(recs) != 0 {
		t.Errorf("audit records on the machine password: got %d, want none", len(recs))
	}
}

// A prune that cut versions and then failed leaves no cut behind: the
// write rolls back, no record exists, and the rotation is recorded failed
// and fails the turn naming the prune.
func TestRotationPruneErrorRollsBack(t *testing.T) {
	fx := newRotation(t)
	fx.confirm(t, pw3, 2, 2)
	fx.restart(t, erroringPrune{Provider: fx.p, err: errors.New("cut and failed")})
	fx.clock.Advance(11 * time.Hour)
	op := fx.op()
	fx.wantFailed(t, op, "prune the machine password")

	if got := fx.versionNumbers(t); !slices.Equal(got, []uint64{2, 1}) {
		t.Errorf("versions %v, want [2 1]: the cut rolled back with its write", got)
	}

	if recs := fx.auditRecords(t); len(recs) != 0 {
		t.Errorf("audit records on the machine password: got %d, want none", len(recs))
	}

	if j := fx.join(t); !j.LastRotation.Failed || !atStore(j.LastRotation.At, fx.clock.Now()) {
		t.Errorf("last rotation %+v, want failed now", j.LastRotation)
	}
}

// The prune drops below the lowest version any node reports holding, and
// its record names the versions that went: a node below the current one
// keeps the versions it holds, and a node reporting version zero holds
// nothing, so nothing goes and no record exists.
func TestRotationPruneNamesVersionsBelowReports(t *testing.T) {
	for _, tc := range []struct {
		name     string
		holds    uint64
		kept     []uint64
		recorded bool
		change   string
		// pruned has version 1 already pruned before the rotation runs.
		pruned bool
	}{
		{
			name: "a node holds version 2", holds: 2, kept: []uint64{4, 3, 2},
			recorded: true, change: `{"pruned":[1]}`,
		},
		{
			name: "version 1 already pruned", holds: 4, pruned: true, kept: []uint64{4},
			recorded: true, change: `{"pruned":[2,3]}`,
		},
		{name: "a node holds version 0", holds: 0, kept: []uint64{4, 3, 2, 1}, recorded: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRotation(t)
			fx.confirm(t, pw3, 2, 2)
			fx.confirm(t, pw3, 3, 3)
			fx.confirm(t, pw4, 4, 4)

			if tc.pruned {
				if err := fx.cp.Write(context.Background(), entity.WriteMeta{},
					func(tx entity.ControlPlaneTx) error {
						return tx.PruneSecret(fx.in.MachinePassword, 2)
					}); err != nil {
					t.Fatal(err)
				}
			}

			node := entity.NodeID(uuid.New())
			if _, err := fx.store.Nodes().RegisterNode(context.Background(), entity.Node{
				ID:    node,
				Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage,
			}); err != nil {
				t.Fatal(err)
			}

			if err := fx.store.Nodes().PutNodeStatus(context.Background(), entity.NodeStatus{
				Node:    node,
				Secrets: []entity.SecretVersion{{Secret: fx.in.MachinePassword, Version: tc.holds}},
			}); err != nil {
				t.Fatal(err)
			}

			fx.restart(t, fx.p)
			fx.clock.Advance(11 * time.Hour)
			op := fx.op()
			fx.runs(t, op)

			if got := fx.phase(op); got != "pin" {
				t.Errorf("first turn: phase %q, want pin", got)
			}

			if got := fx.versionNumbers(t); !slices.Equal(got, tc.kept) {
				t.Errorf("versions %v, want %v", got, tc.kept)
			}

			recs := fx.auditRecords(t)
			if !tc.recorded {
				if len(recs) != 0 {
					t.Errorf("audit records on the machine password: got %d, want none", len(recs))
				}

				return
			}

			if len(recs) != 1 {
				t.Fatalf("audit records on the machine password: got %d, want one", len(recs))
			}

			if got := string(recs[0].Change); got != tc.change {
				t.Errorf("the prune's change %q, want %q", got, tc.change)
			}

			if want := uuid.UUID(op.ID).String(); string(recs[0].Outcome) != want {
				t.Errorf("the prune's outcome %q, want the rotation's operation id %q",
					recs[0].Outcome, want)
			}
		})
	}
}
