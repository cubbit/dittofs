package integration

import (
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The filesystem's Content suite holds over the served engine on the unit
// composition: the fake journal, the real store, the real open-state table
// holding clone sources, and the test clock, with a second share bound beside
// the first and a loss failing a file's sync window. Every group commit
// carries one file and a clone covers at most 16 MiB.
func TestContentSuite(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		u := enginetest.NewUnit(t, enginetest.UnitOptions{OpenState: true})
		if err := u.E.Apply(engine.Settings{ExistenceAge: time.Minute, GroupCommitMaxFiles: 1, CloneMaxLen: 16 << 20}); err != nil {
			t.Fatal(err)
		}
		var n atomic.Int64
		return contenttest.Subject{
			Content: u.E,
			Share:   u.Share.ID,
			Other:   u.BindShare(t, "other", u.Share.Namespace),
			Clock:   u.Clock,
			File: func(t *testing.T) entity.FileID {
				// Created, then its only name unlinked, so Delete may release it.
				name := fmt.Sprintf("f%d", n.Add(1))
				f := u.Create(t, name)
				if _, err := u.FS.Store.Namespace().Unlink(t.Context(), u.FS.Admin, u.FS.Root, []byte(name)); err != nil {
					t.Fatal(err)
				}
				return f
			},
			Lose: func(_ *testing.T, f entity.FileID) {
				u.J.FailSync(journal.NewFileID(f), journal.Extent{Off: 0, Len: math.MaxInt64})
			},
		}
	})
}
