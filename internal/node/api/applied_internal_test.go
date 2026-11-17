package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// A record is applied on a node that has reconciled its kind through its
// position and holds it neither refused nor restart-pending; a kind with no
// position is applied on every node that does not refuse it.
func TestApplied(t *testing.T) {
	ref := entity.ResourceRef{Kind: entity.ResourceSetting, ID: []byte("knob")}
	other := entity.ResourceRef{Kind: entity.ResourceSetting, ID: []byte("dial")}
	settings := func(st entity.NodeStatus) uint64 { return st.Settings }
	node := func(through uint64) entity.NodeStatus { return entity.NodeStatus{Settings: through} }
	refusing := func(r entity.ResourceRef) entity.NodeStatus {
		return entity.NodeStatus{Settings: 9, Refused: []entity.RefusedRecord{{Record: r}}}
	}
	pending := func(r entity.ResourceRef) entity.NodeStatus {
		return entity.NodeStatus{Settings: 9, RestartPending: []entity.ResourceRef{r}}
	}
	for _, tc := range []struct {
		name     string
		statuses []entity.NodeStatus
		at       uint64
		through  func(entity.NodeStatus) uint64
		want     apiv1.Applied
	}{
		{"no node", nil, 5, settings, apiv1.Applied{}},
		{"behind, at and past the position", []entity.NodeStatus{node(4), node(5), node(6)}, 5, settings, apiv1.Applied{Nodes: 2, Of: 3}},
		{"refused there", []entity.NodeStatus{node(9), refusing(ref)}, 5, settings, apiv1.Applied{Nodes: 1, Of: 2}},
		{"restart-pending there", []entity.NodeStatus{node(9), pending(ref)}, 5, settings, apiv1.Applied{Nodes: 1, Of: 2}},
		{"another record refused and pending", []entity.NodeStatus{refusing(other), pending(other)}, 5, settings, apiv1.Applied{Nodes: 2, Of: 2}},
		{"no position, a node behind", []entity.NodeStatus{node(0), node(9)}, 5, nil, apiv1.Applied{Nodes: 2, Of: 2}},
		{"no position, refused there", []entity.NodeStatus{node(0), refusing(ref)}, 0, nil, apiv1.Applied{Nodes: 1, Of: 2}},
	} {
		if got := (perPage{statuses: tc.statuses}).applied(ref, tc.at, tc.through); got != tc.want {
			t.Errorf("%s: applied %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Of counts the registered nodes, read from the store: a lapsed node, one
// registered whose status is behind, counts toward of and not as applied,
// and a status whose node has no record, as a removed node leaves, counts
// in neither.
func TestAppliedCountsRegisteredNodes(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{NodeID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the KV: %v", err)
		}
	})
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	current, lapsed, removed := entity.NodeID(uuid.New()), entity.NodeID(uuid.New()), entity.NodeID(uuid.New())
	for _, id := range []entity.NodeID{current, lapsed} {
		if _, err := st.RaiseNodeEpoch(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	for id, through := range map[entity.NodeID]uint64{current: 5, lapsed: 2, removed: 9} {
		if err := st.Nodes().PutNodeStatus(t.Context(), entity.NodeStatus{Node: id, Settings: through}); err != nil {
			t.Fatal(err)
		}
	}
	statuses, err := st.Nodes().RegisteredStatuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ref := entity.ResourceRef{Kind: entity.ResourceSetting, ID: []byte("knob")}
	got := perPage{statuses: statuses}.applied(ref, 5, func(st entity.NodeStatus) uint64 { return st.Settings })
	if want := (apiv1.Applied{Nodes: 1, Of: 2}); got != want {
		t.Errorf("applied with a current, a lapsed and a removed node: %+v, want %+v", got, want)
	}
}
