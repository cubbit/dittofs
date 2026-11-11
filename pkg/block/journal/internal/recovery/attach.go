package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// SegmentInfo is one own or foreign segment the attach pass found: its
// header as it verified, and the id in its file name. The two may differ —
// an interrupted recycle leaves a header naming the segment its file used
// to be — so the name's id is the one the ledger goes by. Header.Identity
// names the journal the segment belongs to: the opened one for Own,
// another's for Foreign.
type SegmentInfo struct {
	ID     core.SegmentID
	Header format.SegmentHeader
}

// Attached is what the attach pass found in the journal's directory.
type Attached struct {
	// Own holds each segment whose header verifies and names this journal,
	// in listing order.
	Own []SegmentInfo

	// Foreign holds each segment whose header verifies and names another
	// journal: it contributes nothing, is left on disk untouched, and is
	// counted in Rejected.
	Foreign []SegmentInfo

	// Unverified holds the id of each .seg file whose header slot is not
	// all zeros but does not verify: torn, damaged, or a write a crash
	// cut. The pass records it; the later phases decide it.
	Unverified []core.SegmentID

	// ZeroHeader holds the id of each .seg file whose header slot cannot
	// be judged because no byte of it was written: an empty file, one too
	// short for a header, or an all-zero slot. A short file whose prefix
	// is not all zeros is not here: its bytes are judged by the decoder,
	// and fail there into Unverified.
	ZeroHeader []core.SegmentID

	// Unrecognised holds each name the classifier did not recognise,
	// left as it was.
	Unrecognised []string

	// MaxID is the highest id any .seg or .spare.tmp file names: the seed
	// every id made after open is drawn above.
	MaxID core.SegmentID

	// Rejected counts the segments the pass did not attach. A foreign
	// segment counts one; an unreadable header counts none — its segment
	// was never classed, and the open refuses on its range instead.
	Rejected int
}

// attach lists the journal's directory once and classes what it finds,
// opening the segments under journal id. log takes the caller's logger; nil
// discards. It unlinks every .spare.tmp and nothing else; a name the
// classifier does not recognise is left alone, logged once each at Debug.
// A segment header of a format version the pass cannot open fails it with
// ErrUnknownFormat. A header read that no bounded attempt completed leaves
// the segment classed nowhere and the range in rd, and does not fail the
// pass here; the open refuses on what rd collected. The context is checked
// before each name, and a read the open's own cancellation cut fails the
// pass with the context's error, collected nowhere — a cancelled open never
// answers with a segment silently missing, and changes nothing the pass
// made but .spare.tmp unlinks.
func attach(ctx context.Context, fs storage.FS, rd *reader, id core.JournalID, log *slog.Logger) (Attached, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := ctx.Err(); err != nil {
		return Attached{}, err
	}
	names, err := fs.List()
	if err != nil {
		return Attached{}, err
	}

	var att Attached
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Attached{}, err
		}
		kind, sid := format.ClassifyName(name)
		switch kind {
		case format.NameFormat, format.NameFormatTmp:
			// The format file is read by the open itself; a format.tmp is
			// the last writer's, to be overwritten or cleaned up there.
		case format.NameSpare:
			// The temporary name a spare is made under. The pass is the
			// one step that unlinks it, and the id it names still counts,
			// so a crash that leaves the highest spare's temporary name
			// seeds the allocator above it — a spare made after open
			// never reuses an id the directory named.
			if sid > att.MaxID {
				att.MaxID = sid
			}
			if err := fs.Remove(name); err != nil {
				return Attached{}, err
			}
		case format.NameSegment:
			if sid > att.MaxID {
				att.MaxID = sid
			}
			if err := att.attachSegment(rd, name, sid, id); err != nil {
				return Attached{}, err
			}
		default:
			// decision: orphans are reported, never unlinked. Every .seg
			// file this journal wrote is attached, re-adopted as a spare
			// or unlinked by the emptying when the journal ends it; what
			// is left is a foreign segment, a mark-named empty one, or a
			// file like this one, under a name the journal could not
			// have produced — none of which this format version may
			// unlink, and an age gate would only delay unlinking a file
			// whose class never reaches it. Orphans cost their space and
			// an operator's attention; overturned by an orphan class this
			// journal can prove it wrote that accumulates in the field.
			att.Unrecognised = append(att.Unrecognised, name)
			log.Debug("journal attach: a file this journal does not recognise is left alone",
				"name", name)
		}
	}
	return att, nil
}

// attachSegment reads the header slot of the .seg file name, whose id in
// its file name is sid, and classes the segment into att. A header read
// that no bounded attempt completed classes it nowhere — not zero, damaged
// or foreign — and the range stays in rd for the open to refuse on; a read
// the open's own cancellation cut instead fails the pass with the
// context's error, collected nowhere, so a segment is never silently
// missing from the answer. A header of a version the pass cannot open
// fails the pass; anything else is a class the later phases decide.
func (att *Attached) attachSegment(rd *reader, name string, sid core.SegmentID, id core.JournalID) error {
	b := make([]byte, format.SegmentHeaderSize)
	_, rerr := rd.ReadAt(name, b, 0)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
			// The open's own cancellation cut the read: no attempt
			// faulted the device, so the range is collected nowhere and
			// the pass reports the cancellation rather than answering
			// without this segment.
			return rerr
		}
		return nil // no bounded read completed; refused at the open, on rd's range
	}
	if !anyHeaderByte(b) {
		att.ZeroHeader = append(att.ZeroHeader, sid)
		return nil
	}

	h, derr := format.DecodeSegmentHeader(b, id)
	switch {
	case derr == nil:
		att.Own = append(att.Own, SegmentInfo{ID: sid, Header: h})
	case errors.Is(derr, format.ErrForeign):
		// Reported and left untouched, as the orphan decision at the
		// unrecognised name in attach covers a foreign segment too.
		att.Foreign = append(att.Foreign, SegmentInfo{ID: sid, Header: h})
		att.Rejected++
	case errors.Is(derr, format.ErrUnknownVersion):
		// decision: "newer than the format file's" is not told here from
		// "unknown to this binary": the decoder compares the header's
		// version against the one version this binary reads, and the
		// format file of any directory the open reached past names that
		// same version, so the one refusal covers both halves of the
		// rule. Add a comparison of the header's version with the format
		// file's once a second format version exists, from which a
		// binary could read a directory whose format file names an older
		// version than one of its segments.
		return fmt.Errorf("%w: segment %d: %v", core.ErrUnknownFormat, sid, derr)
	default:
		// Damaged, or whatever else the decoder returns that no rule
		// above claimed: the segment is recorded unverified and the
		// later phases judge it, never dropped in silence.
		att.Unverified = append(att.Unverified, sid)
	}
	return nil
}

// anyHeaderByte reports whether any of b's bytes is not zero, so an
// all-zero header slot is told from one that holds bytes a checksum can
// judge.
func anyHeaderByte(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}
