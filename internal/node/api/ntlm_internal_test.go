package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/api/auth"
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

// ntlmOn reads no record as the default, on, and decodes a record as a JSON
// bool, as identity does: only false is off, and a value that is no bool is
// on.
func TestNTLMOn(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
		want  bool
	}{{"", false, true}, {"true", true, true}, {"false", true, false}, {" false ", true, false},
		{`"false"`, true, true}, {"0", true, true}, {"false false", true, true}} {
		if got := ntlmOn(entity.Setting{Value: []byte(tc.value)}, tc.ok); got != tc.want {
			t.Errorf("ntlmOn(%q, %v) = %v, want %v", tc.value, tc.ok, got, tc.want)
		}
	}
}

// ntlmWrite over the store, its write a put of identity.ntlm in the same
// transaction and its start the operation's real start: a turn-off from the
// default sets Off to the settings generation the put raised, not the
// record's own, and starts the deletion; a second turn-off, with that one
// running, starts nothing; and a write leaving no record reads as on,
// refused while the deletion runs. A false at another scope, or of another
// key, runs no rule.
func TestNTLMWrite(t *testing.T) {
	cp := ntlmStore(t)
	scope := entity.SettingScope{Kind: entity.ScopeInstallation}
	run := func(cp kvstore.ControlPlane, scope entity.SettingScope, key string, write func(tx entity.ControlPlaneTx) error) error {
		return cp.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return ntlmWrite(asAdmin(t), tx, func(k apiv1.OpKind, target entity.ResourceRef) error {
				_, err := start(t.Context(), tx, k, target, opStart{})
				return err
			}, scope, key, func() error { return write(tx) })
		})
	}
	put := func(scope entity.SettingScope, key, value string) func(tx entity.ControlPlaneTx) error {
		return func(tx entity.ControlPlaneTx) error {
			old, _, err := tx.Setting(scope, key)
			if err != nil {
				return err
			}
			_, err = tx.PutSetting(entity.Setting{Scope: scope, Key: key, Value: []byte(value)}, old.Generation)
			return err
		}
	}
	putOff := put(scope, ntlmKey, "false")
	readBack := func(cp kvstore.ControlPlane) (entity.Installation, entity.Setting) {
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
	operations := func(cp kvstore.ControlPlane) []entity.Operation {
		var out []entity.Operation
		for op, err := range cp.Operations(t.Context(), entity.OperationID{}) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, op)
		}
		return out
	}

	t.Run("the rule's scope and key", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			scope entity.SettingScope
			key   string
		}{
			{"a node's identity.ntlm", entity.SettingScope{Kind: entity.ScopeNode, ID: uuid.New()}, ntlmKey},
			{"another installation key", scope, "identity.netbios_name"},
		} {
			cp := ntlmStore(t)
			if err := run(cp, tc.scope, tc.key, put(tc.scope, tc.key, "false")); err != nil {
				t.Fatalf("%s set false: %v", tc.name, err)
			}
			if ops := operations(cp); len(ops) != 0 {
				t.Errorf("%s set false: operations %+v, want none", tc.name, ops)
			}
			if in, _ := readBack(cp); in.NTLMOff.Off != 0 {
				t.Errorf("%s set false: Off %d, want zero", tc.name, in.NTLMOff.Off)
			}
		}
	})

	// Another setting first, so the settings generation and the record's
	// own generation differ.
	if err := cp.Write(t.Context(), entity.WriteMeta{}, put(scope, "identity.netbios_name", `"NAS"`)); err != nil {
		t.Fatal(err)
	}
	if err := run(cp, scope, ntlmKey, putOff); err != nil {
		t.Fatalf("turn-off from on: %v", err)
	}
	ops := operations(cp)
	if len(ops) != 1 || ops[0].Kind != string(apiv1.OpNTHashDelete) {
		t.Fatalf("turn-off from on: operations %+v, want the NT-hash deletion", ops)
	}
	in, s := readBack(cp)
	if in.NTLMOff.Off == 0 || in.NTLMOff.Off != s.Position || s.Position == s.Generation {
		t.Errorf("Off %d, want the settings generation the put raised, %d, not the record's %d", in.NTLMOff.Off, s.Position, s.Generation)
	}
	if err := run(cp, scope, ntlmKey, putOff); err != nil {
		t.Errorf("turn-off while off with the deletion running: %v", err)
	}
	if ops := operations(cp); len(ops) != 1 {
		t.Errorf("turn-off while off with the deletion running: operations %+v, want the first alone", ops)
	}
	if again, _ := readBack(cp); again.NTLMOff != in.NTLMOff {
		t.Errorf("NTLMOff after the second turn-off: %+v, want %+v", again.NTLMOff, in.NTLMOff)
	}
	// A write leaving no record, as a delete does, reads as on: with Off set
	// and no deletion completed, it is refused. The store cannot delete a
	// setting yet, so the write here leaves a record that never existed.
	fresh := ntlmStore(t)
	if err := fresh.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.SetNTLMOff(4) }); err != nil {
		t.Fatal(err)
	}
	err := fresh.Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return ntlmWrite(asAdmin(t), tx, func(apiv1.OpKind, entity.ResourceRef) error { return nil }, scope, ntlmKey, func() error { return nil })
	})
	if p, _ := problemOf(err); p.Code != apiv1.CodeOperationRunning {
		t.Errorf("a write leaving no record with NTLM off and no deletion completed: %v, want 409 operation_running", err)
	}
}

// asAdmin is the test's context carrying an admin caller, who holds the
// installation right identity.ntlm's writes take.
func asAdmin(t *testing.T) context.Context {
	return auth.With(t.Context(), auth.Caller{Role: entity.RoleAdmin})
}
