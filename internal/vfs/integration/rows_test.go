//go:build engine_conformance

package integration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

const mib = 1 << 20

// bound is a generous real-time backstop for a wait that must end, so a wrong
// engine fails on its assertion instead of hanging the package.
const bound = 10 * time.Second

// smb is a call from a new SMB client named name, signed in as p.
func (c *composition) smb(t *testing.T, name string, p entity.Principal) vfs.Call {
	t.Helper()
	cl := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte(name), Boot: [8]byte{1}}}
	pr, err := c.Opens.Clients().Propose(t.Context(), cl, client)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := c.Opens.Clients().Connect(t.Context(), cl, &pr)
	if err != nil {
		t.Fatal(err)
	}
	call := signedIn(p)
	call.Protocol, call.Client = entity.ProtocolSMB, id
	return call
}

// open opens name in the share's root as call, for reading and writing, with
// durability d.
func (c *composition) open(t *testing.T, call vfs.Call, name string, d entity.Durability) vfs.OpenRef {
	t.Helper()
	r, err := c.fs.Open(t.Context(), call, vfs.OpenRequest{
		Dir: c.FS.Root, Name: []byte(name), Disposition: vfs.OpenExisting,
		Access: entity.ReadData | entity.WriteData, Durability: d, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	return r.Ref
}

// continuouslyAvailable marks the share continuously available, as a share
// granting persistent opens is.
func (c *composition) continuouslyAvailable(t *testing.T) {
	t.Helper()
	cp := c.FS.Store.ControlPlane()
	p, err := cp.ExportPolicy(t.Context(), c.Share.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.SMB |= entity.SMBContinuouslyAvailable
	if err := cp.PutExportPolicy(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

// bounded is a context that ends after bound, so a wrong engine fails on its
// assertion instead of hanging the package.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	t.Cleanup(cancel)
	return ctx
}

// write writes p at off of the file h names as call, unstable, under a context
// that ends after bound.
func (c *composition) write(t *testing.T, call vfs.Call, h vfs.Handle, off int64, p []byte) (vfs.WriteResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	return c.fs.Write(ctx, call, vfs.Anonymous(h), off, p, vfs.Unstable)
}

// read is what the file h names holds over [off, off+n), read as the
// administrator.
func (c *composition) read(t *testing.T, h vfs.Handle, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	got, _, err := c.fs.Read(bounded(t), c.admin, vfs.Anonymous(h), off, p)
	if err != nil {
		t.Fatalf("read %d bytes at %d: %v", n, off, err)
	}
	return p[:got]
}

// synced reports whether an engine.sync span ended among spans.
func synced(spans []*enginetest.Span) bool {
	for _, sp := range spans {
		if sp.Name == "engine.sync" {
			return true
		}
	}
	return false
}

// overBoth runs rows over the journal and over the fake journal. Over the
// journal they wait on the parts it has not built; over the fake they check
// the engine and the filesystem above it, whose logic the rows are about.
func overBoth(t *testing.T, rows func(t *testing.T, opts ...enginetest.Option)) {
	t.Run("journal", func(t *testing.T) { rows(t) })
	t.Run("fake journal", func(t *testing.T) { rows(t, enginetest.WithFakeJournal()) })
}

// RFC 17 V19: a write through a persistent open is stable whatever it asks; a
// stable write waits for its sync and, overwriting committed content, for its
// existence commit, and for nothing else.
func TestV19(t *testing.T) { overBoth(t, v19) }

func v19(t *testing.T, opts ...enginetest.Option) {
	t.Run("an unstable write on a persistent open survives a crash", func(t *testing.T) {
		c := compose(t, opts...)
		_, id := c.File("f")
		c.continuouslyAvailable(t)
		pc := c.smb(t, "pc", c.FS.Admin.Principal)
		ref := c.open(t, pc, "f", entity.DurabilityPersistent)
		data := []byte("persistent")
		before := len(c.Spans())
		if _, err := c.fs.Write(bounded(t), pc, ref, 0, data, vfs.Unstable); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		// The reply waits for the write's sync, which ends an engine.sync span.
		if !synced(c.Spans()[before:]) {
			t.Errorf("the persistent open's unstable write was answered before any sync")
		}
		c.Crash()
		// Read from the engine: the service refuses reads while the persistent
		// open's grace period lasts.
		got := make([]byte, len(data))
		if n, err := c.E.Read(bounded(t), c.Share.ID, id, 0, got); err != nil || !bytes.Equal(got[:n], data) {
			t.Errorf("after the crash the file reads %q, %v; want %q", got[:n], err, data)
		}
	})

	t.Run("a stable append waits for no metadata transaction", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.File("f")
		c.FS.Counts.TakeUpdates()
		if _, err := c.fs.Write(bounded(t), c.admin, vfs.Anonymous(h), 0, []byte("appended"), vfs.FileSync); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		if got := c.FS.Counts.TakeUpdates(); got.Updates != 0 {
			t.Errorf("the stable append ran %d store transactions before its reply, want 0", got.Updates)
		}
	})

	t.Run("a stable overwrite stalled with the store is a delay", func(t *testing.T) {
		c := compose(t, opts...)
		h, id := c.File("f")
		if _, err := c.fs.Write(bounded(t), c.admin, vfs.Anonymous(h), 0, []byte("before"), vfs.Unstable); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		if err := c.E.CommitExistence(bounded(t), c.Share.ID, id); err != nil {
			t.Fatal(err)
		}
		// The store holds the overwrite's existence commit until the clock
		// passes a minute. The cleanup lifts the hold and moves the clock past
		// it, so the fixture's stop finds the commit done.
		held := c.HoldNextWrite(time.Minute, nil)
		t.Cleanup(func() {
			c.Heal()
			c.Clock.Advance(time.Minute)
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), 0, []byte("after!"), vfs.FileSync)
		if !errors.Is(err, &vfs.Delay{Cause: vfs.DelayCommit}) {
			t.Errorf("a stable overwrite with the store stalled: %v, want ErrDelay (DelayCommit) at the deadline", err)
		}
		select {
		case <-held:
		default:
			t.Error("the stable overwrite was answered before its existence commit reached the store")
		}
	})
}

// RFC 17 V20, adapted: a refusal at the share's journal limit is ErrDelay
// (DelayCapacity) while repack can free space, and ErrNoSpace at once when
// nothing can; a hard quota is ErrQuota at once. The cases that free dirty
// bytes by offload, wait out an unreachable store, or meet a store refusing
// puts need the remote tier, which this composition does not have: RFC 8's
// second part adds them here.
func TestV20(t *testing.T) { overBoth(t, v20) }

func v20(t *testing.T, opts ...enginetest.Option) {
	t.Run("a refusal repack can answer is a delay until it frees", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.File("f")
		g, _ := c.File("g")
		// 7 MiB recorded under the 8 MiB limit, 3 MiB of them superseded.
		for i := range 4 {
			if _, err := c.write(t, c.admin, h, int64(i*mib), bytes.Repeat([]byte{'a'}, mib)); err != nil {
				t.Fatalf("the first write: %v", err)
			}
		}
		for i := range 3 {
			if _, err := c.write(t, c.admin, h, int64(i*mib), bytes.Repeat([]byte{'b'}, mib)); err != nil {
				t.Fatal(err)
			}
		}
		// Past the journal's sync bound, so the superseded bytes are unreclaimed.
		c.Clock.Advance(time.Minute)

		reached, release := c.HoldFirstRepack()
		first := make(chan error, 1)
		go func() {
			_, err := c.write(t, c.admin, h, 4*mib, bytes.Repeat([]byte{'c'}, 2*mib))
			first <- err
		}()
		select {
		case <-reached:
		case err := <-first:
			t.Fatalf("the write past the limit answered %v before its repack reached the journal", err)
		case <-time.After(bound):
			t.Fatal("the write past the limit started no repack")
		}
		second := bytes.Repeat([]byte{'d'}, 3*mib/2)
		_, err := c.write(t, c.admin, g, 0, second)
		if !errors.Is(err, &vfs.Delay{Cause: vfs.DelayCapacity}) || errors.Is(err, vfs.ErrNoSpace) {
			t.Errorf("a write refused while the repack runs: %v, want ErrDelay (DelayCapacity)", err)
		}
		release()
		select {
		case err := <-first:
			if err != nil {
				t.Errorf("the write whose repack freed the superseded bytes: %v, want served", err)
			}
		case <-time.After(bound):
			t.Fatal("the write past the limit was not answered once its repack ran")
		}
		if _, err := c.write(t, c.admin, g, 0, second); err != nil {
			t.Errorf("the refused write's retry: %v, want served", err)
		}
	})

	t.Run("a refusal nothing can free is no space at once", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.File("f")
		var err error
		for i := 0; err == nil; i++ {
			if i == shareLimit/mib+1 {
				t.Fatal("the share took more distinct content than its limit")
			}
			start := time.Now()
			_, err = c.write(t, c.admin, h, int64(i*mib), bytes.Repeat([]byte{byte('a' + i)}, mib))
			if i == 0 && err != nil {
				t.Fatalf("the first write: %v", err)
			}
			if err != nil {
				if !errors.Is(err, vfs.ErrNoSpace) {
					t.Errorf("a write past the limit with nothing to free: %v, want ErrNoSpace", err)
				}
				if took := time.Since(start); took > bound/10 {
					t.Errorf("the refusal took %v, want under %v: it waited", took, bound/10)
				}
			}
		}
		if got := c.read(t, h, 0, 1); !bytes.Equal(got, []byte{'a'}) {
			t.Errorf("the first write reads back %q after the refusal, want %q", got, "a")
		}
	})

	t.Run("a write past a hard quota is refused at once", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.owned(t, "f", c.alice.Principal, 0o644)
		q := entity.Quota{Principal: c.alice.Principal, Hard: entity.Limit{Bytes: 4}}
		if err := c.FS.Store.ControlPlane().SetQuota(t.Context(), c.Share.ID, q); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := c.write(t, signedIn(c.alice.Principal), h, 0, []byte("12345"))
		var qe *vfs.QuotaError
		if !errors.As(err, &qe) || qe.Scope != vfs.QuotaUser || errors.Is(err, vfs.ErrDelay) {
			t.Errorf("a write past alice's hard quota: %v, want ErrQuota naming her quota", err)
		}
		if took := time.Since(start); took > bound/10 {
			t.Errorf("the refusal took %v, want under %v: it waited", took, bound/10)
		}
	})
}

