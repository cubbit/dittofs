//go:build journal_conformance

package conformance

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestOpen_IdleSealBytesSeals checks that the idle-seal size a caller sets in
// Settings reaches the journal Open returns: a segment holding that many bytes
// and taking no append for the idle age is cut to its size, which the seam
// sees as a truncate of the segment file. The fixture sets the size to a
// sixteenth of its cap, so a seal here is one the setting caused.
func TestOpen_IdleSealBytesSeals(t *testing.T) {
	f := journaltest.New(t)
	h := f.Share(0)

	truncates := func() (n int) {
		for _, name := range segmentNames(t, f) {
			n += f.Seam.CallsFor(kit.OpTruncate, name)
		}
		return n
	}
	before := truncates()

	write(t, h, journaltest.File(0), 0, make([]byte, 2*journaltest.FixtureIdleSealBytes))
	f.Advance(2 * journaltest.IdleSealAge)

	eventually(t, "no segment was truncated once idle past the idle-seal size", func() bool {
		return truncates() > before
	})
}
