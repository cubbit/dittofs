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

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
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
	r.unread = append(r.unread, core.SegmentRange{Segment: id, Off: off, Len: int64(len(buf))})
	return n, err
}

// Unreadable returns each byte range no bounded read completed, in the
// order the reads that failed were made. The open refuses on what it
// collected; an empty answer is an open that read everything it reached for.
func (r *reader) Unreadable() []core.SegmentRange {
	return r.unread
}
