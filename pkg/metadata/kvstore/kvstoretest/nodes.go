package kvstoretest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

const bothRoles = entity.NodeRoleProtocol | entity.NodeRoleStorage

// Each start raises the node's epoch and then registers it: the epoch the
// raise answered is the one the record holds, one per start, whatever the
// registration carries.
func testNodeEpochAcrossStarts(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	id := entity.NodeID(uuid.New())
	for start := uint64(1); start <= 2; start++ {
		epoch, err := s.RaiseNodeEpoch(ctx, id)
		must(t, err)
		reg := entity.Node{ID: id, Address: "10.0.0.1:12049", Roles: bothRoles, Binary: "v1", Domain: "a", Epoch: 99,
			Formats: map[entity.Format]entity.VersionRange{1: {Min: 1, Max: 2}, 2: {Min: 3, Max: 3}}}
		rec, err := nodes.RegisterNode(ctx, reg)
		must(t, err)
		got, ok, err := nodes.Node(ctx, id)
		if err != nil || !ok {
			t.Fatalf("start %d: node record: %v, %v", start, ok, err)
		}
		if epoch != start || rec.Epoch != epoch || got.Epoch != epoch {
			t.Errorf("start %d: raised %d, registered %d, read %d; want %d each", start, epoch, rec.Epoch, got.Epoch, start)
		}
		reg.Epoch = start
		if !reflect.DeepEqual(got, reg) || !reflect.DeepEqual(rec, reg) {
			t.Errorf("start %d: node record read %+v, registered %+v; want %+v", start, got, rec, reg)
		}
	}
	if _, ok, err := nodes.Node(ctx, entity.NodeID(uuid.New())); err != nil || ok {
		t.Errorf("node never started: found %v, %v; want none", ok, err)
	}
}

// A node serves both roles, and a version range never runs backwards.
func testRegisterNodeRefuses(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	for name, n := range map[string]entity.Node{
		"protocol role only": {Roles: entity.NodeRoleProtocol},
		"no role":            {},
		"range backwards":    {Roles: bothRoles, Formats: map[entity.Format]entity.VersionRange{1: {Min: 3, Max: 2}}},
	} {
		n.ID = entity.NodeID(uuid.New())
		if _, err := s.Nodes().RegisterNode(ctx, n); !invalid(err) {
			t.Errorf("%s: %v, want invalid argument", name, err)
		}
		if _, ok, err := s.Nodes().Node(ctx, n.ID); err != nil || ok {
			t.Errorf("%s: node record found %v, %v; want none written", name, ok, err)
		}
	}
}

// A node's status reads back whole, its lists in order, and the listing
// gives each node's once, in node ID order, beside the nodes' own records.
func testNodeStatuses(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	ids := []entity.NodeID{entity.NodeID(uuid.New()), entity.NodeID(uuid.New())}
	slices.SortFunc(ids, func(a, b entity.NodeID) int { return slices.Compare(a[:], b[:]) })
	want := []entity.NodeStatus{{
		Node: ids[0], Settings: 1, ShareList: 2, Principals: 3, Claims: 4,
		Refused: []entity.RefusedRecord{
			{Record: entity.ResourceRef{Kind: entity.ResourceSetting, ID: []byte("b")}, Generation: 5, Error: []byte("bad")},
			{Record: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("a")}, Generation: 6, Error: []byte("worse")},
		},
		RestartPending: []entity.ResourceRef{{Kind: entity.ResourceAdapter, ID: []byte("smb")}, {Kind: entity.ResourceSetting, ID: []byte("a")}},
		Secrets:        []entity.SecretVersion{{Secret: entity.SecretRef{2}, Version: 7}, {Secret: entity.SecretRef{1}, Version: 8}},
		Conditions:     []byte("ready"),
	}, {Node: ids[1], Settings: 9}}
	for _, id := range ids {
		_, err := s.RaiseNodeEpoch(ctx, id)
		must(t, err)
	}
	for _, st := range slices.Backward(want) {
		must(t, nodes.PutNodeStatus(ctx, st))
	}
	if got := all(t, nodes.NodeStatuses(ctx, entity.NodeID{})); !reflect.DeepEqual(got, want) {
		t.Errorf("node statuses:\n got %+v\nwant %+v", got, want)
	}
	if got := all(t, nodes.NodeStatuses(ctx, ids[0])); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("node statuses after the first node:\n got %+v\nwant %+v", got, want[1:])
	}
}

// A node lists every setting, at every scope, and every share, each
// resumable after any record it read; no other kind is listed.
func testListReconciledRecords(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp, nodes := s.ControlPlane(), s.Nodes()
	a, b := share("a", "/a"), share("b", "/b")
	must(t, errOf(CreateShare(ctx, cp, a)))
	must(t, errOf(CreateShare(ctx, cp, b)))
	nodeScope := entity.SettingScope{Kind: entity.ScopeNode, ID: uuid.New()}
	must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: instScope, Key: "k", Value: []byte("1")}, 0)))
	must(t, errOf(cp.PutSetting(ctx, entity.Setting{Scope: nodeScope, Key: "k", Value: []byte("2")}, 0)))

	settings := all(t, nodes.List(ctx, entity.ResourceSetting, nil))
	if len(settings) != 2 || settings[0].Setting.Scope != instScope || settings[1].Setting.Scope != nodeScope ||
		string(settings[1].Setting.Value) != "2" {
		t.Errorf("settings listed: got %+v, want the installation's then the node's", settings)
	}
	shares := all(t, nodes.List(ctx, entity.ResourceShare, nil))
	if len(shares) != 2 || shares[0].Share.ID != a.ID || shares[1].Share.ID != b.ID {
		t.Fatalf("shares listed: got %+v, want a then b", shares)
	}
	if rest := all(t, nodes.List(ctx, entity.ResourceShare, shares[0].After)); len(rest) != 1 || rest[0].Share.ID != b.ID {
		t.Errorf("shares listed after a: got %+v, want b alone", rest)
	}
	for _, err := range nodes.List(ctx, entity.ResourceUser, nil) {
		if !invalid(err) {
			t.Errorf("users listed by kind: %v, want invalid argument", err)
		}
	}
}
