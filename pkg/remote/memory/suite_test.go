package memory_test

import (
	"context"
	"sync"
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

				return subject(s)
			})
		})
	}
}

// subject is the suite's view of s: its store wrapped, so the namespace can
// turn read-only, and its hooks backed by the store's raw access. Reopen's
// store is wrapped too, and ReadOnly sets every wrapper over the namespace,
// as a namespace's permissions bind every client of it. Load is the store's
// record, which counts the calls of every store over the namespace.
func subject(s *memory.Store) remotetest.Subject {
	var (
		mu       sync.Mutex
		readOnly bool
		wrappers []*memory.Wrapper
	)

	wrap := func(s *memory.Store) *memory.Wrapper {
		w := memory.Wrap(s)

		mu.Lock()
		defer mu.Unlock()

		w.SetReadOnly(readOnly)
		wrappers = append(wrappers, w)

		return w
	}

	return remotetest.Subject{
		Store: wrap(s),
		PutRaw: func(_ context.Context, key string, body []byte) error {
			memory.PutRaw(s, key, body)
			return nil
		},
		Reopen: func(context.Context) (remote.Store, error) { return wrap(memory.Reopen(s)), nil },
		Raw:    func(context.Context) ([]string, error) { return memory.RawKeys(s), nil },
		ReadOnly: func(_ context.Context, on bool) error {
			mu.Lock()
			defer mu.Unlock()

			readOnly = on
			for _, w := range wrappers {
				w.SetReadOnly(on)
			}

			return nil
		},
		Load: func() remotetest.Load { return suiteLoad(s.Load()) },
	}
}

// suiteLoad is l as the suite declares it, the skipped-key count left out.
func suiteLoad(l memory.Load) remotetest.Load {
	calls := make([]remotetest.CallRecord, len(l.Calls))
	for i, c := range l.Calls {
		calls[i] = remotetest.CallRecord(c)
	}

	return remotetest.Load{InFlight: l.InFlight, Peak: l.Peak, Calls: calls, Puts: l.Puts, Bytes: l.Bytes}
}
