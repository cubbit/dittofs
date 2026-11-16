//go:build ad_dc

package ad_dc_test

import (
	"strings"
	"testing"
)

// TestAdContainerNamesAreDistinct guards the fixture-isolation invariant: each
// test must address its own container, so no test's teardown can remove the DC
// out from under another test's in-flight `docker exec`. A regression here would
// make uniqueContainerName return a fixed or shared name again, and dropping the
// run ID from it would let two runs on one Docker host collide.
func TestAdContainerNamesAreDistinct(t *testing.T) {
	t.Run("alpha", func(t *testing.T) {
		want := adNamePrefix + "-" + adRunID + "-testadcontainernamesaredistinct-alpha"
		if got := uniqueContainerName(t); got != want {
			t.Fatalf("container name = %q, want %q", got, want)
		}
	})
	t.Run("beta", func(t *testing.T) {
		got := uniqueContainerName(t)
		if strings.Contains(got, "alpha") {
			t.Fatalf("beta container name %q leaked the alpha name", got)
		}
	})
}
