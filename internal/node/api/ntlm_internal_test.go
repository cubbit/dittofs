package api

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// ntlmStore is a control plane over an in-memory Badger, its installation
// initialised.
func ntlmStore(t *testing.T) kvstore.ControlPlane {
	t.Helper()
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
	return st.ControlPlane()
}

// ntlmState is what the rule reads and writes, as the store holds it.
type ntlmState struct {
	off     uint64
	deleted entity.OperationID
}

// The identity.ntlm rule, as a function of a transaction over the store:
// each case sets NTLMOff and a running deletion through the store, runs the
// rule for one write, and reads back whether it refused, whether it starts a
// deletion, and what NTLMOff holds after.
func TestNTLMRule(t *testing.T) {
	older := entity.OperationID(uuid.New())
	for _, tc := range []struct {
		name     string
		before   ntlmState
		running  bool
		was, on  bool
		refused  bool
		start    bool
		after    ntlmState
		namesRun bool
	}{
		{name: "a turn-off from on sets Off and clears Deleted, and starts a deletion",
			before: ntlmState{off: 3, deleted: older}, was: true, on: false, start: true, after: ntlmState{off: 9}},
		{name: "a turn-off from on while a deletion runs starts none",
			before: ntlmState{off: 3, deleted: older}, running: true, was: true, on: false, after: ntlmState{off: 9}},
		{name: "a second turn-off while one runs changes neither and starts none",
			before: ntlmState{off: 3}, running: true, was: false, on: false, after: ntlmState{off: 3}},
		{name: "a turn-off while already off with none run since and none running finishes a failed one",
			before: ntlmState{off: 3}, was: false, on: false, start: true, after: ntlmState{off: 3}},
		{name: "a turn-off while already off once one has completed starts none",
			before: ntlmState{off: 3, deleted: older}, was: false, on: false, after: ntlmState{off: 3, deleted: older}},
		{name: "a turn-on refused while a fresh deletion runs after an older mark was cleared",
			before: ntlmState{off: 3}, running: true, was: false, on: true, refused: true, namesRun: true, after: ntlmState{off: 3}},
		{name: "a turn-on refused while the last deletion failed",
			before: ntlmState{off: 3}, was: false, on: true, refused: true, after: ntlmState{off: 3}},
		{name: "a turn-on accepted once a deletion has completed",
			before: ntlmState{off: 3, deleted: older}, was: false, on: true, after: ntlmState{off: 3, deleted: older}},
		{name: "a turn-on accepted when NTLM was never turned off",
			was: true, on: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := ntlmStore(t)
			var runID entity.OperationID
			if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				if tc.before.off != 0 {
					if err := tx.SetNTLMOff(tc.before.off); err != nil {
						return err
					}
				}
				if tc.before.deleted != (entity.OperationID{}) {
					if err := tx.SetNTHashDeleted(tc.before.deleted); err != nil {
						return err
					}
				}
				if !tc.running {
					return nil
				}
				in, err := tx.Installation()
				if err != nil {
					return err
				}
				runID = entity.OperationID(uuid.New())
				_, err = tx.PutOperation(entity.Operation{ID: runID, Kind: string(apiv1.OpNTHashDelete), Target: installationRef(in), State: entity.OpRunning}, 0)
				return err
			}); err != nil {
				t.Fatalf("set up: %v", err)
			}
			var start bool
			err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				var err error
				start, err = ntlmRule(tx, tc.was, tc.on, 9)
				return err
			})
			var running *entity.OperationRunningError
			p, _ := problemOf(err)
			switch {
			case tc.refused && (err == nil || p.Status != 409 || p.Code != apiv1.CodeOperationRunning):
				t.Fatalf("rule: %v, answered %d %q, want 409 operation_running", err, p.Status, p.Code)
			case !tc.refused && err != nil:
				t.Fatalf("rule: %v, want accepted", err)
			case tc.namesRun && (!errors.As(err, &running) || running.ID != runID):
				t.Errorf("refusal %v, want it naming the running deletion %v", err, runID)
			}
			if start != tc.start {
				t.Errorf("starts a deletion: got %v, want %v", start, tc.start)
			}
			var in entity.Installation
			if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				var err error
				in, err = tx.Installation()
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got := (ntlmState{in.NTLMOff.Off, in.NTLMOff.Deleted}); got != tc.after {
				t.Errorf("NTLMOff after: got %+v, want %+v", got, tc.after)
			}
		})
	}
}

