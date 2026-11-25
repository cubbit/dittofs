package audit

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// An actor with no name prints as --actor spells it: kind:id, or the kind
// alone when it has no ID.
func TestTableActor(t *testing.T) {
	for _, tt := range []struct {
		actor apiv1.Principal
		want  string
	}{
		{apiv1.Principal{Kind: apiv1.ScheduleKind}, "schedule"},
		{apiv1.Principal{Kind: "user", ID: "u1"}, "user:u1"},
	} {
		if got := table(apiv1.AuditRecord{Actor: tt.actor})[1][1]; got != tt.want {
			t.Errorf("actor %+v: got %q, want %q", tt.actor, got, tt.want)
		}
	}
}
