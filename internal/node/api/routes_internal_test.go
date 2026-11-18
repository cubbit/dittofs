package api

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api/auth"
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
