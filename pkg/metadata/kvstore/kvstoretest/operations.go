package kvstoretest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

func operation(kind string, target entity.ResourceRef) entity.Operation {
	return entity.Operation{ID: entity.OperationID(uuid.New()), Kind: kind, Target: target, State: entity.OpRunning}
}

func shareRef(id string) entity.ResourceRef {
	return entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte(id)}
}

// One operation of a kind runs on a target: a second is refused while the
// first runs, beside it one of another kind or on another target runs, and
// once the first ends the second starts.
func testOneOperationPerTargetAndKind(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	first, second := operation("share.delete", shareRef("a")), operation("share.delete", shareRef("a"))
	gen, err := cp.PutOperation(ctx, first, 0)
	must(t, err)
	if got, ok, err := cp.RunningOperation(ctx, "share.delete", shareRef("a")); err != nil || !ok || got.ID != first.ID {
		t.Fatalf("running share.delete on a: got %v, %v, %v; want %v", got.ID, ok, err, first.ID)
	}
	_, err = cp.PutOperation(ctx, second, 0)
	var running *entity.OperationRunningError
	if !errors.Is(err, entity.ErrOperationRunning) || !errors.As(err, &running) || running.ID != first.ID {
		t.Errorf("second share.delete on a while the first runs: %v, want %v naming %v", err, entity.ErrOperationRunning, first.ID)
	}
	for _, beside := range []entity.Operation{operation("share.rehome", shareRef("a")), operation("share.delete", shareRef("b"))} {
		if _, err := cp.PutOperation(ctx, beside, 0); err != nil {
			t.Errorf("%s on %s beside share.delete on a: %v, want started", beside.Kind, beside.Target.ID, err)
		}
	}
	first.State = entity.OpSucceeded
	must(t, errOf(cp.PutOperation(ctx, first, gen)))
	if _, ok, err := cp.RunningOperation(ctx, "share.delete", shareRef("a")); err != nil || ok {
		t.Errorf("running share.delete on a after it ended: found %v, %v; want none", ok, err)
	}
	if _, err := cp.PutOperation(ctx, second, 0); err != nil {
		t.Errorf("second share.delete on a after the first ended: %v, want started", err)
	}
}

// An operation reads back as written, with the store's times and
// generation, and the listing pages through every operation in ID order. An
// ended operation, and a change of kind or target, are refused.
func testOperationReads(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	op := operation("user.delete", entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{1}})
	op.Account, op.Step, op.Params = entity.NewUser(), []byte("step"), []byte("params")
	op.Claim = entity.OpClaim{Node: entity.NodeID{1}, Epoch: 3}
	gen, err := cp.PutOperation(ctx, op, 0)
	must(t, err)
	got, ok, err := cp.Operation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("operation just written: %v, %v", ok, err)
	}
	if got.Started.IsZero() || !got.Updated.Equal(got.Started) || got.Generation != gen {
		t.Errorf("store-set fields: started %v, updated %v, generation %d; want one store time and %d", got.Started, got.Updated, got.Generation, gen)
	}
	op.Started, op.Updated, op.Generation = got.Started, got.Updated, gen
	if !reflect.DeepEqual(got, op) {
		t.Errorf("operation read back:\n got %+v\nwant %+v", got, op)
	}

	for name, change := range map[string]func(entity.Operation) entity.Operation{
		"kind":   func(o entity.Operation) entity.Operation { o.Kind = "group.delete"; return o },
		"target": func(o entity.Operation) entity.Operation { o.Target.ID = []byte{2}; return o },
	} {
		if _, err := cp.PutOperation(ctx, change(op), gen); !invalid(err) {
			t.Errorf("operation's %s changed: %v, want invalid argument", name, err)
		}
	}
	op.State = entity.OpFailed
	gen, err = cp.PutOperation(ctx, op, gen)
	must(t, err)
	op.Error = []byte("again")
	if _, err := cp.PutOperation(ctx, op, gen); !invalid(err) {
		t.Errorf("ended operation changed: %v, want invalid argument", err)
	}

	ids := map[entity.OperationID]bool{op.ID: true}
	for range 3 {
		o := operation("share.delete", shareRef(uuid.NewString()))
		must(t, errOf(cp.PutOperation(ctx, o, 0)))
		ids[o.ID] = true
	}
	listed := all(t, cp.Operations(ctx, entity.OperationID{}))
	if len(listed) != len(ids) {
		t.Fatalf("operations listed: got %d, want %d", len(listed), len(ids))
	}
	for i, o := range listed {
		if !ids[o.ID] || i > 0 && string(listed[i-1].ID[:]) >= string(o.ID[:]) {
			t.Errorf("operation %d listed: %v, want each written one once in ID order", i, o.ID)
		}
	}
	if rest := all(t, cp.Operations(ctx, listed[0].ID)); len(rest) != len(ids)-1 || rest[0].ID != listed[1].ID {
		t.Errorf("operations after the first: got %d starting %v, want %d starting %v", len(rest), rest[0].ID, len(ids)-1, listed[1].ID)
	}
}

