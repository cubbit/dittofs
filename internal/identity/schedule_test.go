package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// startSchedule starts fx's server, and with it the rotation's schedule, and
// returns once its first check ran.
func (fx *rotFixture) startSchedule(t *testing.T) {
	t.Helper()
	checked := make(chan struct{}, 1000)
	fx.s.OnScheduled(func() { checked <- struct{}{} })
	fx.checked = checked
	if err := fx.s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx.awaitCheck(t)
}

// awaitCheck waits for the schedule's next check to end.
func (fx *rotFixture) awaitCheck(t *testing.T) {
	t.Helper()
	select {
	case <-fx.checked:
	case <-time.After(10 * time.Second): // a backstop: the check waits on nothing but the store
		t.Fatal("the schedule ran no check")
	}
}

// tickTo moves the clock to at, a store time at least one schedule tick
// on, and waits for the check it wakes. A store time can stand a few
// nanoseconds past the clock's, so the times the schedule reads are
// reached from the store's, not from the clock's.
func (fx *rotFixture) tickTo(t *testing.T, at time.Time) {
	t.Helper()
	fx.clock.Advance(at.Sub(fx.clock.Now()))
	fx.awaitCheck(t)
}

// rotations are the rotate-machine-password operations in the store.
func (fx *rotFixture) rotations(t *testing.T) []entity.Operation {
	t.Helper()
	var ops []entity.Operation
	for op, err := range fx.cp.Operations(context.Background(), entity.OperationID{}) {
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind == string(apiv1.OpRotateMachinePassword) {
			ops = append(ops, op)
		}
	}
	return ops
}

// wantRotations fails t unless the store holds n rotations.
func (fx *rotFixture) wantRotations(t *testing.T, n int, when string) []entity.Operation {
	t.Helper()
	ops := fx.rotations(t)
	if len(ops) != n {
		t.Fatalf("%s: got %d rotations, want %d", when, len(ops), n)
	}
	return ops
}

// failRotation ends op failed and records the last rotation failed at the
// store's time, as a rotation that fails does.
func (fx *rotFixture) failRotation(t *testing.T, op entity.Operation) {
	t.Helper()
	err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		op.State = entity.OpFailed
		if _, err := tx.PutOperation(op, op.Generation); err != nil {
			return err
		}
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		j := in.Join
		j.LastRotation.At, j.LastRotation.Failed = tx.Now(), true
		return tx.SetMachineJoin(j)
	})
	if err != nil {
		t.Fatal(err)
	}
}

const week = 7 * 24 * time.Hour

// The schedule creates a rotation, started by no call, once the last confirm
// is 7 days old, with an audit record the schedule's actor finds, and no
// second one while it runs.
func TestScheduleRotatesAfterPeriod(t *testing.T) {
	fx := newRotation(t)
	confirmed := fx.join(t).LastRotation.Confirmed
	fx.startSchedule(t)
	fx.tickTo(t, confirmed.Add(week-time.Hour))
	fx.wantRotations(t, 0, "6 days 23 hours after the confirm")
	fx.tickTo(t, confirmed.Add(week))
	op := fx.wantRotations(t, 1, "7 days after the confirm")[0]
	installation := entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}
	if !op.Account.IsZero() || op.State != entity.OpPending || op.Target.Kind != installation.Kind || string(op.Target.ID) != string(installation.ID) {
		t.Errorf("the scheduled rotation: got account %v, state %v, target %v; want the zero account, pending, on the installation", op.Account, op.State, op.Target)
	}
	schedule := entity.Principal{}
	var recs []entity.AuditRecord
	for r, err := range fx.cp.AuditRecords(context.Background(), entity.AuditFilter{Actor: &schedule}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if want := uuid.UUID(op.ID).String(); len(recs) != 1 || string(recs[0].Outcome) != want || string(recs[0].Target.ID) != string(installation.ID) {
		t.Errorf("the schedule's audit records: got %+v, want one on the installation with outcome %s", recs, want)
	}
	fx.tickTo(t, confirmed.Add(week+time.Hour))
	fx.wantRotations(t, 1, "an hour on, the rotation still running")
}

// The schedule creates no rotation on an installation not joined, nor while
// a join runs on it.
func TestScheduleHolds(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, fx *rotFixture)
	}{
		{"not joined", func(t *testing.T, fx *rotFixture) {
			err := fx.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				return tx.SetMachineJoin(entity.MachineJoin{})
			})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"a join running", func(t *testing.T, fx *rotFixture) {
			join := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpJoinDomain),
				Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: fx.in.ID[:]}, State: entity.OpRunning}
			if _, err := fx.cp.PutOperation(context.Background(), join, 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := newRotation(t)
			c.setup(t, fx)
			fx.startSchedule(t)
			fx.tickTo(t, fx.clock.Now().Add(8*24*time.Hour))
			fx.wantRotations(t, 0, "8 days after the confirm")
		})
	}
}

// Rotations failing in a row are each retried an hour after the last
// failure, the period read from the confirm, so the password never ages
// toward the domain's 30-day maximum.
func TestScheduleRetriesFailures(t *testing.T) {
	fx := newRotation(t)
	confirmed := fx.join(t).LastRotation.Confirmed
	fx.startSchedule(t)
	fx.tickTo(t, confirmed.Add(week))
	for i := 1; i <= 3; i++ {
		for _, op := range fx.wantRotations(t, i, "at the rotation due") {
			if !op.State.Ended() {
				fx.failRotation(t, op)
			}
		}
		at := fx.join(t).LastRotation.At
		fx.tickTo(t, at.Add(59*time.Minute))
		fx.wantRotations(t, i, "59 minutes after a failure")
		fx.tickTo(t, at.Add(time.Hour))
		fx.wantRotations(t, i+1, "an hour after a failure")
		if age := fx.clock.Now().Sub(confirmed); age >= 30*24*time.Hour {
			t.Fatalf("retry %d: the password is %v old, want under 30 days", i, age)
		}
	}
}

// A server built half an hour after a failure waits out the hour since it,
// read from the store, rather than retrying at its first check.
func TestScheduleRetryAfterRestart(t *testing.T) {
	fx := newRotation(t)
	fx.startSchedule(t)
	fx.tickTo(t, fx.join(t).LastRotation.Confirmed.Add(week))
	fx.failRotation(t, fx.wantRotations(t, 1, "7 days after the confirm")[0])
	at := fx.join(t).LastRotation.At
	fx.clock.Advance(30 * time.Minute)
	fx.restart(t, fx.p)
	fx.startSchedule(t)
	fx.wantRotations(t, 1, "a fresh server's first check, half an hour after the failure")
	fx.tickTo(t, at.Add(59*time.Minute))
	fx.wantRotations(t, 1, "59 minutes after the failure")
	fx.tickTo(t, at.Add(time.Hour))
	fx.wantRotations(t, 2, "an hour after the failure")
}
