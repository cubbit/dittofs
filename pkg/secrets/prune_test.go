package secrets

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// pruneFixture is a resolve fixture with the store opened beside it, so a
// test can register nodes and write their statuses.
type pruneFixture struct {
	*resolveFixture
	st *kvstore.Store
}

func newPruneFixture(t *testing.T) *pruneFixture {
	t.Helper()

	f := newResolveFixture(t)

	st, err := kvstore.Open(context.Background(), f.db)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}

	return &pruneFixture{
		resolveFixture: f,
		st:             st,
	}
}

// seed seals and commits versions 1 to n of a keytab owned by sealOwner,
// version v holding "v<v>".
func (f *pruneFixture) seed(t *testing.T, n int) entity.SecretRef {
	t.Helper()

	ref := sealRef(t)

	for v := 1; v <= n; v++ {
		resolvePut(t, f.p, f.cp, entity.SecretKeytab, sealOwner(), ref, "v"+strconv.Itoa(v), Meta{})
	}

	return ref
}

// node registers a node, as a node that has started is, and returns its ID.
func (f *pruneFixture) node(t *testing.T) entity.NodeID {
	t.Helper()

	id := entity.NodeID(uuid.New())

	if _, err := f.st.RaiseNodeEpoch(context.Background(), id); err != nil {
		t.Fatalf("register a node: %v", err)
	}

	return id
}

// orderedNodes registers n nodes and returns their IDs in node ID order.
func (f *pruneFixture) orderedNodes(t *testing.T, n int) []entity.NodeID {
	t.Helper()

	ids := make([]entity.NodeID, n)
	for i := range ids {
		ids[i] = f.node(t)
	}

	slices.SortFunc(ids, func(a, b entity.NodeID) int {
		return slices.Compare(a[:], b[:])
	})

	return ids
}

// report writes the status of node: the versions it holds of each reference.
func (f *pruneFixture) report(t *testing.T, node entity.NodeID, held ...entity.SecretVersion) {
	t.Helper()

	status := entity.NodeStatus{
		Node:    node,
		Secrets: held,
	}

	if err := f.st.Nodes().PutNodeStatus(context.Background(), status); err != nil {
		t.Fatalf("write a node status: %v", err)
	}
}

// prune runs Prune inside an audited Write, as its caller does.
func (f *pruneFixture) prune(
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	ctx := context.Background()

	meta := entity.WriteMeta{
		Audit: &entity.AuditRecord{
			Target: entity.ResourceRef{
				Kind: entity.ResourceSecret,
				ID:   ref[:],
			},
			Change: []byte("prune"),
		},
	}

	return f.cp.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		return f.p.Prune(ctx, tx, kind, owner, ref)
	})
}

// pruneKeytab prunes ref as its owner, with the kind and owner it was
// sealed under.
func (f *pruneFixture) pruneKeytab(ref entity.SecretRef) error {
	return f.prune(entity.SecretKeytab, sealOwner(), ref)
}

// held lists the versions among 1 to upto that still read.
func (f *pruneFixture) held(t *testing.T, ref entity.SecretRef, upto int) []int {
	t.Helper()

	var out []int

	for v := 1; v <= upto; v++ {
		_, err := f.cp.Secret(context.Background(), ref, uint64(v))
		if merrs.IsNotFoundError(err) {
			continue
		}

		if err != nil {
			t.Fatalf("read version %d: %v", v, err)
		}

		out = append(out, v)
	}

	return out
}

func (f *pruneFixture) wantHeld(t *testing.T, ref entity.SecretRef, upto int, want ...int) {
	t.Helper()

	if got := f.held(t, ref, upto); !slices.Equal(got, want) {
		t.Errorf("versions held: got %v, want %v", got, want)
	}
}

func holds(ref entity.SecretRef, version uint64) entity.SecretVersion {
	return entity.SecretVersion{
		Secret:  ref,
		Version: version,
	}
}

// A registered node still on version 2 of three keeps versions 2 and 3, and
// Versions lists exactly those.
func TestPruneKeepsWhatARegisteredNodeHolds(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.report(t, f.node(t), holds(ref, 2))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 2, 3)

	list, err := f.p.Versions(context.Background(), Protocol, entity.SecretKeytab, sealOwner(), ref)
	if err != nil || len(list) != 2 || list[0].Number != 3 || list[1].Number != 2 {
		t.Errorf("Versions after the prune: %+v, %v; want versions 3 and 2", list, err)
	}
}

// Three nodes on versions 3, 1 and 2, in both node ID orders: the lowest
// wins, so nothing is dropped.
func TestPruneFloorIsTheLowestOfAllNodes(t *testing.T) {
	for name, versions := range map[string][]uint64{
		"low in the middle, then descending": {3, 1, 2},
		"reversed":                           {2, 1, 3},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPruneFixture(t)
			ref := f.seed(t, 3)

			for i, id := range f.orderedNodes(t, 3) {
				f.report(t, id, holds(ref, versions[i]))
			}

			if err := f.pruneKeytab(ref); err != nil {
				t.Fatalf("Prune: %v", err)
			}

			f.wantHeld(t, ref, 3, 1, 2, 3)
		})
	}
}

// A status that lists only another reference, at a version lower than this
// one's current, holds nothing of this one back.
func TestPruneIgnoresOtherReferences(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.report(t, f.node(t), holds(sealRef(t), 1))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 3)
}