// An operation's state change commits with its audit record or not at all:
// with the audit record's write failed, the operation still runs.
func testOperationStateWithAudit(t *testing.T, kvo Opener) {
	f := kvtest.NewFaultKV(kvo(t))
	must(t, kvstore.Init(ctx, f, uuid.New()))
	s, err := kvstore.Open(ctx, f)
	must(t, err)
	cp := s.ControlPlane()
	op := operation("share.delete", shareRef("a"))
	gen, err := cp.PutOperation(ctx, op, 0)
	must(t, err)
	end := func() error {
		meta := entity.WriteMeta{Audit: &entity.AuditRecord{Target: entity.ResourceRef{Kind: entity.ResourceOperation, ID: op.ID[:]}}}
		return cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
			done := op
			done.State = entity.OpSucceeded
			return errOf(tx.PutOperation(done, gen))
		})
	}
	failed := errors.New("audit write failed")
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "AU‖*", Action: sim.FaultAction{Err: failed}})
	must(t, err)
	f.SetFault(sched)
	if err := end(); !errors.Is(err, failed) {
		t.Fatalf("end whose audit record failed: %v, want %v", err, failed)
	}
	if got, _, err := cp.Operation(ctx, op.ID); err != nil || got.State != entity.OpRunning {
		t.Errorf("operation after an end whose audit record failed: %v, %v; want running", got.State, err)
	}
	f.SetFault(nil)
	must(t, end())
	if got, _, err := cp.Operation(ctx, op.ID); err != nil || got.State != entity.OpSucceeded {
		t.Errorf("operation after its end committed: %v, %v; want succeeded", got.State, err)
	}
	target := entity.ResourceRef{Kind: entity.ResourceOperation, ID: op.ID[:]}
	if got := audits(t, cp, entity.AuditFilter{Target: &target}); len(got) != 1 {
		t.Errorf("audit records of the operation: got %d, want 1", len(got))
	}
}

// A write's read of the running index is tracked: a start of that kind on
// that target committed after the read makes the write retry, and its
// committing attempt finds the operation running.
func testRunningOperationTracked(t *testing.T, kvo Opener) {
	for _, b := range kvo.backends() {
		t.Run(b.name, func(t *testing.T) {
			s, h := heldStore(t, b)
			cp := s.ControlPlane()
			op := operation("share.delete", shareRef("a"))
			var found []bool
			outer, inner := race(h,
				func() error {
					return cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
						_, ok, err := tx.RunningOperation(op.Kind, op.Target)
						if err != nil {
							return err
						}
						found = append(found, ok)
						return errOf(tx.PutSetting(entity.Setting{Scope: instScope, Key: "k"}, 0))
					})
				},
				func() error { return errOf(cp.PutOperation(ctx, op, 0)) })
			must(t, inner)
			must(t, outer)
			if len(found) == 0 || !found[len(found)-1] {
				t.Errorf("running share.delete on a, read by each attempt: %v; want the committing attempt to find it", found)
			}
		})
	}
}

// The running listing gives every operation not yet ended, once each, and
// resumes after any it gave.
func testRunningOperations(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	running := map[entity.OperationID]bool{}
	for _, target := range []string{"a", "b", "c"} {
		op := operation("share.delete", shareRef(target))
		gen, err := cp.PutOperation(ctx, op, 0)
		must(t, err)
		if target == "b" {
			op.State = entity.OpSucceeded
			must(t, errOf(cp.PutOperation(ctx, op, gen)))
			continue
		}
		running[op.ID] = true
	}
	var listed []entity.Operation
	for op, err := range cp.RunningOperations(ctx, entity.Operation{}) {
		if err != nil {
			t.Fatalf("running operations: %v", err)
		}
		listed = append(listed, op)
	}
	if len(listed) != len(running) || !running[listed[0].ID] || !running[listed[1].ID] || listed[0].ID == listed[1].ID {
		t.Fatalf("running operations: got %+v, want each of %v once", listed, running)
	}
	if rest := all(t, cp.RunningOperations(ctx, listed[0])); len(rest) != 1 || rest[0].ID != listed[1].ID {
		t.Errorf("running operations after %v: got %+v, want %v alone", listed[0].ID, rest, listed[1].ID)
	}
}
