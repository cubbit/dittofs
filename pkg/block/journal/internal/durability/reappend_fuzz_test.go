package durability

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// FuzzReappendWindow fails a segment holding a random mix of writes, removals
// and releases, with the re-read of some writes failing. Every record is
// either re-appended whole, at its original sequence number, or failed with
// one loss event, never both and never neither, and the window resolves.
func FuzzReappendWindow(f *testing.F) {
	f.Add([]byte{0, 8, 1, 3, 2, 1}, uint8(0))
	f.Add([]byte{0, 8, 0, 9, 0, 10}, uint8(0b101))
	f.Add([]byte{0, 1, 0, 2, 0, 3, 1, 1}, uint8(0xff))
	f.Fuzz(func(t *testing.T, spec []byte, failMask uint8) {
		n := min(len(spec)/2, 6)
		if n == 0 {
			return
		}
		fr, seam := newRereadRig(t)
		type rec struct {
			content bool
			seq     core.Seq
			failed  bool
		}
		var recs []rec
		var first segment.Location
		for i := range n {
			kind, size := spec[2*i]%3, int(spec[2*i+1]%64)+1
			fl := file(byte(i%3 + 1))
			if kind == 0 {
				loc, _ := fr.publishAs(t, fl, int64(i)*128, string(make([]byte, size)), core.Version{Counter: uint64(i + 1)}, nil)
				r := rec{content: true, seq: core.Seq(fr.lastSeq.Load()), failed: failMask>>uint(i)&1 == 1}
				if r.failed {
					seam.set(loc, func([]byte) (int, error) { return 0, syscall.EIO })
				}
				recs = append(recs, r)
				if i == 0 {
					first = loc
				}
				continue
			}
			k, ext := format.KindDelete, core.Extent{}
			if kind == 2 {
				k, ext = format.KindRelease, core.Extent{Off: int64(i) * 128, Len: int64(size)}
			}
			seq := core.Seq(1000 + i)
			loc := fr.appendHeaderOnly(t, fl, k, ext, seq, core.Version{Counter: uint64(100 + i)}, format.RecordBody{})
			recs = append(recs, rec{seq: seq})
			if i == 0 {
				first = loc
			}
		}
		fr.failSyncKeepingPages(t, first.Segment, errors.New("sync boom"))
		// The Sync returns once every file's records are durable or failed.
		_ = mustReturn(t, "Sync", fr.syncAsync(context.Background(), file(1), file(2), file(3)))
		fr.awaitResolved(t)

		hs, _ := fr.durableHeaders(t)
		durable := map[core.Seq]int{}
		for _, h := range hs {
			if h.Segment != first.Segment && h.Kind != format.KindLoss {
				durable[h.Seq]++
			}
		}
		lost := map[core.Seq]int{}
		for _, l := range fr.lossRecordsOnDisk(t) {
			lost[core.Seq(l.Dropped)]++
		}
		for _, r := range recs {
			wantLost := r.content && r.failed
			if durable[r.seq] != b2i(!wantLost) || lost[r.seq] != b2i(wantLost) {
				t.Errorf("seq %d (content %v, failed %v): re-appended %d times, lost %d times", r.seq, r.content, r.failed, durable[r.seq], lost[r.seq])
			}
		}
		if ws := fr.windowsOf(0); len(ws) != 0 {
			t.Errorf("windows still pending: %+v", ws)
		}
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
