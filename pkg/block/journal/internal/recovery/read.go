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

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// reader is the open's one reach into its directory: every read the open
// makes goes through it, so a read the device cannot complete is retried by
// the bound and, when no attempt reads, collected rather than judged. It
// belongs to one open and is not safe for concurrent use.
type reader struct {
	ctx context.Context
	fs  storage.FS
	clk core.Clock

	// unread holds each segment range no bounded read completed, and
	// formatUnread says the format file's read never completed.
	unread       []core.SegmentRange
	formatUnread bool
}

// newReader returns the open's reader over fs, running under ctx and on clk.
func newReader(ctx context.Context, fs storage.FS, clk core.Clock) *reader {
	return &reader{ctx: ctx, fs: fs, clk: clk}
}

// ReadAt reads len(buf) bytes of the file name at offset off. A read that
// ends in io.EOF completed to the file's own end and is passed through: the
// bytes a short file does not hold are the file's length, not a fault of the
// device. Any other error the bound's attempts did not clear is collected —
// the file as the format's, a segment's bytes as their range — and returned,
// except when the open's own context ended the retries: that says nothing
// about the device, so it is only returned.
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
	if kind, id := format.ClassifyName(name); kind == format.NameFormat {
		r.formatUnread = true
	} else {
		r.unread = append(r.unread, core.SegmentRange{Segment: id, Off: off, Len: int64(len(buf))})
	}
	return n, err
}

// Unreadable returns each byte range no bounded read completed, and whether
// the format file never read. The open refuses on what it collected; an
// empty answer is an open that read everything it reached for.
func (r *reader) Unreadable() (ranges []core.SegmentRange, format bool) {
	return r.unread, r.formatUnread
}
