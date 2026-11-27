package engine

import "testing"

// A batch claims a file's writes and grows up to its first removal, whatever
// follows it: a truncate, a deallocate or a clone stops the claim, so no
// batch commits at or above a removal whose phase 1 has not, a guard held or
// not.
func TestABatchClaimsUpToTheFirstRemoval(t *testing.T) {
	write := pendingOp{kind: opWrite}
	grow := pendingOp{kind: opTruncate, grow: true}
	for _, c := range []struct {
		name string
		ops  []pendingOp
		want int
	}{
		{"nothing", nil, 0},
		{"writes and a grow", []pendingOp{write, grow, write}, 3},
		{"a truncate", []pendingOp{write, write, {kind: opTruncate}, write}, 2},
		{"a deallocate", []pendingOp{write, {kind: opDeallocate}, write}, 1},
		{"a clone", []pendingOp{write, {kind: opClone}, write}, 1},
		{"a removal first", []pendingOp{{kind: opTruncate}, write}, 0},
	} {
		if got := claimable(c.ops); got != c.want {
			t.Errorf("%s: a batch claims %d operations, want %d", c.name, got, c.want)
		}
	}
}
