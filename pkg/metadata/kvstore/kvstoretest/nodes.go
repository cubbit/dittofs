package kvstoretest

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

const bothRoles = entity.NodeRoleProtocol | entity.NodeRoleStorage

// Each start raises the node's epoch and registers the node, in either
// order: the record holds the registration with the raised epoch, one per
// start, whatever the registration carries, and registering returns the
// epoch as stored then, this start's only once it is raised.
func testNodeEpochAcrossStarts(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	for _, c := range []struct {
		name          string
		registerFirst bool
	}{{"raise then register", false}, {"register then raise", true}} {
		id := entity.NodeID(uuid.New())
		for start := uint64(1); start <= 2; start++ {
			reg := entity.Node{ID: id, Address: "10.0.0.1:12049", Roles: bothRoles, Binary: "v1", Domain: "a", Epoch: 99,
				Formats: map[entity.Format]entity.VersionRange{1: {Min: 1, Max: 2}, 2: {Min: 3, Max: 3}}}
			var epoch uint64
			var rec entity.Node
			var err error
			wantRec := start
			if c.registerFirst {
				rec, err = nodes.RegisterNode(ctx, reg)
				must(t, err)
				epoch, err = s.RaiseNodeEpoch(ctx, id)
				must(t, err)
				wantRec = start - 1
			} else {
				epoch, err = s.RaiseNodeEpoch(ctx, id)
				must(t, err)
				rec, err = nodes.RegisterNode(ctx, reg)
				must(t, err)
			}
			got, ok, err := nodes.Node(ctx, id)
			if err != nil || !ok {
				t.Fatalf("%s, start %d: node record: %v, %v", c.name, start, ok, err)
			}
			if epoch != start || rec.Epoch != wantRec || got.Epoch != start {
				t.Errorf("%s, start %d: raised %d, registered %d, read %d; want %d, %d, %d",
					c.name, start, epoch, rec.Epoch, got.Epoch, start, wantRec, start)
			}
			reg.Epoch = start
			if !reflect.DeepEqual(got, reg) {
				t.Errorf("%s, start %d: node record read %+v; want %+v", c.name, start, got, reg)
			}
			reg.Epoch = wantRec
			if !reflect.DeepEqual(rec, reg) {
				t.Errorf("%s, start %d: node record registered %+v; want %+v", c.name, start, rec, reg)
			}
		}
	}
	if _, ok, err := nodes.Node(ctx, entity.NodeID(uuid.New())); err != nil || ok {
		t.Errorf("node never started: found %v, %v; want none", ok, err)
	}
}

// A node write that names no node is refused and writes nothing.
func testZeroNodeRefused(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	for name, write := range map[string]func() error{
		"raise epoch": func() error { return errOf(s.RaiseNodeEpoch(ctx, entity.NodeID{})) },
		"register":    func() error { return errOf(nodes.RegisterNode(ctx, entity.Node{Roles: bothRoles})) },
		"status":      func() error { return nodes.PutNodeStatus(ctx, entity.NodeStatus{Settings: 1}) },
	} {
		if err := write(); !invalid(err) {
			t.Errorf("%s with the zero node ID: %v, want invalid argument", name, err)
		}
	}
	if _, ok, err := nodes.Node(ctx, entity.NodeID{}); err != nil || ok {
		t.Errorf("zero node's record: found %v, %v; want none written", ok, err)
	}
	for st, err := range nodes.NodeStatuses(ctx, entity.NodeID{}) {
		t.Errorf("node status listed after refused writes: %+v, %v; want none", st, err)
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
			{Record: entity.SettingRef(instScope, "b"), Generation: 5, Error: []byte("bad")},
			{Record: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("a")}, Generation: 6, Error: []byte("worse")},
		},
		RestartPending: []entity.ResourceRef{{Kind: entity.ResourceAdapter, ID: []byte("smb")}, entity.SettingRef(entity.SettingScope{Kind: entity.ScopeShare, ID: uuid.New()}, "a")},
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
	list := func(after entity.NodeID) (got []entity.NodeStatus) {
		for st, err := range nodes.NodeStatuses(ctx, after) {
			if err != nil {
				t.Fatalf("node statuses after %v: %v", after, err)
			}
			got = append(got, st)
		}
		return got
	}
	if got := list(entity.NodeID{}); !reflect.DeepEqual(got, want) {
		t.Errorf("node statuses:\n got %+v\nwant %+v", got, want)
	}
	if got := list(ids[0]); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("node statuses after the first node:\n got %+v\nwant %+v", got, want[1:])
	}
}

