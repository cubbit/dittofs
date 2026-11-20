package api

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Every row a caller must authenticate for names the kind of resource it
// reaches, which its refusal is audited against; the store refuses an audit
// record of no kind, so a row without one would leave its refusals
// unaudited.
func TestRowsNameTheirKind(t *testing.T) {
	for _, rt := range append(slices.Clone(routes), testRows...) {
		if rt.right != auth.Public && rt.kind == 0 {
			t.Errorf("row %s: kind zero, want the kind of resource it reaches", rt.op())
		}
	}
}

// Every kind a row starts, or may start, is started and cancelled under that
// row's right, so a second row starting a kind under another right shows here
// instead of being overwritten in the map. The one exception is the NT-hash
// deletion: only the installation settings PUT may start it, through
// identity.ntlm's write, which asks ntlmRight for that key alone, so its
// right is ntlmRight, not the row's.
func TestStartRightsAreTheirRowsRights(t *testing.T) {
	rights := startRights(routes)
	for _, rt := range routes {
		for _, k := range []apiv1.OpKind{rt.starts, rt.mayStart} {
			switch {
			case k == "":
			case k == apiv1.OpNTHashDelete:
				if rt.op() != "PUT /api/v1/settings/{sub}" {
					t.Errorf("row %s may start %s, want only PUT /api/v1/settings/{sub}, whose identity.ntlm write asks its own right", rt.op(), k)
				}
			case rights[k] != rt.right:
				t.Errorf("row %s starts %s under %v, but startRights gives %v: two rows start it under different rights", rt.op(), k, rt.right, rights[k])
			}
		}
	}
	if got := rights[apiv1.OpNTHashDelete]; got != ntlmRight {
		t.Errorf("startRights for %s: got %v, want ntlmRight %v, identity.ntlm's write's", apiv1.OpNTHashDelete, got, ntlmRight)
	}
}
