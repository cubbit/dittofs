//go:build ad_dc

package ad_dc_test

import (
	"slices"
	"strings"
	"testing"
	"time"
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

// TestStaleADContainers checks that the leftover sweep picks only containers
// older than its age limit, so it never removes a concurrent run's live DC,
// only those whose name starts with the prefix, since Docker's name filter
// also matches it mid-name, and keeps a line whose time it cannot read.
func TestStaleADContainers(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ps := "old\tp-run1-test\t2026-11-16 12:30:00 +0200 CEST\n" + // 90 minutes ago
		"young\tp-run2-test\t2026-11-16 11:30:00 +0000 UTC\n" +
		"midname\tother-p-test\t2026-11-16 09:00:00 +0000 UTC\n" +
		"garbled\tp-run3-test\tyesterday\n"
	got := staleADContainers(ps, "p-", now, time.Hour)
	if want := []string{"old"}; !slices.Equal(got, want) {
		t.Fatalf("staleADContainers = %q, want %q", got, want)
	}
	if got := staleADContainers("", "p-", now, time.Hour); len(got) != 0 {
		t.Fatalf("staleADContainers of empty output = %q, want none", got)
	}
}