// RFC 17 V22, its write, truncate and deallocate cases: an unprivileged
// caller's change of a setuid or setgid file, the owner's included, clears the
// bits in a committed change before the bytes are staged. Its copy and clone
// cases run with the filesystem's copy and clone.
func TestV22(t *testing.T) { overBoth(t, v22) }

func v22(t *testing.T, opts ...enginetest.Option) {
	// setID is a file of alice's, setuid and setgid, holding "data" written
	// by the administrator, who keeps the bits.
	setID := func(t *testing.T, c *composition) vfs.Handle {
		t.Helper()
		h, _ := c.owned(t, "f", c.alice.Principal, 0o6777)
		if _, err := c.write(t, c.admin, h, 0, []byte("data")); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		if m := c.Committed(h).Mode; m != 0o6777 {
			t.Fatalf("the administrator's write left mode %o, want 6777", m)
		}
		return h
	}
	cleared := func(t *testing.T, c *composition, h vfs.Handle, who string) {
		t.Helper()
		if f, err := c.fs.GetAttr(t.Context(), c.admin, h); err != nil || f.Mode != 0o777 {
			t.Errorf("GetAttr after %s: mode %o, %v; want 777", who, f.Mode, err)
		}
		if m := c.Committed(h).Mode; m != 0o777 {
			t.Errorf("the store's mode after %s %o, want 777 committed", who, m)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, c *composition, h vfs.Handle) error
	}{
		{"a write by bob", func(t *testing.T, c *composition, h vfs.Handle) error {
			_, err := c.write(t, signedIn(c.bob.Principal), h, 0, []byte("x"))
			return err
		}},
		{"a write by alice", func(t *testing.T, c *composition, h vfs.Handle) error {
			_, err := c.write(t, signedIn(c.alice.Principal), h, 0, []byte("x"))
			return err
		}},
		{"a truncate by bob", func(t *testing.T, c *composition, h vfs.Handle) error {
			_, err := c.fs.SetAttr(bounded(t), signedIn(c.bob.Principal), vfs.Target{H: h},
				vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(2)})
			return err
		}},
		{"a deallocate by bob", func(t *testing.T, c *composition, h vfs.Handle) error {
			_, err := c.fs.Deallocate(bounded(t), signedIn(c.bob.Principal), vfs.Anonymous(h), 0, 2)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compose(t, opts...)
			h := setID(t, c)
			if err := tc.change(t, c, h); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			cleared(t, c, h, tc.name)
		})
	}

	t.Run("a clear that fails stages nothing", func(t *testing.T) {
		c := compose(t, opts...)
		h := setID(t, c)
		c.FailWrites()
		t.Cleanup(c.Heal)
		if _, err := c.write(t, signedIn(c.bob.Principal), h, 0, []byte("x")); err == nil {
			t.Fatal("bob's write answered success while the store refused the clear")
		}
		c.Heal()
		if got := c.read(t, h, 0, 4); !bytes.Equal(got, []byte("data")) {
			t.Errorf("the file reads %q after a write whose clear failed, want %q: the bytes were staged first", got, "data")
		}
	})

	t.Run("an append-only file", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.owned(t, "f", c.alice.Principal, 0o666)
		if _, err := c.write(t, c.admin, h, 0, []byte("old")); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		files := c.FS.Store.Files()
		if _, err := files.SetAttrs(t.Context(), c.FS.Admin, h, entity.Attrs{}.WithMode(0o4666)); err != nil {
			t.Fatal(err)
		}
		if _, err := files.SetAttrs(t.Context(), c.FS.Admin, h, entity.Attrs{}.WithFlags(entity.FlagAppendOnly)); err != nil {
			t.Fatal(err)
		}
		bob := signedIn(c.bob.Principal)
		if _, err := c.write(t, bob, h, 0, []byte("new")); !errors.Is(err, vfs.ErrPerm) {
			t.Errorf("bob's write below an append-only file's size: %v, want ErrPerm", err)
		}
		if m := c.Committed(h).Mode; m != 0o4666 {
			t.Errorf("the refused write left mode %o, want 4666", m)
		}
		if _, err := c.write(t, bob, h, 3, []byte("new")); err != nil {
			t.Errorf("bob's append to a setuid, append-only file: %v", err)
		}
		if m := c.Committed(h).Mode; m != 0o666 {
			t.Errorf("the append left mode %o, want 666", m)
		}
	})

	t.Run("a write of no byte", func(t *testing.T) {
		c := compose(t, opts...)
		h := setID(t, c)
		before, err := c.fs.GetAttr(t.Context(), c.admin, h)
		if err != nil {
			t.Fatal(err)
		}
		c.Clock.Advance(time.Second)
		if _, err := c.write(t, signedIn(c.bob.Principal), h, 0, nil); err != nil {
			t.Fatalf("bob's write of no byte: %v", err)
		}
		after, err := c.fs.GetAttr(t.Context(), c.admin, h)
		if err != nil || after.Mode != 0o6777 || !after.Modify.Equal(before.Modify) {
			t.Errorf("after bob's write of no byte: mode %o, mtime %v, %v; want 6777, %v", after.Mode, after.Modify, err, before.Modify)
		}
	})
}