// The registered nodes' statuses are one per node record, in node ID
// order: a node that has reported none reads as the zero status naming it,
// and a status whose node has no record is left out.
func testRegisteredStatuses(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	ids := []entity.NodeID{entity.NodeID(uuid.New()), entity.NodeID(uuid.New()), entity.NodeID(uuid.New())}
	slices.SortFunc(ids, func(a, b entity.NodeID) int { return slices.Compare(a[:], b[:]) })
	reporting, silent, removed := ids[0], ids[1], ids[2]
	for _, id := range []entity.NodeID{reporting, silent} {
		_, err := s.RaiseNodeEpoch(ctx, id)
		must(t, err)
	}
	for _, id := range []entity.NodeID{reporting, removed} {
		must(t, nodes.PutNodeStatus(ctx, entity.NodeStatus{Node: id, Settings: 7, Conditions: []byte("ready")}))
	}
	got, err := nodes.RegisteredStatuses(ctx)
	must(t, err)
	want := []entity.NodeStatus{{Node: reporting, Settings: 7, Conditions: []byte("ready")}, {Node: silent}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registered statuses:\n got %+v\nwant %+v", got, want)
	}
}

// Node records list in node ID order, each once, beside the statuses and
// epochs that share their prefix, resumable after any node.
func testNodeRecords(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	nodes := s.Nodes()
	ids := []entity.NodeID{entity.NodeID(uuid.New()), entity.NodeID(uuid.New())}
	slices.SortFunc(ids, func(a, b entity.NodeID) int { return slices.Compare(a[:], b[:]) })
	var want []entity.Node
	for i, id := range ids {
		_, err := s.RaiseNodeEpoch(ctx, id)
		must(t, err)
		rec, err := nodes.RegisterNode(ctx, entity.Node{ID: id, Address: fmt.Sprintf("10.0.0.%d:12049", i), Roles: bothRoles, Binary: fmt.Sprintf("v0.%d.0", i)})
		must(t, err)
		must(t, nodes.PutNodeStatus(ctx, entity.NodeStatus{Node: id, Settings: 1}))
		want = append(want, rec)
	}
	must(t, nodes.PutNodeStatus(ctx, entity.NodeStatus{Node: entity.NodeID(uuid.New())})) // a status with no record
	list := func(after entity.NodeID) (got []entity.Node) {
		for n, err := range nodes.NodeRecords(ctx, after) {
			if err != nil {
				t.Fatalf("node records after %v: %v", after, err)
			}
			got = append(got, n)
		}
		return got
	}
	if got := list(entity.NodeID{}); !reflect.DeepEqual(got, want) {
		t.Errorf("node records:\n got %+v\nwant %+v", got, want)
	}
	if got := list(ids[0]); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("node records after the first node:\n got %+v\nwant %+v", got, want[1:])
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
	if bytes.Compare(a.ID[:], b.ID[:]) > 0 {
		a, b = b, a
	}
	shares := all(t, nodes.List(ctx, entity.ResourceShare, nil))
	if len(shares) != 2 || shares[0].Share.ID != a.ID || shares[1].Share.ID != b.ID {
		t.Fatalf("shares listed: got %+v, want %v then %v, in share ID order", shares, a.ID, b.ID)
	}
	if rest := all(t, nodes.List(ctx, entity.ResourceShare, shares[0].After)); len(rest) != 1 || rest[0].Share.ID != b.ID {
		t.Errorf("shares listed after %v: got %+v, want %v alone", a.ID, rest, b.ID)
	}
	must(t, cp.DeleteShare(ctx, a.ID, entity.SnapshotsRefuse, 1))
	if rest := all(t, nodes.List(ctx, entity.ResourceShare, nil)); len(rest) != 1 || rest[0].Share.ID != b.ID {
		t.Errorf("shares listed with %v being removed: got %+v, want %v alone", a.ID, rest, b.ID)
	}
	for _, err := range nodes.List(ctx, entity.ResourceUser, nil) {
		if !invalid(err) {
			t.Errorf("users listed by kind: %v, want invalid argument", err)
		}
	}
}

// A share renamed while a node pages through the shares, from a name that
// sorts after the first page to one that sorts before it, is still listed
// exactly once, as is every other share.
func testListSharesAcrossRename(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	const n = 300 // more than one page
	gens := map[entity.ShareID]uint64{}
	var last entity.Share
	for i := range n {
		last = share(fmt.Sprintf("s%03d", i), fmt.Sprintf("/s%03d", i))
		gen, err := CreateShare(ctx, cp, last)
		must(t, err)
		gens[last.ID] = gen
	}
	seen := map[entity.ShareID]int{}
	for rec, err := range s.Nodes().List(ctx, entity.ResourceShare, nil) {
		if err != nil {
			t.Fatalf("shares listed: %v", err)
		}
		if len(seen) == 0 {
			renamed := last
			renamed.Name = "a"
			must(t, errOf(cp.PutShare(ctx, renamed, gens[last.ID])))
		}
		seen[rec.Share.ID]++
	}
	if seen[last.ID] != 1 {
		t.Errorf("share renamed mid-listing listed %d times, want once", seen[last.ID])
	}
	for id := range gens {
		if seen[id] != 1 {
			t.Errorf("share %v listed %d times, want once", id, seen[id])
		}
	}
	if len(seen) != n {
		t.Errorf("shares listed: got %d, want %d", len(seen), n)
	}
}

