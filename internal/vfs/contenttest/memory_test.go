package contenttest_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The two declarations of Content hold the same methods.
var (
	_ vfs.Content         = contenttest.Content(nil)
	_ contenttest.Content = vfs.Content(nil)
)

// The suite holds over the in-memory Content.
func TestMemory(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		m := contenttest.New(clock)
		share, other := entity.NewShareID(), entity.NewShareID()
		m.Bind(share)
		m.Bind(other)
		return contenttest.Subject{
			Content: m,
			Share:   share,
			Other:   other,
			File:    func(*testing.T) entity.FileID { return entity.NewFileID() },
			Clock:   clock,
			Lose:    func(_ *testing.T, f entity.FileID) { m.Lose(share, f) },
		}
	})
}
