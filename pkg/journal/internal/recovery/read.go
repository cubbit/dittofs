// Package recovery rebuilds the journal's state at open. Its attach pass
// lists the directory once, unlinks the spare temporaries, classifies every
// segment header and reports the id high-water mark; every read it makes is
// bounded by internal/readretry, and each byte range no bounded read
// completed is collected here for the open to refuse on.
package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// reader is the open's one reach into its directory: every read the open
// makes goes through it, so a read the device cannot complete is retried by
// the bound and, when no attempt reads, collected rather than judged. It
// belongs to one open and is not safe for concurrent use.
//
// A format file's failed read is the caller's to answer, never the
// reader's: ReadFormat runs under the same bound and knows the one file it
// asked for, so it judges the failure on the read's own error rather than
// consulting a flag here.
type reader struct {
	ctx context.Context
	fs  storage.FS
	clk core.Clock
	log *slog.Logger

	// unread holds each segment range no bounded read completed.
	unread []core.SegmentRange
}

// newReader returns the open's reader over fs, running under ctx and on
// clk. log takes the open's logger; nil discards.
func newReader(ctx context.Context, fs storage.FS, clk core.Clock, log *slog.Logger) *reader {
	return &reader{ctx: ctx, fs: fs, clk: clk, log: log}
}

// warnUnreadable warns of the error that exhausted the read of name, the
// last one the bound made, before the range is collected: a refusal the
// open raises on a read its own logs never name gives an operator nothing
// to steer a repair by. The warning names the file and the error, never a
// verdict — the collected range is the open's to answer with.
func (r *reader) warnUnreadable(name string, err error) {
	if r.log == nil {
		return
	}
	r.log.Warn("journal open: a read its bound could not complete",
		"name", name, "err", err)
}

// ReadAt reads len(buf) bytes of the file name at offset off. A read that
// ends in io.EOF completed to the file's own end and is passed through: the
// bytes a short file does not hold are the file's length, not a fault of the
// device. Any other error the bound's attempts did not clear is collected —
// the file as the format's, a segment's bytes as their range — and returned,
// except when the open's own context ended the retries: that says nothing
// about the device, so it is only returned.
//
// ponytail: every attempt opens the file and closes it again, so a scan's
// per-record header reads each pay an open and a close. Upgrade to one
// handle per segment per scan, kept open across its reads and reopened by
// the bound only after a failure, when a profile of the open shows the
// per-read opens.
func (r *reader) ReadAt(name string, buf []byte, off int64) (n int, err error) {
	// decision: an I/O error suspends, a bad checksum discards. A failed
	// read returned no bytes, so the device may return them later or after
	// a repair; a record that does not verify returned wrong bytes, and no
	// retry fixes them. The failed read is therefore retried here and,
	// when no attempt reads, collected below — never classed as zeros,
	// torn or corrupt. Revisit if a device is ever seen returning an I/O
	// error for a torn write.
	//
	// decision: no timer retries a failed read; only this bound starts
	// another attempt, and nothing waits out the fault. A read blocked in
	// the kernel cannot be cancelled, and the kernel has already retried
	// before returning the error, so the window bounds starting an
	// attempt, not the total: with attempts of similar length the total
	// stays under about twice it.
	endOfFile := false
	_, err = readretry.Do(r.ctx, r.clk, readretry.Attempts, func() error {
		f, ferr := r.fs.Open(name)
		if ferr != nil {
			return ferr
		}
		// The close's own error is dropped: the read the attempt existed
		// for has already finished, and a close that failed releases
		// nothing the caller still needs.
		defer func() { _ = f.Close() }()

		var rerr error
		n, rerr = f.ReadAt(buf, off)
		if errors.Is(rerr, io.EOF) {
			endOfFile = true
			return nil
		}
		return rerr
	})
	switch {
	case endOfFile:
		return n, io.EOF
	case err == nil:
		return n, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return n, err
	}

	// decision: a transient fault on a read at open keeps the journal
	// closed. An open that holds an unread record, the suspect segment
	// and the unread file until the device reads again is the later change
	// that replaces this refusal; until then the range is collected — the
	// open refuses on what it could not read rather than opening with
	// holes it would have to serve — and no byte of it is classed,
	// truncated or reported as lost. Withdrawn when an unread record can
	// be held instead of refused.
	r.warnUnreadable(name, err)
	// The format file is read by ReadFormat, which answers its own failure;
	// every name here is a segment the open reaches through the pass.
	_, id := format.ClassifyName(name)
	r.unreadable(core.SegmentRange{Segment: id, Off: off, Len: int64(len(buf))})
	return n, err
}

