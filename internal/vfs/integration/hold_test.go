//go:build engine_conformance

package integration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	filesystem "github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// awaiting is the engine with each AwaitClones call delivered on reached as
// it arrives: a pass-through wrapper, the engine answering every call. A
// test reads reached to know a call is waiting behind a clone. A call past
// reached's room is not delivered, so a filesystem that waits again and again
// fails a test on its assertion rather than blocking here.
type awaiting struct {
	filesystem.Content
	reached chan entity.FileID
}

func (a *awaiting) AwaitClones(ctx context.Context, f entity.FileID) error {
	select {
	case a.reached <- f:
	default:
	}
	return a.Content.AwaitClones(ctx, f)
}

// over is a composeOver wrap calling the engine through a.
func (a *awaiting) over(_ *composition, e filesystem.Content) filesystem.Content {
	a.Content = e
	return a
}

// composeAwaiting is the composition over the fake journal, the engine
// reached through an awaiting wrapper with room for n waits.
func composeAwaiting(t *testing.T, n int) (*composition, *awaiting) {
	t.Helper()
	a := &awaiting{reached: make(chan entity.FileID, n)}
	return composeOver(t, a.over, enginetest.WithFakeJournal()), a
}

// startClone runs a clone of x from src to dst as the administrator, its
// next clone held at phase 1 until release, and answers once it is held,
// with the channel its answer arrives on.
func (c *composition) startClone(t *testing.T, src, dst vfs.Handle, x vfs.Extent) (cloned <-chan error, release func()) {
	t.Helper()
	entered, release := c.HoldNextClone()
	ch := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backstop)
		defer cancel()
		_, err := c.fs.Clone(ctx, c.admin, copyArgs(src, dst, x))
		ch <- err
	}()
	select {
	case <-entered:
	case err := <-ch:
		t.Fatalf("the clone answered %v before its phase 1 reached the store", err)
	case <-time.After(backstop):
		t.Fatalf("the clone's phase 1 did not reach the store within %v", backstop)
	}
	return ch, release
}

// waits takes n waits from a's reached, failing the test past the backstop:
// a call that answered instead of waiting never arrives there.
func (a *awaiting) waits(t *testing.T, n int, what string) {
	t.Helper()
	for i := range n {
		select {
		case <-a.reached:
		case <-time.After(backstop):
			t.Fatalf("%s: %d of %d calls waited behind the clone within %v", what, i, n, backstop)
		}
	}
}

// answer is what arrives on ch within the backstop.
func answer(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(backstop):
		t.Fatalf("%s did not answer within %v", what, backstop)
		return nil
	}
}

// hold is the hold a held call carries: inside the backstop, so a call
// held past the clone's end answers DelayClone before a test gives up on it.
const hold = backstop / 2

var delayClone = &vfs.Delay{Cause: vfs.DelayClone}

// V30: the service waits inside a call only behind a running clone, and at
// most the call's WithHold. A write into a cloning extent with
// a hold is held, and completes once the clone ends, its bytes
// landing over the clone's; with WithHold(0) it answers DelayClone at once,
// never waiting; with a hold shorter than the clone it answers DelayClone
// once the hold runs out, and a held write whose caller cancels answers
// DelayClone then.
func TestV30(t *testing.T) {
	c, a := composeAwaiting(t, 8)
	src := c.written(t, "src", c.FS.Admin.Principal, 0o644, []byte("abcdefgh"))
	dst := c.written(t, "dst", c.FS.Admin.Principal, 0o644, []byte("01234567"))
	cloned, release := c.startClone(t, src, dst, vfs.Extent{Len: 4})
	ctx, cancel := context.WithTimeout(t.Context(), backstop)
	defer cancel()
	write := func(ctx context.Context) error {
		_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(dst), 0, []byte("wxyz"), vfs.Unstable)
		return err
	}
	if err := write(vfs.WithHold(ctx, 0)); !errors.Is(err, delayClone) {
		t.Errorf("a write into the cloning extent with WithHold(0): %v, want DelayClone", err)
	}
	if len(a.reached) != 0 {
		t.Error("a write with WithHold(0) waited behind the clone, want DelayClone at once")
	}
	// No clock reaches the hold's timer: the real 20 ms hold running out
	// within the backstop is the assertion.
	if err := write(vfs.WithHold(ctx, 20*time.Millisecond)); !errors.Is(err, delayClone) {
		t.Errorf("a write with a hold shorter than the clone: %v, want DelayClone once it runs out", err)
	}
	a.waits(t, 1, "the write with a short hold")

	cancelled, cancelHeld := context.WithCancel(ctx)
	gaveUp := make(chan error, 1)
	go func() { gaveUp <- write(vfs.WithHold(cancelled, hold)) }()
	a.waits(t, 1, "the write its caller cancels")
	cancelHeld()
	if err := answer(t, gaveUp, "the held write its caller cancelled"); !errors.Is(err, delayClone) {
		t.Errorf("a held write its caller cancelled: %v, want DelayClone", err)
	}

	held := make(chan error, 1)
	go func() { held <- write(vfs.WithHold(ctx, hold)) }()
	a.waits(t, 1, "the held write")
	select {
	case err := <-held:
		t.Fatalf("the held write answered %v while the clone runs", err)
	case err := <-cloned:
		t.Fatalf("the clone answered %v before its release", err)
	default:
	}
	release()
	if err := answer(t, cloned, "the clone"); err != nil {
		t.Fatalf("the clone once released: %v", err)
	}
	if err := answer(t, held, "the held write"); err != nil {
		t.Errorf("the held write once the clone ended: %v, want it to complete", err)
	}
	if got, err := c.readAt(t, dst, 0, 8); err != nil || !bytes.Equal(got, []byte("wxyz4567")) {
		t.Errorf("the destination after the clone and the held write reads %q, %v; want %q", got, err, "wxyz4567")
	}
}

