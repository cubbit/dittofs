//go:build engine_conformance

package integration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// copyChunk is the engine's copy chunk under the fixture's clone_max_len: it
// must match copyChunk in pkg/block/engine/clone.go.
const copyChunk = 64 << 20

// backstop is a generous real-time bound on a wait that must end, so a wrong
// engine fails on its assertion instead of hanging the package.
const backstop = 10 * time.Second

// readAt is what the file h names holds over [off, off+n), read through the
// service as the administrator, and the read's error.
func (c *composition) readAt(t *testing.T, h vfs.Handle, off int64, n int) ([]byte, error) {
	t.Helper()
	p := make([]byte, n)
	got, _, err := c.fs.Read(t.Context(), c.admin, vfs.Anonymous(h), off, p)
	return p[:got], err
}

// written creates name in the share's root, owned by owner with mode, and
// writes p at 0 as the administrator.
func (c *composition) written(t *testing.T, name string, owner entity.Principal, mode uint32, p []byte) vfs.Handle {
	t.Helper()
	h, _ := c.owned(t, name, owner, mode)
	if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(h), 0, p, vfs.Unstable); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	return h
}

// copyArgs is a copy or clone of xs from src to dst.
func copyArgs(src, dst vfs.Handle, xs ...vfs.Extent) vfs.CopyArgs {
	return vfs.CopyArgs{Src: vfs.Anonymous(src), Dst: vfs.Anonymous(dst), Extents: xs}
}

// RFC 17 V22, its copy and clone cases: an unprivileged caller's copy or
// clone into a setuid or setgid file clears the bits in a committed change
// before the engine changes the destination.
func TestV22CopyAndClone(t *testing.T) { overBoth(t, v22CopyAndClone) }

func v22CopyAndClone(t *testing.T, opts ...enginetest.Option) {
	for _, tc := range []struct {
		name string
		do   func(ctx context.Context, c *composition, call vfs.Call, a vfs.CopyArgs) error
	}{
		{"a copy by bob", func(ctx context.Context, c *composition, call vfs.Call, a vfs.CopyArgs) error {
			_, err := c.fs.Copy(ctx, call, a)
			return err
		}},
		{"a clone by bob", func(ctx context.Context, c *composition, call vfs.Call, a vfs.CopyArgs) error {
			_, err := c.fs.Clone(ctx, call, a)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compose(t, opts...)
			src := c.written(t, "src", c.alice.Principal, 0o644, []byte("copy"))
			dst := c.written(t, "dst", c.alice.Principal, 0o6777, []byte("data"))
			ctx, cancel := context.WithTimeout(t.Context(), backstop)
			defer cancel()
			if err := tc.do(ctx, c, signedIn(c.bob.Principal), copyArgs(src, dst, vfs.Extent{Len: 4})); err != nil {
				t.Fatalf("%s into alice's setuid file: %v", tc.name, err)
			}
			if f, err := c.fs.GetAttr(t.Context(), c.admin, dst); err != nil || f.Mode != 0o777 {
				t.Errorf("GetAttr after %s: mode %o, %v; want 777", tc.name, f.Mode, err)
			}
			if m := c.Committed(dst).Mode; m != 0o777 {
				t.Errorf("the store's mode after %s %o, want 777 committed", tc.name, m)
			}
			if got, err := c.readAt(t, dst, 0, 4); err != nil || !bytes.Equal(got, []byte("copy")) {
				t.Errorf("the destination after %s reads %q, %v; want %q", tc.name, got, err, "copy")
			}
			c.forgetClones(t, dst, 0)
		})
	}
}

// RFC 17 V26, adapted: a copy or clone refused for overlap, a range past the
// source's size, a clone over clone_max_len or a namespace crossing changes
// nothing; a copy whose deadline passes partway answers a short count, and
// the chunk it was in completes past it; a Lost source run is cloned as Lost.
// An Incomplete source is RFC 12's, which adds that refusal here; a shard
// crossing and a copy across shards are the cluster's, which adds them here.
func TestV26(t *testing.T) { overBoth(t, v26) }

