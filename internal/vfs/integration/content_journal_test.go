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
// the engine fixture, with its second share bound beside the first.
//
// decision: Lose is left nil, so the loss case skips. The real journal fails
// a whole sync window, every file pending in it, and the case asserts that
// another file's loss sequence stays put; the run over the unit composition
// covers it over the fake journal, which loses one file. Supply it once the
// fixture can fail one file's window.
func TestContentSuiteOverTheJournal(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		f := enginetest.New(t)
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
		}
	})
}
