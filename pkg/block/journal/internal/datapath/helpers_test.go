package datapath

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

const (
	testSegmentCap = int64(64 << 10)
	// testStreams is the number of append streams a test core has.
	testStreams = 2
)

var testIdentity = core.JournalID{0xd, 0xa, 0x7, 0xa}

// newTestCore builds a Core over a real directory through the seam, with two
// streams and small segments, and closes it when the test ends. opts adjust
// the unexported options before the layers are built.
func newTestCore(t *testing.T, opts ...func(*coreOptions)) (*Core, *journaltest.Seam) {
	t.Helper()
	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}

	var o coreOptions
	for _, f := range opts {
		f(&o)
	}
	c, err := newCore(Deps{
		FS:          seam.FS(),
		Clock:       journaltest.NewStopwatchClock(),
		Dir:         t.TempDir(),
		Identity:    testIdentity,
		Streams:     testStreams,
		SegmentCap:  testSegmentCap,
		ExtentLimit: 1 << 20,
		Descriptors: core.NewDescriptorBudget(64),
		Targets:     segment.Targets{Content: 2 * testStreams},
	}, o.state, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		err := c.Close()
		// Only a seam that was crashed closed every descriptor under the core,
		// so only then may a close error be fs.ErrClosed; any other error
		// joined with it still fails the test.
		if _, crashed := crashedSeams.Load(seam); crashed {
			err = withoutErrClosed(err)
		}
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); c.pool.Counters().Spares < 2*testStreams; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied its spares: %+v", c.pool.Counters())
		}
	}
	return c, seam
}

// crashedSeams holds the seams a test crashed.
var crashedSeams sync.Map

// withoutErrClosed returns the leaves of err that are not fs.ErrClosed, joined,
// or nil when there are none.
func withoutErrClosed(err error) error {
	switch e := err.(type) {
	case nil:
		return nil
	case interface{ Unwrap() []error }:
		var rest []error
		for _, c := range e.Unwrap() {
			rest = append(rest, withoutErrClosed(c))
		}
		return errors.Join(rest...)
	default:
		if errors.Is(err, fs.ErrClosed) {
			return nil
		}
		return err
	}
}

// testFile returns a file id distinguished by b.
func testFile(b byte) core.FileID { return core.NewFileID([16]byte{b}) }

// publish appends a write of payload at off to f and publishes it through the
// core's index and accounts its delta, under the file's append domain, as a
// writer does. It returns the write's version.
func publish(t *testing.T, c *Core, f core.FileID, off int64, payload string) core.Version {
	t.Helper()
	unlock := c.domains.Lock(f)
	defer unlock()
	v, err := c.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	seq, err := c.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	x := core.Extent{Off: off, Len: int64(len(payload))}
	p, err := c.Streams().For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: x, Seq: seq, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	placed := index.Placed{Extent: x, Record: index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}, Version: v}
	hd, err := c.Index().WriteRetain(1, f, placed, seq)
	if err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	c.Account(hd)
	return v
}

func syncFiles(t *testing.T, c *Core, ids ...core.FileID) {
	t.Helper()
	if err := c.Durability().Sync(context.Background(), ids...); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}