func v26(t *testing.T, opts ...enginetest.Option) {
	t.Run("a refusal changes nothing", func(t *testing.T) {
		source, data := []byte("source bytes"), []byte("destination!")
		for _, tc := range []struct {
			name string
			// do refuses with the composition's files: src holds source, dst
			// holds data, and big is a sparse file one byte longer than
			// clone_max_len.
			do   func(ctx context.Context, t *testing.T, c *composition, src, dst, big vfs.Handle) error
			want func(error) bool
		}{
			{"a same-file clone whose extents overlap", func(ctx context.Context, t *testing.T, c *composition, _, dst, _ vfs.Handle) error {
				_, err := c.fs.Clone(ctx, c.admin, copyArgs(dst, dst, vfs.Extent{SrcOff: 0, DstOff: 2, Len: 4}))
				return err
			}, invalid},
			{"a same-file copy whose extents overlap", func(ctx context.Context, t *testing.T, c *composition, _, dst, _ vfs.Handle) error {
				_, err := c.fs.Copy(ctx, c.admin, copyArgs(dst, dst, vfs.Extent{SrcOff: 0, DstOff: 2, Len: 4}))
				return err
			}, invalid},
			{"a copy ending one byte past the source's size", func(ctx context.Context, t *testing.T, c *composition, src, dst, _ vfs.Handle) error {
				_, err := c.fs.Copy(ctx, c.admin, copyArgs(src, dst, vfs.Extent{Len: int64(len(source)) + 1}))
				return err
			}, invalid},
			{"a clone one byte over clone_max_len", func(ctx context.Context, t *testing.T, c *composition, _, dst, big vfs.Handle) error {
				_, err := c.fs.Clone(ctx, c.admin, copyArgs(big, dst, vfs.Extent{Len: enginetest.Settings.CloneMaxLen + 1}))
				return err
			}, invalid},
			{"a clone into a share of another namespace", func(ctx context.Context, t *testing.T, c *composition, src, _, _ vfs.Handle) error {
				other := c.Second
				other.Namespace = entity.NewNamespaceID()
				if err := c.E.Bind(t.Context(), other); err != nil {
					t.Fatalf("bind the second share in another namespace: %v", err)
				}
				// The destination is g, in the second share, holding data.
				g, _ := c.FS.Create(t, c.FS.Admin, c.SecondRoot, "g", kvstore.Regular(0o644))
				if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(g), 0, data, vfs.Unstable); err != nil {
					t.Fatal(err)
				}
				_, err := c.fs.Clone(ctx, c.admin, copyArgs(src, g, vfs.Extent{Len: 4}))
				if got, rerr := c.readAt(t, g, 0, len(data)+1); rerr != nil || !bytes.Equal(got, data) {
					t.Errorf("g after the clone across namespaces reads %q, %v; want %q", got, rerr, data)
				}
				return err
			}, func(err error) bool {
				var x *vfs.CrossError
				return errors.As(err, &x) && x.Kind == vfs.CrossNamespace
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := compose(t, opts...)
				src := c.written(t, "src", c.FS.Admin.Principal, 0o644, source)
				dst := c.written(t, "dst", c.FS.Admin.Principal, 0o644, data)
				big, _ := c.owned(t, "big", c.FS.Admin.Principal, 0o644)
				grow := vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(enginetest.Settings.CloneMaxLen + 1)}
				if _, err := c.fs.SetAttr(t.Context(), c.admin, vfs.Target{H: big}, grow); err != nil {
					t.Fatal(err)
				}
				// Bounded, so a refusal the engine misses fails here rather
				// than holding the test behind a clone that cannot finish.
				ctx, cancel := context.WithTimeout(t.Context(), backstop)
				defer cancel()
				if err := tc.do(ctx, t, c, src, dst, big); !tc.want(err) {
					t.Errorf("%s: %v, want it refused", tc.name, err)
				}
				if got, err := c.readAt(t, dst, 0, len(data)+1); err != nil || !bytes.Equal(got, data) {
					t.Errorf("the destination after %s reads %q, %v; want %q", tc.name, got, err, data)
				}
			})
		}
	})

	t.Run("a copy past its deadline is short and its running chunk completes", func(t *testing.T) {
		c := compose(t, opts...)
		const size = 200 << 20
		// Each file holds a marker at the start and the end of each chunk,
		// and holes between, so the share's small journal holds it.
		marks := []int64{0, copyChunk - 4, copyChunk, 2*copyChunk - 4, 2 * copyChunk, 3 * copyChunk, size - 4}
		src, _ := c.owned(t, "src", c.FS.Admin.Principal, 0o644)
		dst, _ := c.owned(t, "dst", c.FS.Admin.Principal, 0o644)
		for i, at := range marks {
			if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(src), at, []byte{'s', 'r', 'c', byte(i)}, vfs.Unstable); err != nil {
				t.Fatalf("the first write: %v", err)
			}
			if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(dst), at, []byte("dst!"), vfs.Unstable); err != nil {
				t.Fatal(err)
			}
		}
		// holds reports whether dst holds the source's markers below copied
		// and its own from on; a read of a chunk still running into dst is
		// refused, so the markers between the two are not read.
		holds := func(copied, from int64) bool {
			for i, at := range marks {
				want := []byte("dst!")
				switch {
				case at < copied:
					want = []byte{'s', 'r', 'c', byte(i)}
				case at < from:
					continue
				}
				if got, err := c.readAt(t, dst, at, 4); err != nil || !bytes.Equal(got, want) {
					return false
				}
			}
			return true
		}

		first, releaseFirst := c.HoldNextClone()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type answer struct {
			res vfs.CopyResult
			err error
		}
		answered := make(chan answer, 1)
		go func() {
			res, err := c.fs.Copy(ctx, c.admin, copyArgs(src, dst, vfs.Extent{Len: size}))
			answered <- answer{res, err}
		}()
		select {
		case <-first:
		case a := <-answered:
			t.Fatalf("the copy answered %d, %v before its first chunk reached the store", a.res.N, a.err)
		case <-time.After(backstop):
			t.Fatalf("the copy's first chunk did not reach the store within %v", backstop)
		}
		second, releaseSecond := c.HoldNextClone()
		releaseFirst()
		select {
		case <-second:
		case a := <-answered:
			t.Fatalf("the copy answered %d, %v before its second chunk reached the store", a.res.N, a.err)
		case <-time.After(backstop):
			t.Fatalf("the copy's second chunk did not reach the store within %v", backstop)
		}
		// The deadline passes while the second chunk runs.
		cancel()
		var a answer
		select {
		case a = <-answered:
		case <-time.After(backstop):
			t.Fatalf("the copy did not answer within %v of its deadline", backstop)
		}
		if a.err != nil || a.res.N != copyChunk {
			t.Fatalf("the copy whose deadline passed in its second chunk: %d, %v; want %d, nil", a.res.N, a.err, copyChunk)
		}
		if !holds(copyChunk, 2*copyChunk) {
			t.Error("at the answer the destination does not hold exactly the first chunk's bytes")
		}
		releaseSecond()
		// The chunk the deadline met completes past the count.
		for deadline := time.Now().Add(backstop); !holds(2*copyChunk, 2*copyChunk); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("after %v the second chunk has not completed past the count", backstop)
			}
		}
		rest := vfs.Extent{SrcOff: copyChunk, DstOff: copyChunk, Len: size - copyChunk}
		restCtx, restCancel := context.WithTimeout(t.Context(), backstop)
		defer restCancel()
		res, err := c.fs.Copy(restCtx, c.admin, copyArgs(src, dst, rest))
		if err != nil || res.N != rest.Len {
			t.Fatalf("the copy continued from the count: %d, %v; want %d", res.N, err, rest.Len)
		}
		if !holds(size, size) {
			t.Error("after the continuing copy the destination does not hold the source")
		}
		c.forgetClones(t, dst, 0, copyChunk, 2*copyChunk, 3*copyChunk)
	})

	if len(opts) > 0 {
		return // the loss is a byte flipped in a segment's durable image, which only the journal keeps
	}
	t.Run("a clone of a lost run reads lost there", func(t *testing.T) {
		c := compose(t)
		const part = 64 << 10
		parts := [][]byte{bytes.Repeat([]byte{'a'}, part), bytes.Repeat([]byte("lost"), part/4), bytes.Repeat([]byte{'c'}, part)}
		src, _ := c.owned(t, "src", c.FS.Admin.Principal, 0o644)
		for i, p := range parts {
			if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(src), int64(i*part), p, vfs.Unstable); err != nil {
				t.Fatalf("the first write: %v", err)
			}
		}
		loseRun(t, c, src, parts[1])
		dst, _ := c.owned(t, "dst", c.FS.Admin.Principal, 0o644)
		ctx, cancel := context.WithTimeout(t.Context(), backstop)
		defer cancel()
		if _, err := c.fs.Clone(ctx, c.admin, copyArgs(src, dst, vfs.Extent{Len: 3 * part})); err != nil {
			t.Fatalf("the clone of a source with a lost run: %v", err)
		}
		for _, i := range []int{0, 2} {
			if got, err := c.readAt(t, dst, int64(i*part), part); err != nil || !bytes.Equal(got, parts[i]) {
				t.Errorf("the destination's part %d reads %d bytes, %v; want the source's", i, len(got), err)
			}
		}
		if _, err := c.readAt(t, dst, part, part); !errors.Is(err, vfs.ErrLost) {
			t.Errorf("the destination over the source's lost run: %v, want ErrLost", err)
		}
	})
}

