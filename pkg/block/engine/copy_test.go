package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// chunk is the copy chunk of copyUnit's engine, its clone_max_len.
const chunk = 64 << 10

// copyUnit is a clonedUnit, its clone_max_len, and so its copy chunk, chunk.
func copyUnit(t *testing.T, opts ...func(*engine.Deps)) *unit {
	t.Helper()
	u := clonedUnit(t, opts...)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Minute, GroupCommitMaxFiles: 1, CloneMaxLen: chunk}); err != nil {
		t.Fatal(err)
	}
	return u
}

// pattern is n bytes that differ from one offset to the next, so a chunk
// copied from or to the wrong offset reads back wrong.
func pattern(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return p
}

func (u *unit) copy(ctx context.Context, src, dst entity.FileID, xs ...vfserr.Extent) (int64, error) {
	return u.e.Copy(ctx, u.share.ID, src, u.share.ID, dst, xs)
}

// A copy of several chunks over two extents copies each in place and
// counts every byte.
func TestCopyCopiesEveryChunk(t *testing.T) {
	u := copyUnit(t)
	p := pattern(3 * chunk)
	src := u.committedFile(t, "src", p)
	dst := u.create(t, "dst")
	n, err := u.copy(t.Context(), src, dst,
		vfserr.Extent{SrcOff: 0, DstOff: chunk, Len: 2*chunk + 10}, vfserr.Extent{SrcOff: 5, DstOff: 0, Len: 7})
	if err != nil || n != 2*chunk+17 {
		t.Fatalf("Copy: %d, %v; want %d", n, err, 2*chunk+17)
	}
	if got := u.read(t, dst, chunk, 2*chunk+10); !bytes.Equal(got, p[:2*chunk+10]) {
		t.Error("the first extent read back wrong")
	}
	if got := u.read(t, dst, 0, 7); !bytes.Equal(got, p[5:12]) {
		t.Error("the second extent read back wrong")
	}
}

// An extent that overlaps itself in one file is refused before any chunk
// runs: the valid extent before it copies nothing.
func TestCopyRefusesAnOverlapBeforeAnyChunk(t *testing.T) {
	u := copyUnit(t)
	f := u.committedFile(t, "f", pattern(2*chunk))
	n, err := u.copy(t.Context(), f, f,
		vfserr.Extent{SrcOff: 0, DstOff: 2 * chunk, Len: chunk}, vfserr.Extent{SrcOff: 0, DstOff: chunk / 2, Len: chunk})
	if n != 0 || !errors.Is(err, vfserr.ErrInvalid) {
		t.Fatalf("Copy: %d, %v; want 0, ErrInvalid", n, err)
	}
	if size, _ := u.fs.Store.Existence().Size(t.Context(), u.share.ID, f); size != 2*chunk {
		t.Errorf("the file's size is %d after the refused copy, want %d: a chunk ran", size, 2*chunk)
	}
}

// A chunk refused after others are done answers their count and no error;
// the first chunk refused answers its error.
func TestCopyRefusedChunk(t *testing.T) {
	u := copyUnit(t)
	p := pattern(2*chunk + chunk/2)
	src := u.committedFile(t, "src", p)
	dst := u.create(t, "dst")
	// The third chunk reaches past the source's end, which its admission refuses.
	n, err := u.copy(t.Context(), src, dst, vfserr.Extent{Len: 3 * chunk})
	if err != nil || n != 2*chunk {
		t.Fatalf("Copy with its third chunk refused: %d, %v; want %d, nil", n, err, 2*chunk)
	}
	if got := u.read(t, dst, 0, 2*chunk); !bytes.Equal(got, p[:2*chunk]) {
		t.Error("the chunks done read back wrong")
	}
	n, err = u.e.Copy(t.Context(), u.share.ID, src, entity.NewShareID(), dst, []vfserr.Extent{{Len: 3 * chunk}})
	var d *vfserr.Delay
	if n != 0 || !errors.As(err, &d) || d.Cause != vfserr.DelayFrozen {
		t.Errorf("Copy with its first chunk refused: %d, %v; want 0, ErrDelay(DelayFrozen)", n, err)
	}
}

// The deadline met during a chunk answers the bytes of the chunks done,
// leaving the running chunk to finish; met during the first, it answers
// the chunk's Delay, never a count of 0.
func TestCopyDeadline(t *testing.T) {
	for k := range 2 {
		t.Run(fmt.Sprintf("chunk %d", k), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			release := make(chan struct{})
			u := copyUnit(t, onClone(func(cctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
				if op.Spec.SrcOff == int64(k)*chunk {
					cancel()
					<-release
				}
				return next.Clone(cctx, op, pending)
			}))
			p := pattern(3 * chunk)
			src := u.committedFile(t, "src", p)
			dst := u.create(t, "dst")
			n, err := u.copy(ctx, src, dst, vfserr.Extent{Len: 3 * chunk})
			close(release)
			var d *vfserr.Delay
			switch {
			case k == 0 && (n != 0 || !errors.As(err, &d) || d.Cause != vfserr.DelayClone):
				t.Errorf("the deadline in the first chunk: %d, %v; want 0, ErrDelay(DelayClone)", n, err)
			case k == 1 && (n != chunk || err != nil):
				t.Errorf("the deadline in the second chunk: %d, %v; want %d, nil", n, err, chunk)
			}
			// The chunk the deadline met still completes.
			end := (k + 1) * chunk
			u.until(t, "the running chunk completes", func() bool {
				_, err := u.e.Read(t.Context(), u.share.ID, dst, 0, make([]byte, end))
				return err == nil
			})
			if got := u.read(t, dst, 0, end); !bytes.Equal(got, p[:end]) {
				t.Errorf("chunk %d: the destination after the running chunk completed reads wrong", k)
			}
		})
	}
}

// A writer of the source is refused only by the chunk running over its
// extent, not by the whole copy: a write into the second chunk's source
// while the first runs is served, and the second chunk copies it.
func TestCopyHoldsOneChunkOfTheSource(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	u := copyUnit(t, onClone(func(cctx context.Context, next engine.Existence, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
		if op.Spec.SrcOff == 0 {
			close(held)
			<-release
		}
		return next.Clone(cctx, op, pending)
	}))
	p := pattern(2 * chunk)
	src := u.committedFile(t, "src", p)
	dst := u.create(t, "dst")
	type answer struct {
		n   int64
		err error
	}
	ans := make(chan answer, 1)
	go func() {
		n, err := u.copy(t.Context(), src, dst, vfserr.Extent{Len: 2 * chunk})
		ans <- answer{n, err}
	}()
	select {
	case <-held:
	case a := <-ans:
		t.Fatalf("Copy answered %d, %v before its first chunk's phase 1", a.n, a.err)
	}
	_, werr := u.e.Write(t.Context(), u.share.ID, src, chunk, fill(10, 'w'), engine.WriteOpts{Done: func() {}})
	close(release)
	if werr != nil {
		t.Fatalf("a write into the second chunk's source while the first runs: %v", werr)
	}
	if a := <-ans; a.err != nil || a.n != 2*chunk {
		t.Fatalf("Copy: %d, %v; want %d", a.n, a.err, 2*chunk)
	}
	if got := u.read(t, dst, chunk, 10); !bytes.Equal(got, fill(10, 'w')) {
		t.Error("the second chunk did not copy the write made while the first ran")
	}
	if got := u.read(t, dst, 0, chunk); !bytes.Equal(got, p[:chunk]) {
		t.Error("the first chunk read back wrong")
	}
}
