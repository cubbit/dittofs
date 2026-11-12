package engine_test

import (
	"fmt"
	"math"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The filesystem's Content suite holds over the served engine on the unit
// composition: the fake journal, the real store and the test clock, with a
// second share bound beside the first and a loss failing a file's sync
// window. Its clones hold their sources in the real open-state table, as
// clonedUnit composes it with no fault set.
func TestContentSuite(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		u := clonedUnit(t)
		var n atomic.Int64
		return contenttest.Subject{
			Content: u.e,
			Share:   u.share.ID,
			Other:   u.bindShare(t, "other", u.share.Namespace),
			Clock:   u.clock,
			File: func(t *testing.T) entity.FileID {
				// Created, then its only name unlinked, so Delete may release it.
				name := fmt.Sprintf("f%d", n.Add(1))
				f := u.create(t, name)
				if _, err := u.fs.Store.Namespace().Unlink(t.Context(), u.fs.Admin, u.fs.Root, []byte(name)); err != nil {
					t.Fatal(err)
				}
				return f
			},
			Lose: func(_ *testing.T, f entity.FileID) {
				u.j.FailSync(journal.NewFileID(f), journal.Extent{Off: 0, Len: math.MaxInt64})
			},
		}
	})
}
