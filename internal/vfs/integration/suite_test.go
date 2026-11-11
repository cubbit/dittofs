//go:build engine_conformance

package integration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/suite"
)

// The service's conformance suite holds over the filesystem composed with the
// engine.
func TestSuite(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return compose(t).fs })
}

// The filesystem's Stop closes nothing it was handed: after it, the engine
// still reads the file written through the filesystem, and open state still
// serves, until the fixture's own stop.
func TestStopClosesNothingItWasHanded(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, id := c.File("f")
	data := []byte("hello")
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, data, vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	c.StopService()
	buf := make([]byte, len(data))
	if n, err := c.E.Read(ctx, c.Share.ID, id, 0, buf); err != nil || !bytes.Equal(buf[:n], data) {
		t.Errorf("the engine's Read after the filesystem's Stop: %q, %v; want %q", buf[:n], err, data)
	}
	if _, err := c.Opens.Clients().AddSession(ctx, 1, entity.NodeID{}, nil, nil); errors.Is(err, openstate.ErrStopped) {
		t.Errorf("open state is stopped after the filesystem's Stop: %v", err)
	}
}

// A write to a share the engine has not bound reaches the caller as the
// engine answers it, ErrDelay with DelayFrozen, for a retry once the binding
// comes.
func TestAWriteToAnUnboundShareIsFrozen(t *testing.T) {
	c := compose(t)
	h, _ := c.FS.Create(t, c.FS.Admin, c.SecondRoot, "f", kvstore.Regular(0o644))
	_, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
	if !errors.Is(err, &vfs.Delay{Cause: vfs.DelayFrozen}) {
		t.Errorf("a write to the unbound share: %v, want ErrDelay (DelayFrozen)", err)
	}
}

// A write acknowledged and committed through the service, its existence
// still pending, survives a crash: the composition built after it reads the
// bytes back, and the store holds the size the start's re-application of
// existence wrote.
func TestACommittedWriteSurvivesACrash(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, _ := c.File("f")
	data := []byte("survives")
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, data, vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	// The commit syncs the write in the journal and leaves its existence
	// pending, for the next start to re-apply.
	if _, err := c.fs.Commit(ctx, c.admin, vfs.Anonymous(h), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.Committed(h).Size; got != 0 {
		t.Fatalf("the store's size before the crash %d, want 0, the existence pending", got)
	}
	c.Crash()
	if got := c.Committed(h).Size; got != int64(len(data)) {
		t.Errorf("the store's size after the crash %d, want %d: the start re-applied no existence", got, len(data))
	}
	buf := make([]byte, len(data))
	n, _, err := c.fs.Read(ctx, c.admin, vfs.Anonymous(h), 0, buf)
	if err != nil || !bytes.Equal(buf[:n], data) {
		t.Errorf("the read after the crash: %q, %v; want %q", buf[:n], err, data)
	}
}

// While a truncate is pending, its phase 1 held at the store, GetAttr answers
// its size, below both the committed record and the writes pending before
// it; and the same once phase 1 commits.
func TestAPendingTruncateIsTheSize(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, id := c.File("f")
	const mib = 1 << 20
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, make([]byte, mib), vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if err := c.E.CommitExistence(ctx, c.Share.ID, id); err != nil {
		t.Fatal(err)
	}
	if got := c.Committed(h).Size; got != mib {
		t.Fatalf("the committed record's size %d, want %d", got, mib)
	}
	if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), mib, make([]byte, mib), vfs.Unstable); err != nil {
		t.Fatal(err)
	}
	if f, err := c.fs.GetAttr(ctx, c.admin, h); err != nil || f.Size != 2*mib {
		t.Fatalf("GetAttr with the writes pending: size %d, %v; want %d", f.Size, err, 2*mib)
	}
	const hold = time.Second
	held := c.HoldNextWrite(hold, nil)
	truncated := make(chan error, 1)
	go func() {
		_, err := c.fs.SetAttr(ctx, c.admin, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(mib / 2)})
		truncated <- err
	}()
	select {
	case <-held:
	case err := <-truncated:
		// Lifted, so the hold does not catch the fixture's stop instead.
		c.Heal()
		t.Fatalf("the truncate ended before its phase 1 reached the store: %v", err)
	}
	during, err := c.fs.GetAttr(ctx, c.admin, h)
	c.Clock.Advance(hold)
	if err := <-truncated; err != nil {
		t.Fatal(err)
	}
	if err != nil || during.Size != mib/2 {
		t.Errorf("GetAttr before the truncate's phase 1: size %d, %v; want %d", during.Size, err, mib/2)
	}
	if f, err := c.fs.GetAttr(ctx, c.admin, h); err != nil || f.Size != mib/2 {
		t.Errorf("GetAttr after the truncate's phase 1: size %d, %v; want %d", f.Size, err, mib/2)
	}
}

