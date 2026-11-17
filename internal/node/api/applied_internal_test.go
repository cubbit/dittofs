package api

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
