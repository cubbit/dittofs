package platform

import (
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestRefusalMatchesErrUnsuitableStorage pins every rule's answer to the
// caller's sentinel at the verdict's own level, which every platform builds
// and can name without making a system call: a refusal is the one error Init
// and Open answer with ErrUnsuitableStorage, while its message still names
// the rule, the directory and what to change, and no error that says the
// inputs did not describe the directory matches the sentinel, so a caller
// can tell a fault to fix from a refusal.
func TestRefusalMatchesErrUnsuitableStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, rule := range []refusalRule{
		ruleBarrier,
		ruleSubvolume,
		ruleCopyOnWrite,
		ruleCachePolicy,
	} {
		r := &refusal{rule: rule, dir: "/journal", mount: "/", path: "/journal"}
		if !errors.Is(r, core.ErrUnsuitableStorage) {
			t.Errorf("refusal %d does not match core.ErrUnsuitableStorage", rule)
		}

		if !strings.Contains(r.Error(), "/journal") {
			t.Errorf("refusal %d: got %q, want the message naming the directory", rule, r)
		}
	}

	for _, err := range []error{
		errDirectoryNotClean,
		errMountTableMalformed,
		errMountUnresolved,
		errMountIDDisagrees,
		errMountTypeMismatch,
	} {
		if errors.Is(err, core.ErrUnsuitableStorage) {
			t.Errorf("%v matches core.ErrUnsuitableStorage", err)
		}
	}
}