// A chown commits the file's pending existence first: alice's write staged
// before it is charged to alice, and the chown moves the whole charge to bob
// at once, so bob's usage rises by the file's charge once, and no later
// commit charges the write again.
func TestAChownChargesStagedBytesToTheOldOwnerFirst(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	h, _ := c.owned(t, "f", c.alice.Principal, 0o644)
	data := make([]byte, 64<<10)
	alices, bobs := c.usage(t, c.alice.Principal), c.usage(t, c.bob.Principal)
	if _, err := c.fs.Write(ctx, signedIn(c.alice.Principal), vfs.Anonymous(h), 0, data, vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(c.bob.Principal)}
	if _, err := c.fs.SetAttr(ctx, c.admin, vfs.Target{H: h}, chown); err != nil {
		t.Fatal(err)
	}
	charged := c.Committed(h).Charged
	if charged < int64(len(data)) {
		t.Errorf("the file's charge after the chown %d, want the %d bytes staged before it", charged, len(data))
	}
	if got := c.usage(t, c.bob.Principal); got.Bytes != bobs.Bytes+charged {
		t.Errorf("bob's usage after the chown %d bytes, want %d", got.Bytes, bobs.Bytes+charged)
	}
	if got := c.usage(t, c.alice.Principal); got.Bytes != alices.Bytes {
		t.Errorf("alice's usage after the chown %d bytes, want %d as before her write", got.Bytes, alices.Bytes)
	}
	if _, err := c.fs.Commit(ctx, c.admin, vfs.Anonymous(h), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.usage(t, c.bob.Principal); got.Bytes != bobs.Bytes+charged {
		t.Errorf("bob's usage after a later commit %d bytes, want %d: the write was charged again", got.Bytes, bobs.Bytes+charged)
	}
}

// A store refusing one file's records leaves that file's stable overwrite
// unanswered success while the fault holds, and another file's stable
// overwrite still commits.
func TestAFileFailingItsCommitDelaysOnlyItself(t *testing.T) {
	c := compose(t)
	ctx := t.Context()
	ha, a := c.File("a")
	hb, b := c.File("b")
	for _, f := range []struct {
		h  vfs.Handle
		id entity.FileID
	}{{ha, a}, {hb, b}} {
		if _, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(f.h), 0, []byte("before"), vfs.Unstable); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		if err := c.E.CommitExistence(ctx, c.Share.ID, f.id); err != nil {
			t.Fatal(err)
		}
	}
	c.FailUpdatesSetting(func(key []byte) bool { return bytes.Contains(key, a[:]) })
	// Lifted before the fixture's stop, and the clock moved past a's retry,
	// so the engine's close finds a's commit done rather than waiting on it.
	t.Cleanup(func() {
		c.FailUpdatesSetting(nil)
		c.Clock.Advance(time.Minute)
	})
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := c.fs.Write(short, c.admin, vfs.Anonymous(ha), 0, []byte("after!"), vfs.FileSync); err == nil {
		t.Error("a's stable overwrite answered success while the store refused a's records")
	}
	// Bounded, so a fault that wedges every file fails here rather than
	// holding the test.
	long, cancelLong := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLong()
	if _, err := c.fs.Write(long, c.admin, vfs.Anonymous(hb), 0, []byte("after!"), vfs.FileSync); err != nil {
		t.Errorf("b's stable overwrite while the store refused a's records: %v", err)
	}
}
