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
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// copyChunk is the engine's copy chunk under the fixture's clone_max_len.
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
func TestV22CopyAndClone(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(c *composition, call vfs.Call, a vfs.CopyArgs) error
	}{
		{"a copy by bob", func(c *composition, call vfs.Call, a vfs.CopyArgs) error {
			_, err := c.fs.Copy(context.Background(), call, a)
			return err
		}},
		{"a clone by bob", func(c *composition, call vfs.Call, a vfs.CopyArgs) error {
			_, err := c.fs.Clone(context.Background(), call, a)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compose(t)
			src := c.written(t, "src", c.alice.Principal, 0o644, []byte("copy"))
			dst := c.written(t, "dst", c.alice.Principal, 0o6777, []byte("data"))
			if err := tc.do(c, signedIn(c.bob.Principal), copyArgs(src, dst, vfs.Extent{Len: 4})); err != nil {
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
		})
	}
}

// RFC 17 V26, adapted: a copy or clone refused for overlap, a range past the
// source's size, a clone over clone_max_len or a namespace crossing changes
// nothing; a copy whose deadline passes partway answers a short count, and
// the chunk it was in completes past it; a Lost source run is cloned as Lost.
// An Incomplete source is RFC 12's, which adds that refusal here; a shard
// crossing and a copy across shards are the cluster's, which adds them here.
func TestV26(t *testing.T) {
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
				c := compose(t)
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
		c := compose(t)
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
		}
		second, releaseSecond := c.HoldNextClone()
		releaseFirst()
		select {
		case <-second:
		case a := <-answered:
			t.Fatalf("the copy answered %d, %v before its second chunk reached the store", a.res.N, a.err)
		}
		// The deadline passes while the second chunk runs.
		cancel()
		a := <-answered
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
		res, err := c.fs.Copy(t.Context(), c.admin, copyArgs(src, dst, rest))
		if err != nil || res.N != rest.Len {
			t.Fatalf("the copy continued from the count: %d, %v; want %d", res.N, err, rest.Len)
		}
		if !holds(size, size) {
			t.Error("after the continuing copy the destination does not hold the source")
		}
	})

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
		if _, err := c.fs.Clone(t.Context(), c.admin, copyArgs(src, dst, vfs.Extent{Len: 3 * part})); err != nil {
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