// ntlmOn reads no record as the default, on, and only false as off.
func TestNTLMOn(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
		want  bool
	}{{"", false, true}, {"true", true, true}, {"false", true, false}, {" false ", true, false}} {
		if got := ntlmOn(entity.Setting{Value: []byte(tc.value)}, tc.ok); got != tc.want {
			t.Errorf("ntlmOn(%q, %v) = %v, want %v", tc.value, tc.ok, got, tc.want)
		}
	}
}

// ntlmWrite over the store, its write a put of identity.ntlm in the same
// transaction and its start the operation's real start: a turn-off from the
// default sets Off to the generation the put raised and starts the
// deletion; a second turn-off, with that one running, starts nothing; and a
// write leaving no record reads as on, refused while the deletion runs.
func TestNTLMWrite(t *testing.T) {
	cp := ntlmStore(t)
	scope := entity.SettingScope{Kind: entity.ScopeInstallation}
	run := func(write func(tx entity.ControlPlaneTx) error) (started []apiv1.OpKind, err error) {
		err = cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			started = nil
			return ntlmWrite(tx, func(k apiv1.OpKind, target entity.ResourceRef) error {
				started = append(started, k)
				_, err := start(t.Context(), tx, k, target, opStart{})
				return err
			}, scope, ntlmKey, func() error { return write(tx) })
		})
		return started, err
	}
	putOff := func(tx entity.ControlPlaneTx) error {
		old, _, err := tx.Setting(scope, ntlmKey)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(entity.Setting{Scope: scope, Key: ntlmKey, Value: []byte("false")}, old.Generation)
		return err
	}
	readBack := func() (entity.Installation, entity.Setting) {
		var in entity.Installation
		var s entity.Setting
		if err := cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
			if in, err = tx.Installation(); err != nil {
				return err
			}
			s, _, err = tx.Setting(scope, ntlmKey)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return in, s
	}

	started, err := run(putOff)
	if err != nil || !slices.Equal(started, []apiv1.OpKind{apiv1.OpNTHashDelete}) {
		t.Fatalf("turn-off from on: started %v (%v), want the NT-hash deletion", started, err)
	}
	in, s := readBack()
	if in.NTLMOff.Off == 0 || in.NTLMOff.Off != s.Position {
		t.Errorf("Off %d, want the position the put raised, %d", in.NTLMOff.Off, s.Position)
	}
	if started, err = run(putOff); err != nil || len(started) != 0 {
		t.Errorf("turn-off while off with the deletion running: started %v (%v), want none", started, err)
	}
	if again, _ := readBack(); again.NTLMOff != in.NTLMOff {
		t.Errorf("NTLMOff after the second turn-off: %+v, want %+v", again.NTLMOff, in.NTLMOff)
	}
	// A write leaving no record, as a delete does, reads as on: with Off set
	// and no deletion completed, it is refused. The store cannot delete a
	// setting yet, so the write here leaves a record that never existed.
	fresh := ntlmStore(t)
	if err := fresh.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetNTLMOff(4) }); err != nil {
		t.Fatal(err)
	}
	err = fresh.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return ntlmWrite(tx, func(apiv1.OpKind, entity.ResourceRef) error { return nil }, scope, ntlmKey, func() error { return nil })
	})
	if p, _ := problemOf(err); p.Code != apiv1.CodeOperationRunning {
		t.Errorf("a write leaving no record with NTLM off and no deletion completed: %v, want 409 operation_running", err)
	}
}
