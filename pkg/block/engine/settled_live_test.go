package engine_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A share filled past one segment with live bytes holds them in a segment
// that takes no more appends. Nothing a repack does frees a live byte, so the
// refusal at the share's limit answers no space, not a retry-later that would
// repeat for as long as the bytes stay live.
func TestAShareOfLiveBytesInAnEndedSegmentIsNoSpace(t *testing.T) {
	const (
		mib   = 1 << 20
		chunk = 64 << 10
	)

	f := enginetest.New(t, enginetest.WithSegmentCap(mib))
	f.Second.Limit = 3 * mib

	if err := f.E.Bind(t.Context(), f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, file := f.FS.Create(t, f.FS.Admin, f.SecondRoot, "live", kvstore.Regular(0o644))
	p := bytes.Repeat([]byte{'l'}, chunk)

	put := func(off int64) error {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		_, err := f.E.Write(ctx, f.Second.ID, file.ID, off, p, engine.WriteOpts{Done: func() {}})

		return err
	}

	var (
		err error
		off int64
	)

	for ; off < 2*f.Second.Limit; off += chunk {
		if err = put(off); err != nil {
			break
		}
	}

	if err == nil {
		t.Fatalf("no refusal after %d bytes into a share limited to %d", off, f.Second.Limit)
	}

	ss := f.E.Stats().Journals[f.Dir].Stats.Shares[f.Second.Tag]
	if ss.UnreclaimedEndedBytes == 0 {
		t.Fatalf("the share's ended bytes read 0 at its limit (used %d): no segment ended", ss.UsedBytes)
	}

	for i := range 3 {
		if i > 0 {
			err = put(off)
		}

		if !errors.Is(err, vfserr.ErrNoSpace) {
			t.Fatalf("refusal %d at the share limit, live bytes in an ended segment: %v, want ErrNoSpace", i, err)
		}
	}
}
