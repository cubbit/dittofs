package kvtest_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
)

// The suite through its own exported surface, against the embedded backend
// in memory: every case it holds runs and passes on a conforming store. The
// memtable is 8 MiB, as in badgerkv's own conformance runs: at Badger's
// default, filling the entry limit under -race takes most of the 5 s
// transaction age, and a loaded machine aborted it.
func TestRun(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV {
		s, err := badgerkv.Open(badgerkv.Options{MemTableSize: 8 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}
