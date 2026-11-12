package vfs

import (
	"context"

	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Copy copies a's extents of its source into its destination, in order, as
// the engine's bounded chunks, and answers the bytes copied with the
// destination's WCC. At the caller's deadline, or once a chunk after the
// first is refused, it answers the chunks done, a short count; only the
// first chunk's refusal is its error.
func (fs *Filesystem) Copy(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.CopyResult, error) {
	n, w, err := fs.copyOrClone(ctx, c, a, vfs.OpCopy)
	return vfs.CopyResult{N: n, WCC: w}, err
}

// Clone makes a's one extent of its destination the content of that of its
// source, atomically, answered once done, with the destination's WCC.
func (fs *Filesystem) Clone(ctx context.Context, c vfs.Call, a vfs.CopyArgs) (vfs.WCC, error) {
	_, w, err := fs.copyOrClone(ctx, c, a, vfs.OpClone)
	return w, err
}

// copyOrClone runs RFC 17 §5.9's steps for op. Step 1: the source is
// admitted and judged for read data over its extents, and the destination
// for a write over its own, each as a read or write through its reference
// is. Step 2: a clone of other than one extent, an extent no clone moves,
// and one ending past the source's size, read under the source's admission
// lock, are ErrInvalid before anything changes. Step 3: the destination's
// charge is reserved, and its flags and setuid and setgid handled as a
// write's are (stage). Step 4: the engine, under the destination's accept
// lock, which answers the WCC from that one hold. The source's admission
// lock is released before the destination's is taken, so the two are never
// held together and no lock order binds them; a source truncated between
// the two ends a copy short, as the engine answers it.
//
// decision: the refusals the engine answers at its admission, a clone past
// clone_max_len (ErrInvalid) and two write namespaces (ErrCross,
// CrossNamespace), come after step 3, as the filesystem holds neither the
// setting nor a share's namespace: such a refused call may leave setuid and
// setgid cleared on the destination, as a write the engine refuses does,
// and changes nothing else. Overturned by the filesystem holding both.
//
// decision: an incomplete source is not refused (ErrIncomplete), as no File
// record carries the mark yet: nothing restores a snapshot. Overturned when
// snapshots (RFC 12) mark a file incomplete.
func (fs *Filesystem) copyOrClone(ctx context.Context, c vfs.Call, a vfs.CopyArgs, op vfs.Op) (int64, vfs.WCC, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	defer done()
	if op == vfs.OpClone && len(a.Extents) != 1 {
		return 0, vfs.WCC{}, vfs.ErrInvalid
	}
	src, dst := spans(a.Extents)
	_, sf, sunlock, srelease, err := fs.io(ctx, c, a.Src, op, src.Offset, src.Length, false)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	defer srelease()
	sunlock()
	ad, df, unlock, release, err := fs.io(ctx, c, a.Dst, op, dst.Offset, dst.Length, true)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	defer release()
	for _, x := range a.Extents {
		if badExtent(x, sf.ID == df.ID) || x.SrcOff+x.Len > sf.Size {
			unlock()
			return 0, vfs.WCC{}, vfs.ErrInvalid
		}
	}
	if len(a.Extents) == 0 { // a copy of nothing returns once judged, as a write of no byte does
		unlock()
		return 0, vfs.WCC{Before: &df, After: df}, nil
	}
	var add int64
	for _, x := range a.Extents {
		n, err := fs.charges(ctx, df, x.DstOff, x.Len)
		if err != nil {
			unlock()
			return 0, vfs.WCC{}, err
		}
		add += n
	}
	var res *quota.Reservation
	if res, err = fs.reserve(ctx, ad, charged(df), entity.Usage{Bytes: add}); err != nil {
		unlock()
		return 0, vfs.WCC{}, err
	}
	// decision: the reservation is released once the engine answers, not
	// when the destination's charge commits: the engine's Clone takes no
	// Done, and the runs it copies through the journal are charged at the
	// destination's next existence commit, so until then a quota may be
	// overshot by those bytes, and a copy's chunk left running past its
	// deadline is not reserved at all. It reserves every byte an extent
	// lands in a destination hole or past its size, a source hole's and a
	// later extent's overwrite of an earlier one's included, so it may
	// refuse ErrQuota a copy that would have fit. Overturned by the engine's
	// Clone and Copy taking a Done.
	defer res.Release()
	var n int64
	w, err := fs.stage(ctx, c, ad, a.Dst.Handle, df, dst.Offset, true, func() (err error) {
		if op == vfs.OpClone {
			return fs.engine.Clone(ctx, sf.Share, sf.ID, df.Share, df.ID, a.Extents[0])
		}
		n, err = fs.engine.Copy(ctx, sf.Share, sf.ID, df.Share, df.ID, a.Extents)
		return err
	})
	unlock()
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	fs.entryChanged(ctx, ad, &a.Dst, df, written(w))
	return n, w, nil
}

// spans are the smallest ranges holding every source and every destination
// extent of xs, which open state judges against locks and deny modes;
// empty for no extent, or for any extent no clone moves, refused after.
//
// ponytail: a copy of extents far apart is judged over the whole range
// between them, so a lock held in a gap refuses it. Judge each extent once
// a copychunk's sparse chunks are seen refused by locks between them.
func spans(xs []vfs.Extent) (src, dst entity.Extent) {
	for i, x := range xs {
		if badExtent(x, false) {
			return entity.Extent{}, entity.Extent{}
		}
		s, d := entity.Extent{Offset: x.SrcOff, Length: x.Len}, entity.Extent{Offset: x.DstOff, Length: x.Len}
		if i > 0 {
			s, d = cover(src, s), cover(dst, d)
		}
		src, dst = s, d
	}
	return src, dst
}

// cover is the smallest extent holding a and b.
func cover(a, b entity.Extent) entity.Extent {
	lo := min(a.Offset, b.Offset)
	return entity.Extent{Offset: lo, Length: max(a.Offset+a.Length, b.Offset+b.Length) - lo}
}

// badExtent reports whether x is an extent no clone moves: empty, at a
// negative offset, ending past the largest offset, or, oneFile when source
// and destination are one file, overlapping itself, as Linux and RFC 7862
// refuse it.
func badExtent(x vfs.Extent, oneFile bool) bool {
	return x.Len <= 0 || x.SrcOff < 0 || x.DstOff < 0 || x.Len > entity.EndOfFile-max(x.SrcOff, x.DstOff) ||
		oneFile && x.SrcOff < x.DstOff+x.Len && x.DstOff < x.SrcOff+x.Len
}
