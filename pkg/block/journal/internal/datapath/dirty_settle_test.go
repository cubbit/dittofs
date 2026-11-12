package datapath

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

func TestDirtyWriteAfterSettleEmptiesHeldKeepsItsTime(t *testing.T) {
	c, clk := dirtyClockCore(t)
	s := mustShare(t, c, 1)
	f := testFile(3)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	v, err := c.Truncate(context.Background(), s, f, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Settle(s, f, v)
	clk.Advance(time.Minute)
	t1 := clk.Now()
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	wantListed(t, c, s, "write after truncate+settle", map[core.FileID]time.Time{f: t1})
}