// invalid reports whether err is ErrInvalid.
func invalid(err error) bool { return errors.Is(err, vfs.ErrInvalid) }

// loseRun makes the journal lose src's run p: the file is synced, a byte of
// p's record flipped in the segment's durable image, and the process
// crashed, so the restart replays the loss over the synced run.
func loseRun(t *testing.T, c *composition, src vfs.Handle, p []byte) {
	t.Helper()
	if _, err := c.fs.Commit(t.Context(), c.admin, vfs.Anonymous(src), 0, 0); err != nil {
		t.Fatal(err)
	}
	names, err := c.Seam.List()
	if err != nil {
		t.Fatal(err)
	}
	flipped := false
	for _, name := range names {
		image, err := c.Seam.Durable(name)
		if err != nil {
			continue
		}
		at := bytes.Index(image, p)
		if at < 0 {
			continue
		}
		f, err := c.Seam.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{^image[at]}, int64(at)); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		flipped = true
	}
	if !flipped {
		t.Fatal("the journal's segments hold no record of the run")
	}
	c.Crash()
}

// A clone the engine or the store would refuse is refused before setuid and
// setgid are cleared: bob's clone into alice's 06777 file one byte over
// clone_max_len, from a share bound in another namespace, and between two
// shares bound as the store makes them, each in a namespace of its own,
// leaves the mode and the bytes as they were. Over the fake journal alone,
// as the journal stops at the first write.
func TestACloneRefusedLeavesSetIDAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// src is the clone's source, made as bob may read it.
		src  func(t *testing.T, c *composition) vfs.Handle
		ext  vfs.Extent
		want func(error) bool
	}{
		{"one byte over clone_max_len", func(t *testing.T, c *composition) vfs.Handle {
			big, _ := c.owned(t, "big", c.FS.Admin.Principal, 0o644)
			grow := vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(enginetest.Settings.CloneMaxLen + 1)}
			if _, err := c.fs.SetAttr(t.Context(), c.admin, vfs.Target{H: big}, grow); err != nil {
				t.Fatal(err)
			}
			return big
		}, vfs.Extent{Len: enginetest.Settings.CloneMaxLen + 1}, invalid},
		{"from a share bound in another namespace", func(t *testing.T, c *composition) vfs.Handle {
			other := c.Second
			other.Namespace = entity.NewNamespaceID()
			return c.secondSource(t, other)
		}, vfs.Extent{Len: 4}, crossNamespace},
		{"between two shares as the store makes them", func(t *testing.T, c *composition) vfs.Handle {
			return c.secondSource(t, c.Second)
		}, vfs.Extent{Len: 4}, crossNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compose(t, enginetest.WithFakeJournal())
			dst := c.written(t, "dst", c.alice.Principal, 0o6777, []byte("data"))
			src := tc.src(t, c)
			ctx, cancel := context.WithTimeout(t.Context(), backstop)
			defer cancel()
			if _, err := c.fs.Clone(ctx, signedIn(c.bob.Principal), copyArgs(src, dst, tc.ext)); !tc.want(err) {
				t.Errorf("bob's clone %s: %v, want it refused", tc.name, err)
			}
			if m := c.Committed(dst).Mode; m != 0o6777 {
				t.Errorf("the destination's mode after the refused clone %o, want 6777", m)
			}
			if got, err := c.readAt(t, dst, 0, 5); err != nil || !bytes.Equal(got, []byte("data")) {
				t.Errorf("the destination after the refused clone reads %q, %v; want %q", got, err, "data")
			}
		})
	}
}