// unreadable collects sr as a range no bounded read completed. A range an
// earlier failed read already collected contains is not named twice: a scan
// that narrows a failed read to fewer bytes of the same place fails the same
// bytes again, and the open names them once.
func (r *reader) unreadable(sr core.SegmentRange) {
	if !slices.ContainsFunc(r.unread, func(o core.SegmentRange) bool {
		return o.Segment == sr.Segment && o.Off <= sr.Off && sr.Off+sr.Len <= o.Off+o.Len
	}) {
		r.unread = append(r.unread, sr)
	}
}

// openSegment opens the segment id's file as the open's other reaches into
// storage do: under the same bound every read takes, so an open the device
// does not answer is started again and, when no bounded attempt completes,
// collected — the file whole, for an open reaches every byte of the segment
// and names no range of its own, the same name a size it could not measure
// takes. The sync the flows that open a segment make happens on the handle
// between the open and the read, so the open stays this method's and the
// reads below the flow's own.
func (r *reader) openSegment(id core.SegmentID) (storage.File, error) {
	name := format.SegmentName(id)
	var f storage.File
	_, err := readretry.Do(r.ctx, r.clk, readretry.Attempts, func() error {
		var ferr error
		f, ferr = r.fs.Open(name)
		return ferr
	})
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, err
	}
	r.warnUnreadable(name, err)
	r.unreadable(core.SegmentRange{Segment: id, Off: 0, Len: math.MaxInt64})
	return nil, err
}

// ReadAtDirect reads len(buf) bytes of the segment file name holds at off
// directly — off the device, past the page cache — through the open file f,
// under the same bound ReadAt takes: a direct read the device does not answer
// is started again, and a read that ends in io.EOF completed to the file's
// own end, coming back as ReadAt's short read does.
//
// A read no bounded attempt completed answers with refused set — the verdict
// is the caller's rule to apply, never a parameter the caller hands in: the
// reader warns of the fault but collects no range on the caller's behalf, so
// a caller that kept no copy cannot silently turn a device fault into a
// verdict by passing a flag. A caller whose refusal keeps the journal closed
// collects the range itself and refuses the open on it, as the marker flow
// does — a seal no read the device showed may prove nothing; the tail's
// re-read of a record whose first read verified answers lost, its collected
// proof ruled elsewhere, for the record's copy is kept and the re-append
// writes it back.
//
// decision: the open stays the caller's, not one opened per attempt inside
// the bound. The read proves the bytes a sync carried to the device, and
// the flows sync the segment between opening it and reading — the marker
// its marker, the tail its records — so a fresh handle per attempt would
// read beside the very sync the flow made on the handle it holds. The bound
// retries the read over the handle the flow owns, as the direct read's own
// contract leaves the refusal's meaning to the tail check. Overturned if a
// platform is found whose direct reads answer differently per handle.
func (r *reader) ReadAtDirect(f storage.File, name string, buf []byte, off int64) (n int, refused bool, err error) {
	endOfFile := false
	_, err = readretry.Do(r.ctx, r.clk, readretry.Attempts, func() error {
		var rerr error
		n, rerr = f.ReadAtDirect(buf, off)
		if errors.Is(rerr, io.EOF) {
			endOfFile = true
			return nil
		}
		return rerr
	})
	switch {
	case endOfFile:
		return n, false, io.EOF
	case err == nil:
		return n, false, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return n, false, err
	}
	r.warnUnreadable(name, err)
	return n, true, err
}

// Unreadable returns each byte range no bounded read completed, in the
// order the reads that failed were made. The open refuses on what it
// collected; an empty answer is an open that read everything it reached for.
func (r *reader) Unreadable() []core.SegmentRange {
	return r.unread
}