// V29, the clone case: a 1 GiB clone of a VM disk under writes, with a lease
// break pending. alice-pc holds the disk open, denying writes, under a
// read-and-handle lease, and build01's write open has started its break.
// Each of alice-pc's writes into the source extent is held behind the clone,
// holding neither its open-state step nor any lock, so alice-pc's
// acknowledgement of the break and a new open of the disk by build01 both
// answer during the clone; once it ends, each held write runs its checks
// again and completes, and the snapshot holds the disk as the clone read it.
func TestV29Clone(t *testing.T) {
	const writers = 4
	c, a := composeAwaiting(t, writers)
	ctx, cancel := context.WithTimeout(t.Context(), backstop)
	defer cancel()
	disk := c.written(t, "disk", c.FS.Admin.Principal, 0o666, []byte("boot"))
	if _, err := c.fs.SetAttr(ctx, c.admin, vfs.Target{H: disk}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(1 << 30)}); err != nil {
		t.Fatalf("growing the disk to 1 GiB: %v", err)
	}
	snap := c.written(t, "disk.snap", c.FS.Admin.Principal, 0o666, nil)

	pc := c.smb(t, "alice-pc", c.alice.Principal)
	open := func(access entity.AccessMask) vfs.OpenRequest {
		return vfs.OpenRequest{Dir: c.FS.Root, Name: []byte("disk"), Disposition: vfs.OpenExisting, Access: access, Timeout: time.Minute}
	}
	req := open(entity.ReadData | entity.WriteData)
	req.Deny, req.Lease, req.Want = entity.WriteData, vfs.LeaseKey{1}, openstate.GrantRead|openstate.GrantHandle
	leased, err := c.fs.Open(ctx, pc, req)
	if err != nil || leased.Grant == nil {
		t.Fatalf("alice-pc's leased open: %+v, %v", leased.Grant, err)
	}
	build := c.smb(t, "build01", c.bob.Principal)
	if _, err := c.fs.Open(ctx, build, open(entity.WriteData)); !errors.Is(err, vfs.ErrDelay) {
		t.Fatalf("build01's write open against the cached deny-write: %v, want ErrDelay while the lease breaks", err)
	}

	cloned, release := c.startClone(t, disk, snap, vfs.Extent{Len: 1 << 30})
	wrote := make(chan error, writers)
	for i := range writers {
		go func() {
			_, err := c.fs.Write(vfs.WithHold(ctx, hold), pc, leased.Ref, int64(i)<<20, []byte("vm"), vfs.Unstable)
			wrote <- err
		}()
	}
	a.waits(t, writers, "alice-pc's writes into the source extent")

	// Each call gets a deadline well inside the backstop: one waiting on a
	// lock or open-state step a held write kept answers at it instead.
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		t.Cleanup(cancel)
		return ctx
	}
	if err := c.fs.AckRecall(short(), pc, leased.Grant.ID, openstate.GrantNone); err != nil {
		t.Errorf("alice-pc's acknowledgement of the break during the clone: %v", err)
	}
	if r, err := c.fs.Open(short(), build, open(entity.ReadData)); err != nil {
		t.Errorf("build01's read open of the disk during the clone: %v", err)
	} else if _, err := c.fs.Close(short(), build, r.Ref, vfs.Seqs{}); err != nil {
		t.Errorf("build01's close during the clone: %v", err)
	}
	select {
	case err := <-wrote:
		t.Fatalf("a held write answered %v while the clone runs", err)
	default:
	}

	release()
	if err := answer(t, cloned, "the clone"); err != nil {
		t.Fatalf("the clone once released: %v", err)
	}
	for range writers {
		if err := answer(t, wrote, "a held write"); err != nil {
			t.Errorf("a held write once the clone ended: %v, want it to complete", err)
		}
	}
	for i := range writers {
		if got, err := c.readAt(t, disk, int64(i)<<20, 2); err != nil || !bytes.Equal(got, []byte("vm")) {
			t.Errorf("the disk at %d MiB after the held writes reads %q, %v; want %q", i, got, err, "vm")
		}
	}
	if got, err := c.readAt(t, snap, 0, 4); err != nil || !bytes.Equal(got, []byte("boot")) {
		t.Errorf("the snapshot reads %q, %v; want the disk as the clone read it, %q", got, err, "boot")
	}
	if got, err := c.readAt(t, snap, 1<<20, 2); err != nil || !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("the snapshot at 1 MiB reads %q, %v; want the hole the clone read there", got, err)
	}
}
