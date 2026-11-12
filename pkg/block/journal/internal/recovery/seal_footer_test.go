package recovery

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// coverFS opens the real files of fs, failing every read of a segment that
// covers byte at with EIO, as a device fails every read over a bad sector.
type coverFS struct {
	storage.FS
	at int64
}

func (c coverFS) Open(name string) (storage.File, error) {
	f, err := c.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if kind, _ := format.ClassifyName(name); kind != format.NameSegment {
		return f, nil
	}
	return coverFile{File: f, at: c.at}, nil
}

type coverFile struct {
	storage.File
	at int64
}

func (f coverFile) ReadAt(p []byte, off int64) (int, error) {
	if off <= f.at && f.at < off+int64(len(p)) {
		return 0, syscall.EIO
	}
	return f.File.ReadAt(p, off)
}

// TestRunRebuildASealedSegmentWithAnUnreadableFooterOpens: a byte past a
// seal marker that fails with EIO on every attempt — in the footer a sealed
// segment holds there — does not refuse the open: the rebuild's walk reads
// the marker's own bytes and no more, so no read of the bounded reader
// covers the bad byte and no range is collected for it.
func TestRunRebuildASealedSegmentWithAnUnreadableFooterOpens(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	seg := buildSeg(1, rec(3, nil), rec(3, nil))
	seal := int64(len(seg))
	seg = append(seg, raw.SealMarker{Identity: journalID, Segment: 1, Offset: uint64(seal)}.Bytes()...)
	seg = append(seg, make([]byte, 128)...) // the footer the walk must never read
	r.plantSeg(1, seg)

	deps := r.runDeps()
	deps.FS = coverFS{FS: deps.FS, at: seal + format.SealMarkerSize + 8}
	st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
	var ure *core.UnreadableRangesError
	if errors.As(err, &ure) {
		t.Fatalf("Run with a footer byte unreadable refused on the ranges %v, want the open to rebuild from the marker's own bytes", ure.Ranges)
	}
	if err != nil {
		t.Fatalf("Run with a footer byte unreadable: %v, want no error", err)
	}
	if len(st.Rebuilt.Scans) != 1 || !st.Rebuilt.Scans[0].Sealed || st.Rebuilt.Scans[0].End != seal {
		t.Errorf("Run: scans %+v, want one sealed at the marker's %d", st.Rebuilt.Scans, seal)
	}
}