// seedSettings writes n settings at installation scope and n at a node's
// scope, keys k000 upwards, and returns the node's scope and every key in
// the order a listing of every scope yields them.
func seedSettings(
	t *testing.T,
	cp kvstore.ControlPlane,
	n int,
) (entity.SettingScope, []entity.SettingKey) {
	t.Helper()

	nodeScope := entity.SettingScope{Kind: entity.ScopeNode, ID: uuid.New()}

	var want []entity.SettingKey

	for _, scope := range []entity.SettingScope{instScope, nodeScope} {
		for i := range n {
			k := entity.SettingKey{Scope: scope, Key: fmt.Sprintf("k%03d", i)}
			putSetting(t, cp, k)
			want = append(want, k)
		}
	}

	return nodeScope, want
}

// putSetting writes a new setting under k, its value settingValue's.
func putSetting(t *testing.T, cp kvstore.ControlPlane, k entity.SettingKey) {
	t.Helper()

	s := entity.Setting{Scope: k.Scope, Key: k.Key, Value: settingValue(k)}
	must(t, errOf(cp.PutSetting(ctx, s, 0)))
}

// sameKeys fails the test, naming what, unless got and want hold the same
// keys in the same order.
func sameKeys(t *testing.T, what string, got, want []entity.SettingKey) {
	t.Helper()

	if slices.Equal(got, want) {
		return
	}

	i := 0
	for i < min(len(got), len(want)) && got[i] == want[i] {
		i++
	}

	t.Errorf("%s: got %d keys, want %d, first difference at %d: got %v, want %v",
		what, len(got), len(want), i, got[i:min(i+1, len(got))], want[i:min(i+1, len(want))])
}

// settingValue is the value seedSettings writes under k.
func settingValue(k entity.SettingKey) []byte {
	return fmt.Appendf(nil, "%v/%s", k.Scope.Kind, k.Key)
}

// settingKeys lists every scope's settings after after, failing on any
// error or on a value other than the one seedSettings writes, and returns
// their keys.
func settingKeys(t *testing.T, nodes kvstore.Nodes, after entity.SettingKey) []entity.SettingKey {
	t.Helper()

	var got []entity.SettingKey

	for s, err := range nodes.AllSettings(ctx, after) {
		must(t, err)

		k := entity.SettingKey{Scope: s.Scope, Key: s.Key}
		if !bytes.Equal(s.Value, settingValue(k)) {
			t.Errorf("setting %v: value %q, want %q", k, s.Value, settingValue(k))
		}

		got = append(got, k)
	}

	return got
}

// Every scope's settings are listed once, in key order, over more than one
// page, the zero key starting from the first.
func testAllSettingsPages(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	_, want := seedSettings(t, s.ControlPlane(), 150)

	got := settingKeys(t, s.Nodes(), entity.SettingKey{})
	sameKeys(t, "every setting from the zero key", got, want)
}

// Each page is its own read: a write landing in a page already read is not
// listed, and one landing in a later page is.
func testAllSettingsWriteBetweenPages(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cp := s.ControlPlane()
	nodeScope, want := seedSettings(t, cp, 150)
	early := entity.SettingKey{Scope: instScope, Key: "k000a"} // in the first page
	late := entity.SettingKey{Scope: nodeScope, Key: "k200"}   // past the first page

	var got []entity.SettingKey

	for st, err := range s.Nodes().AllSettings(ctx, entity.SettingKey{}) {
		must(t, err)

		if len(got) == 0 {
			putSetting(t, cp, early)
			putSetting(t, cp, late)
		}

		got = append(got, entity.SettingKey{Scope: st.Scope, Key: st.Key})
	}

	sameKeys(t, "settings with a write between pages", got, append(want, late))
}

// A listing resumed after any key it yielded, a page's last among them,
// yields exactly the keys after it.
func testAllSettingsResumes(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	_, keys := seedSettings(t, s.ControlPlane(), 150)

	for _, i := range []int{0, 255, 299} {
		what := fmt.Sprintf("settings after key %d", i)
		sameKeys(t, what, settingKeys(t, s.Nodes(), keys[i]), keys[i+1:])
	}
}
