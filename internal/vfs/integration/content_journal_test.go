//go:build engine_conformance

package integration

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The filesystem's Content suite holds over the engine on the real journal:
// the engine fixture, with its second share bound beside the first, and a
// loss failing the sync window of the file's own Commit.
func TestContentSuiteOverTheJournal(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		// Eight concurrent writers each hold a content spare while they run, so the
		// pool keeps one per writer, plus the fixture's own two: a pool of two refuses
		// the excess retry-later whenever its refill lags.
		f := enginetest.New(t, enginetest.WithSpares(8+2))
		if err := f.E.Bind(t.Context(), f.Second); err != nil {
			t.Fatal(err)
		}
		var n atomic.Int64
		return contenttest.Subject{
			Content: f.E,
			Share:   f.Share.ID,
			Other:   f.Second.ID,
			Clock:   f.Clock,
			File: func(t *testing.T) entity.FileID {
				// Created, then its only name unlinked, so Delete may release it.
				name := fmt.Sprintf("f%d", n.Add(1))
				_, id := f.File(name)
				if _, err := f.FS.Store.Namespace().Unlink(t.Context(), f.FS.Admin, f.FS.Root, []byte(name)); err != nil {
					t.Fatal(err)
				}
				return id
			},
			Lose: func(t *testing.T, id entity.FileID) {
				f.FailNextWindow()
				// The Commit fails the window holding id's pending writes, so the
				// loss is in place when Lose returns; it may answer that failure.
				_, _, _ = f.E.Commit(t.Context(), f.Share.ID, id, 0, 0)
			},
		}
	})
}
