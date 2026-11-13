package vfs

import (
	"context"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/block/engine"
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

// copyOrClone runs RFC 17 §5.9's steps for op. Step 1: both handles are
// admitted, both files' admission locks taken in one Take, and the source
// judged for read data over its extents and the destination for a write
// over its own, each as a read or write through its reference is. Step 2: a
// clone of other than one extent, an extent no clone moves, one ending past
// the source's size, and a source whose share counts content in a namespace
// the destination's does not (ErrCross, CrossNamespace), are refused before
// anything changes. Step 3: the destination's charge is reserved. Step 4:
// the engine, which refuses at its admission what the filesystem cannot
// judge, a clone past clone_max_len and an extent a running clone holds, and
// then calls the admitted hook (engine.WithAdmitted) before it changes the
// destination: there setuid and setgid are cleared, as a write's are
// (staging), the WCC's Before read under the accept lock, and every lock
// released. From there the clone's registrations, not the locks, hold off
// what meets its extents, so no sync, phase 1 or wait for the clone runs
// under either lock, and a call on the destination outside the extents, or
// an open, a close or a lock of either file, does not wait for the clone. A
// removal in flight over an extent, one its loop retries, is answered
// DelayConflict at the engine's registration rather than waited out under
// both locks.
//
// decision: the two admission locks are held until the engine has
// registered the clone, not until its I/O ends. A lock or deny mode granted
// once they are released is still ordered after a clone's I/O (RFC 17 §4.8),
// since the registrations refuse every write and truncate of its extents
// until it ends, so it reads the source as it was before the grant. A copy's
// later chunks are judged with its first and registered only as each runs:
// a lock granted on a later chunk's range, or a deny mode, between the first
// chunk's registration and that chunk's is not ordered against it, and a
// truncate there ends the copy short, as §5.9 allows. Overturned by the
// engine calling back before each chunk, for the filesystem to judge it
// under the two admission locks again, once a copy's later chunks are seen
// racing grants.
//
// decision: the WCC's Before is read under the destination's accept lock
// before the engine changes it, and its After once the engine answers,
// under no lock, so a change a concurrent call makes outside the extents
// during the clone shows in the After, unlike V31's one hold. No adapter answers a copy's or
// clone's pre-operation attributes: NFSv4.2's COPY and CLONE and SMB's
// copychunk and duplicate-extents carry none. Overturned by an adapter that
// does, or RFC 17 holding the WCC of a call this long to one hold.
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
	sa, srelease, err := fs.admitRef(ctx, c, a.Src, false)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	defer srelease()
	da, drelease, err := fs.admitRef(ctx, c, a.Dst, false)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	defer drelease()
	held, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(fileOf(a.Src.Handle)), lockorder.Key(fileOf(a.Dst.Handle))}})
	if err != nil {
		return 0, vfs.WCC{}, errContended
	}
	unlock := sync.OnceFunc(held)
	defer unlock()
	sf, err := fs.judge(ctx, sa, a.Src, op, src.Offset, src.Length, false)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	df, err := fs.judge(ctx, da, a.Dst, op, dst.Offset, dst.Length, true)
	if err != nil {
		return 0, vfs.WCC{}, err
	}
	for _, x := range a.Extents {
		if badExtent(x, sf.ID == df.ID) || x.SrcOff+x.Len > sf.Size {
			return 0, vfs.WCC{}, vfs.ErrInvalid
		}
	}
	if !sameNamespaces(sa.share, da.share) {
		return 0, vfs.WCC{}, &vfs.CrossError{Kind: vfs.CrossNamespace}
	}
	if len(a.Extents) == 0 { // a copy of nothing returns once judged, as a write of no byte does
		return 0, vfs.WCC{Before: &df, After: df}, nil
	}
	var add int64
	for _, x := range a.Extents {
		n, err := fs.charges(ctx, df, x.DstOff, x.Len)
		if err != nil {
			return 0, vfs.WCC{}, err
		}
		add += n
	}
	var res *quota.Reservation
	if res, err = fs.reserve(ctx, da, charged(df), entity.Usage{Bytes: add}); err != nil {
		return 0, vfs.WCC{}, err
	}
	// decision: the reservation is released once the engine answers. A
	// clone, and each chunk of a copy, charges its whole destination extent
	// in its phase 1's own transaction before it is answered done, so a call
	// answered done is charged by then. One answered at its deadline leaves
	// its clone, or its copy's running chunk, to run on unreserved: until
	// that phase 1 commits a quota may be overshot by its extent, past the
	// concurrent commits RFC 17 §5.6 bounds the overshoot at. Holding it
	// until the clone ends needs the engine to say when it does, which it
	// does not. It reserves every byte an extent lands in a destination hole
	// or past its size, a source hole's and a later extent's overwrite of an
	// earlier one's included, so it may refuse ErrQuota a copy that would
	// have fit. Overturned by the engine's Clone and Copy taking a Done.
	defer res.Release()
	if flagsRefuse(df, dst.Offset, true) {
		return 0, vfs.WCC{}, vfs.ErrPerm
	}
	var before *vfs.File
	admitted := func() error {
		if before != nil { // a copy's later chunk: the first one's hook ran
			return nil
		}
		b, unlockAccept, err := fs.staging(ctx, da, a.Dst.Handle, df, dst.Offset, true)
		if err != nil {
			return err
		}
		unlockAccept()
		unlock()
		before = &b
		return nil
	}
	ectx := engine.WithAdmitted(ctx, admitted)
	var n int64
	if op == vfs.OpClone {
		err = fs.engine.Clone(ectx, sf.Share, sf.ID, df.Share, df.ID, a.Extents[0])
	} else {
		n, err = fs.engine.Copy(ectx, sf.Share, sf.ID, df.Share, df.ID, a.Extents)
	}
	unlock()
	if err != nil {
		return 0, vfs.WCC{}, storeErr(err)
	}
	if before == nil { // a retry of a clone done, answered before the hook: nothing changed now
		before = &df
	}
	w := fs.wcc(ctx, c, da, a.Dst.Handle, *before)
	fs.entryChanged(ctx, da, &a.Dst, df, written(w))
	return n, w, nil
}

// sameNamespaces reports whether dst counts content in every namespace src
// does, as RFC 8 §9.1's admission asks of a clone between their files.
func sameNamespaces(src, dst entity.Share) bool {
	for _, g := range src.Namespaces {
		if !slices.ContainsFunc(dst.Namespaces, func(d entity.NamespaceGen) bool { return d.Namespace == g.Namespace }) {
			return false
		}
	}
	return true
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