// secondSource binds the second share as b, grants bob read and write on it,
// and answers a file there holding "copy".
func (c *composition) secondSource(t *testing.T, b engine.Share) vfs.Handle {
	t.Helper()
	if err := c.E.Bind(t.Context(), b); err != nil {
		t.Fatalf("bind the second share: %v", err)
	}
	if err := c.FS.Store.ControlPlane().SetGrant(t.Context(), entity.ShareGrant{Share: c.Second.ID, Principal: c.bob.Principal, Access: entity.AccessReadWrite}); err != nil {
		t.Fatal(err)
	}
	g, _ := c.FS.Create(t, c.FS.Admin, c.SecondRoot, "g", kvstore.Regular(0o644))
	if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(g), 0, []byte("copy"), vfs.Unstable); err != nil {
		t.Fatal(err)
	}
	return g
}

// crossNamespace reports whether err is ErrCross with CrossNamespace.
func crossNamespace(err error) bool {
	var x *vfs.CrossError
	return errors.As(err, &x) && x.Kind == vfs.CrossNamespace
}

// While a clone runs, held at its phase 1, it holds none of the filesystem's
// locks: a read and a write of the destination outside its extent, an open
// of it by name, a lock and a close, and a lock on the source's extent, each
// answer at once; a read or a write inside the destination's extent, and a
// write inside the source's, meet the clone's registrations and answer
// DelayClone at once, there being no held wait (WithHold) yet. Over the fake
// journal alone, as the journal stops at the first write.
func TestARunningCloneHoldsNoLockOfItsFiles(t *testing.T) {
	c := compose(t, enginetest.WithFakeJournal())
	src := c.written(t, "src", c.FS.Admin.Principal, 0o644, []byte("abcdefgh"))
	dst := c.written(t, "dst", c.FS.Admin.Principal, 0o644, []byte("01234567"))
	entered, release := c.HoldNextClone()
	cloned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backstop)
		defer cancel()
		_, err := c.fs.Clone(ctx, c.admin, copyArgs(src, dst, vfs.Extent{Len: 4}))
		cloned <- err
	}()
	select {
	case <-entered:
	case err := <-cloned:
		t.Fatalf("the clone answered %v before its phase 1 reached the store", err)
	case <-time.After(backstop):
		t.Fatalf("the clone's phase 1 did not reach the store within %v", backstop)
	}
	// Each call gets a deadline well inside the backstop: one waiting on a
	// lock the clone holds answers DelayConflict at it, not its own answer.
	const short = 2 * time.Second
	call := func(name string, want error, do func(ctx context.Context) error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), short)
		defer cancel()
		if err := do(ctx); want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Errorf("%s during the clone: %v, want %v", name, err, want)
		}
	}
	delayClone := &vfs.Delay{Cause: vfs.DelayClone}
	read := func(off int64) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, _, err := c.fs.Read(ctx, c.admin, vfs.Anonymous(dst), off, make([]byte, 4))
			return err
		}
	}
	write := func(h vfs.Handle, off int64) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := c.fs.Write(ctx, c.admin, vfs.Anonymous(h), off, []byte("wxyz"), vfs.Unstable)
			return err
		}
	}
	call("a read outside the extent", nil, read(4))
	call("a write outside the extent", nil, write(dst, 4))
	call("a read inside the extent", delayClone, read(0))
	call("a write inside the extent", delayClone, write(dst, 0))
	call("a write inside the source's extent", delayClone, write(src, 0))
	pc := c.smb(t, "pc", c.FS.Admin.Principal)
	var ref vfs.OpenRef
	call("an open of the destination by name", nil, func(ctx context.Context) error {
		r, err := c.fs.Open(ctx, pc, vfs.OpenRequest{Dir: c.FS.Root, Name: []byte("dst"), Disposition: vfs.OpenExisting,
			Access: entity.ReadData | entity.WriteData, Timeout: time.Minute})
		ref = r.Ref
		return err
	})
	lock := func(ref vfs.OpenRef, name string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			o := vfs.LockOwner{Client: pc.Client, Name: []byte(name), Stack: true}
			_, err := c.fs.Lock(ctx, pc, ref, vfs.LockReq{Owner: o, Ranges: []vfs.LockRange{{ByteRange: vfs.ByteRange{Offset: 0, Length: 4}, Exclusive: true}}})
			return err
		}
	}
	call("a lock of the destination's extent", nil, lock(ref, "dst"))
	call("a lock of the source's extent", nil, lock(vfs.Anonymous(src), "src"))
	call("a close of the destination", nil, func(ctx context.Context) error {
		_, err := c.fs.Close(ctx, pc, ref, vfs.Seqs{})
		return err
	})
	select {
	case err := <-cloned:
		t.Fatalf("the clone answered %v before its release: a call above waited until the engine gave up holding it", err)
	default:
	}
	release()
	select {
	case err := <-cloned:
		if err != nil {
			t.Fatalf("the clone once released: %v", err)
		}
	case <-time.After(backstop):
		t.Fatalf("the clone did not answer within %v of its release", backstop)
	}
	if got, err := c.readAt(t, dst, 0, 8); err != nil || !bytes.Equal(got, []byte("abcdwxyz")) {
		t.Errorf("the destination after the clone reads %q, %v; want %q", got, err, "abcdwxyz")
	}
	c.forgetClones(t, dst, 0)
}

// forgetClones writes back the byte the file h names holds at each of offs,
// each inside the destination extent of a clone done there: a write over a
// done clone's extent drops the engine's memory of it, which otherwise keeps
// the file's entry live and holds the engine's Close, at the fixture's stop,
// for its whole stop bound. Drop it once Close no longer waits on a
// remembered clone.
func (c *composition) forgetClones(t *testing.T, h vfs.Handle, offs ...int64) {
	t.Helper()
	for _, off := range offs {
		b, err := c.readAt(t, h, off, 1)
		if err != nil || len(b) != 1 {
			t.Fatalf("read the byte at %d to write back: %q, %v", off, b, err)
		}
		if _, err := c.fs.Write(t.Context(), c.admin, vfs.Anonymous(h), off, b, vfs.Unstable); err != nil {
			t.Fatalf("write back the byte at %d: %v", off, err)
		}
	}
}
