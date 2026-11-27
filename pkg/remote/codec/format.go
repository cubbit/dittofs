package codec

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Format version 1 (RFC 4 §3.2): a fixed-width, little-endian header, then the
// bodies in index order. No field has a variable-length integer, so the bound
// is a constant and every check before an allocation is a comparison.
//
//	offset  field                                   width
//	0       marker "DFSB"                           4
//	4       format version                          1
//	5       header length, the fixed part included  4
//	9       chunk count, 1 to maxChunks             2
//	11      census count, 0 to maxCensus            2
//	13      block name                              32
//	45      nonce                                   16
//	61      chain ID                                32
//	93      index: per chunk, chunk ID (32), body offset (4), body length (4)
//	then    census: per entry, transform ID (2), version (1), fingerprint (32),
//	        kind length (1) and kind, ID length (1) and ID
const (
	marker         = "DFSB"
	fixedLen       = 93
	maxChunks      = 1024               // RFC 2 §5's N
	maxCensus      = maxChunks * 3      // N times RFC 5 §2.3's three stages
	indexEntryLen  = 32 + 4 + 4         // chunk ID, offset, length
	censusFixedLen = 2 + 1 + 32 + 1 + 1 // transform ID, version, fingerprint, two length bytes
	maxKindLen     = 32
	maxIDLen       = 255

	maxHeaderLenV1 = fixedLen + maxChunks*indexEntryLen + maxCensus*(censusFixedLen+maxKindLen+maxIDLen)
)

// indexEntry is one chunk's entry: its ID and its body's place in the block.
type indexEntry struct {
	id          [32]byte
	off, length uint32
}

// censusEntry is one (transform, version, material) the bodies were encoded
// under. A parsed entry's kind and id are views into the header's buffer. An
// entry with no material has an empty kind and ID and a zero fingerprint.
type censusEntry struct {
	transform   uint16
	version     uint8
	fingerprint [32]byte
	kind, id    []byte
}

// header is a parsed or to-be-written version 1 header.
type header struct {
	version uint8
	name    remote.Name
	nonce   [16]byte
	chain   [32]byte
	index   []indexEntry
	census  []censusEntry
}

// size is the header's encoded length in bytes.
func (h *header) size() int {
	n := fixedLen + len(h.index)*indexEntryLen
	for _, e := range h.census {
		n += censusFixedLen + len(e.kind) + len(e.id)
	}
	return n
}

// encodeHeader appends h's bytes to dst. The caller has already held h to the
// format's bounds and D1's canonical census (sorted, distinct, kinds of at
// most maxKindLen and IDs of at most maxIDLen bytes): encodeHeader checks
// nothing, so a header out of bounds encodes to bytes parseHeader refuses.
func encodeHeader(dst []byte, h *header) []byte {
	dst = append(dst, marker...)
	dst = append(dst, h.version)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.size()))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(h.index)))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(h.census)))
	dst = append(dst, h.name[:]...)
	dst = append(dst, h.nonce[:]...)
	dst = append(dst, h.chain[:]...)
	for _, e := range h.index {
		dst = append(dst, e.id[:]...)
		dst = binary.LittleEndian.AppendUint32(dst, e.off)
		dst = binary.LittleEndian.AppendUint32(dst, e.length)
	}
	for _, e := range h.census {
		dst = binary.LittleEndian.AppendUint16(dst, e.transform)
		dst = append(dst, e.version)
		dst = append(dst, e.fingerprint[:]...)
		dst = append(dst, byte(len(e.kind)))
		dst = append(dst, e.kind...)
		dst = append(dst, byte(len(e.id)))
		dst = append(dst, e.id...)
	}
	return dst
}

// makeBuf is the allocator a decoder uses outside its tests.
func makeBuf(n int) []byte { return make([]byte, n) }