// No node reports the reference: every version but the current goes, and
// the current still resolves.
func TestPruneWithNoReportKeepsOnlyTheCurrent(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 3)

	got, err := f.p.Resolve(context.Background(), Protocol, entity.SecretKeytab, sealOwner(), ref)
	if err != nil || string(got) != "v3" {
		t.Errorf("Resolve after the prune: %q, %v; want %q", got, err, "v3")
	}
}

// A node that reports a version above the current cannot lift the floor
// above it: the current version stays.
func TestPruneNeverRemovesTheCurrent(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.report(t, f.node(t), holds(ref, 7))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 3)
}

// A wrong kind, or an owner differing in any one part, is refused and
// removes nothing; an absent reference is not found.
func TestPruneRefusesAnotherKindOrOwner(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)

	cases := map[string]struct {
		kind  entity.SecretKind
		owner entity.SecretOwner
		ref   entity.SecretRef
	}{
		"another kind": {entity.SecretNTHash, sealOwner(), ref},
	}

	for name, owner := range resolveOwnerVariants() {
		cases["another owner "+name] = struct {
			kind  entity.SecretKind
			owner entity.SecretOwner
			ref   entity.SecretRef
		}{entity.SecretKeytab, owner, ref}
	}

	for name, c := range cases {
		err := f.prune(c.kind, c.owner, c.ref)
		if !errors.Is(err, ErrWrongKind) {
			t.Errorf("%s: Prune = %v, want ErrWrongKind", name, err)
		}

		f.wantHeld(t, ref, 3, 1, 2, 3)
	}

	if err := f.pruneKeytab(sealRef(t)); !merrs.IsNotFoundError(err) {
		t.Errorf("an absent reference: Prune = %v, want not found", err)
	}
}

// An error in the middle of a prune leaves every version: the status read
// failing, the secret read failing, the delete failing, and the caller's
// commit failing.
func TestPruneErrorsLeaveEveryVersion(t *testing.T) {
	for name, fault := range map[string]faultOn{
		"status read":  {op: kvtest.OpScan, prefix: "N"},
		"secret read":  {op: kvtest.OpBefore, prefix: "SEC‖"},
		"delete":       {op: kvtest.OpDelete, prefix: "SEC‖"},
		"audit commit": {op: kvtest.OpSet, prefix: "AU‖"},
		"secret guard": {op: kvtest.OpGuardRange, prefix: "SEC‖"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPruneFixture(t)
			ref := f.seed(t, 3)

			f.db.SetFault(fault)

			err := f.pruneKeytab(ref)

			f.db.SetFault(nil)

			if !errors.Is(err, errSealInjected) {
				t.Errorf("Prune with a failing %s: %v, want the injected error", name, err)
			}

			f.wantHeld(t, ref, 3, 1, 2, 3)
		})
	}
}

// After a prune every version below the floor reads not found, those from
// the floor up read, and Versions lists them in one run down from the
// current.
func TestPruneLeavesOneRun(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 5)
	f.report(t, f.node(t), holds(ref, 3))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 5, 3, 4, 5)

	list, err := f.p.Versions(context.Background(), Protocol, entity.SecretKeytab, sealOwner(), ref)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}

	var numbers []uint64
	for _, v := range list {
		numbers = append(numbers, v.Number)
	}

	if !slices.Equal(numbers, []uint64{5, 4, 3}) {
		t.Errorf("Versions after the prune: %v, want [5 4 3]", numbers)
	}

	got, err := f.p.Resolve(context.Background(), Protocol, entity.SecretKeytab, sealOwner(), ref)
	if err != nil || string(got) != "v5" {
		t.Errorf("Resolve after the prune: %q, %v; want %q", got, err, "v5")
	}
}

// A prune raises no settings generation.
func TestPruneRaisesNoGeneration(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)

	before, err := f.st.Nodes().Counters(context.Background())
	if err != nil {
		t.Fatalf("counters: %v", err)
	}

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := f.st.Nodes().Counters(context.Background())
	if err != nil {
		t.Fatalf("counters: %v", err)
	}

	if after.Settings != before.Settings {
		t.Errorf("settings generation: got %d, want %d", after.Settings, before.Settings)
	}
}

// A node that reports version 0 of the reference holds everything back.
func TestPruneVersionZeroHoldsEverything(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.report(t, f.node(t), holds(ref, 0))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 1, 2, 3)
}

// The status of a node that has no node record, as one removed from the
// installation leaves, holds nothing back, even on version 1.
func TestPruneIgnoresANodeThatIsNotRegistered(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.report(t, entity.NodeID(uuid.New()), holds(ref, 1))

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 3)
}

// A registered node that has reported no status holds no old version of the
// reference: the prune goes ahead.
func TestPruneFreedByARegisteredNodeWithNoStatus(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	f.node(t)

	if err := f.pruneKeytab(ref); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	f.wantHeld(t, ref, 3, 3)
}

// A Write that fails after Prune leaves every version: the prune commits
// with the caller's Write or not at all.
func TestPruneIsInTheCallersWrite(t *testing.T) {
	f := newPruneFixture(t)
	ref := f.seed(t, 3)
	ctx := context.Background()

	err := f.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := f.p.Prune(ctx, tx, entity.SecretKeytab, sealOwner(), ref); err != nil {
			return err
		}

		return errSealInjected
	})
	if !errors.Is(err, errSealInjected) {
		t.Fatalf("Write: got %v, want the injected error", err)
	}

	f.wantHeld(t, ref, 3, 1, 2, 3)
}
