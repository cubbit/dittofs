package journaltest

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// TestSuitableMountIsOverridable pins that a caller's own probe option,
// passed through the options NewWithOptions takes, replaces the suitable
// mount the fixture sets by default: a journal whose check reads a mount
// without cache flushes, fed to the real verdict as an input, is refused
// as unsuitable storage, so a test of the refusal still reaches the real
// rules. The fixture's journals init and open over the default suitable
// mount in every other test of this package, which is the other half
// checked.
func TestSuitableMountIsOverridable(t *testing.T) {
	f := bareFixture(t, nil, FixtureSegmentCap, nil, journal.WithMountProbe(func(dir string) (storage.Probe, error) {
		return platformtest.NoBarrierMount(dir), nil
	}))
	if _, err := f.TryInit(); !errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Fatalf("a journal whose check read a mount without cache flushes inited over it: %v, want ErrUnsuitableStorage", err)
	}
}