// parseHeader reads one header from r, taking its buffer from alloc. Every
// bound of the fixed part is checked before alloc is asked for anything, and
// then alloc is asked once, for the header length, which is at most
// MaxHeaderLen of the header's version (RFC 4 §3.2). r is read up to the
// header's end and no further.
//
// Every refusal is remote.ErrCorrupt naming the field that failed; an unknown
// version's message names the version, so it reads as a version error and not
// a parse error. A read error other than a short read is returned as itself.
func parseHeader(r io.Reader, alloc func(int) []byte) (*header, error) {
	var fixed [fixedLen]byte
	if err := readFull(r, fixed[:]); err != nil {
		return nil, err
	}
	if string(fixed[0:4]) != marker {
		return nil, malformed("marker %q is not %q", fixed[0:4], marker)
	}
	h := &header{version: fixed[4]}
	bound, ok := MaxHeaderLen(h.version)
	if !ok {
		// decision: an unknown version is ErrCorrupt, as RFC 4 §3.5's gate
		// means no reader meets one unless a newer binary wrote past it; the
		// message names the version so it is not mistaken for a parse error.
		return nil, fmt.Errorf("codec: block format version %d is unknown: %w", h.version, remote.ErrCorrupt)
	}
	length := binary.LittleEndian.Uint32(fixed[5:9])
	if length < fixedLen || length > uint32(bound) {
		return nil, malformed("header length %d is outside [%d, %d]", length, fixedLen, bound)
	}
	chunks := int(binary.LittleEndian.Uint16(fixed[9:11]))
	if chunks < 1 || chunks > maxChunks {
		return nil, malformed("chunk count %d is outside [1, %d]", chunks, maxChunks)
	}
	entries := int(binary.LittleEndian.Uint16(fixed[11:13]))
	if entries > maxCensus {
		return nil, malformed("census count %d is above %d", entries, maxCensus)
	}

	buf := alloc(int(length))
	copy(buf, fixed[:])
	if err := readFull(r, buf[fixedLen:]); err != nil {
		return nil, err
	}
	copy(h.name[:], buf[13:45])
	copy(h.nonce[:], buf[45:61])
	copy(h.chain[:], buf[61:93])

	at := fixedLen
	if at+chunks*indexEntryLen > len(buf) {
		return nil, malformed("index of %d entries does not fit a header of %d bytes", chunks, length)
	}
	h.index = make([]indexEntry, chunks)
	for i := range h.index {
		e := &h.index[i]
		copy(e.id[:], buf[at:at+32])
		e.off = binary.LittleEndian.Uint32(buf[at+32:])
		e.length = binary.LittleEndian.Uint32(buf[at+36:])
		at += indexEntryLen
	}

	h.census = make([]censusEntry, entries)
	for i := range h.census {
		if at+censusFixedLen > len(buf) {
			return nil, malformed("census entry %d runs past the header", i)
		}
		e := &h.census[i]
		e.transform = binary.LittleEndian.Uint16(buf[at:])
		e.version = buf[at+2]
		copy(e.fingerprint[:], buf[at+3:at+35])
		at += 35
		kind := int(buf[at])
		if kind > maxKindLen {
			return nil, malformed("census entry %d kind length %d is above %d", i, kind, maxKindLen)
		}
		at++
		// at+kind+1 leaves room for the ID's length byte after the kind.
		if at+kind+1 > len(buf) {
			return nil, malformed("census entry %d kind runs past the header", i)
		}
		e.kind = buf[at : at+kind : at+kind]
		at += kind
		id := int(buf[at])
		at++
		if at+id > len(buf) {
			return nil, malformed("census entry %d ID runs past the header", i)
		}
		e.id = buf[at : at+id : at+id]
		at += id

		if kind == 0 && id != 0 {
			return nil, malformed("census entry %d has an ID but no kind", i)
		}
		if kind == 0 && e.fingerprint != ([32]byte{}) {
			return nil, malformed("census entry %d has no material but a fingerprint", i)
		}
		if i > 0 && compareCensus(&h.census[i-1], e) >= 0 {
			return nil, malformed("census entry %d is out of order or repeated", i)
		}
	}
	if at != len(buf) {
		return nil, malformed("census ends at %d, not at the header length %d", at, length)
	}
	return h, nil
}

// compareCensus orders census entries by transform ID, version, kind, ID and
// fingerprint, D1's canonical order.
func compareCensus(a, b *censusEntry) int {
	return cmp.Or(
		cmp.Compare(a.transform, b.transform),
		cmp.Compare(a.version, b.version),
		bytes.Compare(a.kind, b.kind),
		bytes.Compare(a.id, b.id),
		bytes.Compare(a.fingerprint[:], b.fingerprint[:]),
	)
}

// readFull fills p from r. A short read is a truncated header, malformed; any
// other read error is r's, returned as itself.
func readFull(r io.Reader, p []byte) error {
	_, err := io.ReadFull(r, p)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("header is truncated")
	}
	if err != nil {
		return fmt.Errorf("codec: reading the header: %w", err)
	}
	return nil
}

// malformed is a header refusal: remote.ErrCorrupt, naming what failed.
func malformed(format string, args ...any) error {
	return fmt.Errorf("codec: malformed header: "+format+": %w", append(args, remote.ErrCorrupt)...)
}