// RFC 17 V25, adapted: Write samples the verifier before staging and Commit
// after its sync, so a sync window that fails between them is told apart; and
// an SMB flush across a loss fails once per open. A shard handed away and
// back, and a shard-epoch raise that moves nothing, need a cluster: a single
// node raises no shard epoch, so the cluster adds those cases here.
func TestV25(t *testing.T) { overBoth(t, v25) }

func v25(t *testing.T, opts ...enginetest.Option) {
	t.Run("a commit over a failed window answers another verifier", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.File("f")
		w, err := c.write(t, c.admin, h, 0, []byte("in a failed window"))
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		c.FailNextWindow()
		if v, err := c.fs.Commit(bounded(t), c.admin, vfs.Anonymous(h), 0, 0); err != nil || v == w.Verifier {
			t.Errorf("the commit over a failed window: verifier %x, %v; want success, other than the write's %x", v, err, w.Verifier)
		}
	})

	t.Run("a stable write over a failed window is a delay", func(t *testing.T) {
		c := compose(t, opts...)
		h, _ := c.File("f")
		w, err := c.fs.Write(bounded(t), c.admin, vfs.Anonymous(h), 0, []byte("stable"), vfs.FileSync)
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		if v, err := c.fs.Commit(bounded(t), c.admin, vfs.Anonymous(h), 0, 0); err != nil || v != w.Verifier {
			t.Errorf("a stable write with no failed window answered verifier %x, the commit after it %x, %v; want the same", w.Verifier, v, err)
		}
		c.FailNextWindow()
		_, err = c.fs.Write(bounded(t), c.admin, vfs.Anonymous(h), 0, []byte("stable"), vfs.FileSync)
		if !errors.Is(err, &vfs.Delay{Cause: vfs.DelayCommit}) {
			t.Errorf("a stable write whose sync window failed: %v, want ErrDelay (DelayCommit)", err)
		}
	})

	t.Run("an unstable write dropped as lost changes the verifier", func(t *testing.T) {
		c := compose(t, opts...)
		h, id := c.File("f")
		w, err := c.write(t, c.admin, h, 0, []byte("lost"))
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		c.FailNextWindow()
		// The group commit's sync fails the window and drops the write once the
		// clock passes the existence age; the clock moves on each poll, so a
		// timer set after one move still comes due.
		for deadline := time.Now().Add(bound); ; time.Sleep(10 * time.Millisecond) {
			c.Clock.Advance(enginetest.Settings.ExistenceAge)
			if _, pending, err := c.E.Overlay(bounded(t), c.Share.ID, id); err == nil && !pending {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %v the group commit has not dropped the write", bound)
			}
		}
		if again, err := c.write(t, c.admin, h, 0, []byte("anew")); err != nil || again.Verifier == w.Verifier {
			t.Errorf("the next write in the same process: verifier %x, %v; want other than the lost write's %x", again.Verifier, err, w.Verifier)
		}
	})

	t.Run("an SMB flush through each open fails once across a loss", func(t *testing.T) {
		c := compose(t, opts...)
		c.File("f")
		pc, laptop := c.smb(t, "pc", c.FS.Admin.Principal), c.smb(t, "laptop", c.FS.Admin.Principal)
		opens := []struct {
			name string
			c    vfs.Call
			ref  vfs.OpenRef
		}{
			{"pc", pc, c.open(t, pc, "f", 0)},
			{"laptop", laptop, c.open(t, laptop, "f", 0)},
		}
		if _, err := c.fs.Write(bounded(t), pc, opens[0].ref, 0, []byte("lost"), vfs.Unstable); err != nil {
			t.Fatalf("the first write: %v", err)
		}
		c.FailNextWindow()
		for _, o := range opens {
			if _, err := c.fs.Commit(bounded(t), o.c, o.ref, 0, 0); !errors.Is(err, vfs.ErrLost) {
				t.Errorf("%s: the first flush across the loss: %v, want ErrLost", o.name, err)
			}
			if _, err := c.fs.Commit(bounded(t), o.c, o.ref, 0, 0); err != nil {
				t.Errorf("%s: the flush after it: %v, want success", o.name, err)
			}
		}
	})
}
