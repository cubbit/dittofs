package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/health"
)

// The stored conditions are the same bytes whatever order their sources
// listed them in, and whatever cause and detail each carries, so a
// condition whose figures change while it stands writes no new status.
func TestEncodeConditionsCanonical(t *testing.T) {
	full := health.Condition{Name: "journal_full", Subject: health.Subject{Scope: health.ScopeJournal, ID: "j1", Name: "/dev/a"},
		Cause: errors.New("refused"), Detail: "used 10 of 20 bytes"}
	behind := health.Condition{Name: "config_generation_behind", Subject: health.Subject{Scope: health.ScopeNode, ID: "n1", Name: "settings"}}
	moved := full
	moved.Cause, moved.Detail = errors.New("refused again"), "used 19 of 20 bytes"
	want := encodeConditions([]health.Condition{full, behind})
	for _, tc := range []struct {
		name  string
		conds []health.Condition
	}{
		{"listed in the other order", []health.Condition{behind, full}},
		{"journal_full's figures changed", []health.Condition{moved, behind}},
	} {
		if got := encodeConditions(tc.conds); !bytes.Equal(got, want) {
			t.Errorf("conditions %s: got %s, want %s", tc.name, got, want)
		}
	}
	var stored []condition
	if err := json.Unmarshal(want, &stored); err != nil {
		t.Fatalf("stored conditions %s: %v", want, err)
	}
	if len(stored) != 2 || stored[0] != (condition{Name: "config_generation_behind", Scope: "node", ID: "n1", Subject: "settings"}) {
		t.Errorf("stored conditions: got %+v, want config_generation_behind of node n1's settings first, by name", stored)
	}
}

// At most maxPerName conditions of one name are stored, those sorting
// first, beside every condition of another name.
func TestEncodeConditionsCapsEachName(t *testing.T) {
	var conds []health.Condition
	for i := range maxPerName + 5 {
		conds = append(conds, health.Condition{Name: "share_unhealthy", Subject: health.Subject{Scope: health.ScopeShare, ID: fmt.Sprintf("s%03d", i)}})
	}
	conds = append(conds, health.Condition{Name: "journal_full", Subject: health.Subject{Scope: health.ScopeJournal, ID: "j1"}})
	var stored []condition
	if err := json.Unmarshal(encodeConditions(conds), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != maxPerName+1 || stored[0].Name != "journal_full" || stored[maxPerName].ID != fmt.Sprintf("s%03d", maxPerName-1) {
		t.Errorf("stored conditions of %d share_unhealthy and one journal_full: got %d, last %+v; want journal_full and the first %d shares",
			maxPerName+5, len(stored), stored[len(stored)-1], maxPerName)
	}
}
