package memory_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/remote/memory"
	"github.com/cubbit/dittofs/pkg/remote/remotetest"
	"github.com/cubbit/dittofs/pkg/sim"
)

// epoch is where every test's clock starts.
var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// The in-memory store passes the conformance suite in each of its checksum
// modes, so the rows run once against each kind a put can carry.
func TestConformance(t *testing.T) {
	for _, mode := range []struct {
		name string
		sum  remote.Checksum
	}{
		{"None", remote.ChecksumNone},
		{"CRC32C", remote.ChecksumCRC32C},
		{"MD5", remote.ChecksumMD5},
	} {
		t.Run(mode.name, func(t *testing.T) {
			remotetest.Run(t, func(t *testing.T) remotetest.Subject {
				s := memory.New(sim.NewTestClock(epoch).Now, mode.sum)
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Errorf("Close: %v", err)
					}
				})

				return remotetest.Subject{Store: s}
			})
		})
	}
}
