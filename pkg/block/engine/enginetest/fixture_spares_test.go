package enginetest

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
)

// The first write of a fixture over the real journal is never refused for want
// of a spare segment: the fixture hands the journal over once its pool has
// made its first spares.
func TestTheFirstWriteOfAFixtureIsNeverRefusedForSpares(t *testing.T) {
	for i := range 20 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := New(t)
			_, id := f.File("a")

			if _, err := f.E.Write(t.Context(), f.Share.ID, id, 0, make([]byte, 4096), engine.WriteOpts{Done: func() {}}); err != nil {
				t.Fatalf("the first write after the fixture opened: %v", err)
			}
		})
	}
}
