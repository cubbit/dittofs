package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/cubbit/dittofs/pkg/remote"
)

// rawEntry is one census entry laid out by hand. kindLen and idLen, when not
// -1, are written in place of the true lengths, to plant a length that lies.
type rawEntry struct {
	t        uint16
	v        uint8
	fp       byte // every fingerprint byte
	kind, id string
	kindLen  int
	idLen    int
}

func entry(t uint16, fp byte, kind, id string) rawEntry {
	return rawEntry{t: t, v: 1, fp: fp, kind: kind, id: id, kindLen: -1, idLen: -1}
}

func (e rawEntry) bytes() []byte {
	b := binary.LittleEndian.AppendUint16(nil, e.t)
	b = append(b, e.v)
	b = append(b, bytes.Repeat([]byte{e.fp}, 32)...)
	b = append(b, lenByte(e.kindLen, e.kind))
	b = append(b, e.kind...)
	b = append(b, lenByte(e.idLen, e.id))
	return append(b, e.id...)
}

func lenByte(planted int, s string) byte {
	if planted >= 0 {
		return byte(planted)
	}
	return byte(len(s))
}

// rawHeader is a version 1 header laid out by hand from D1's table, apart from
// the codec's encoder. Each field left at its zero value takes the true value
// of the bytes written.
type rawHeader struct {
	marker  string
	version uint8
	length  int // the declared header length; 0 is the bytes' own
	chunks  int // the declared chunk count; index entries written
	declare int // a chunk count declared other than chunks; 0 is chunks
	count   int // a census count declared other than len(census); -1 is len(census)
	census  []rawEntry
	tail    []byte // bytes inside the header after the census
}

func validHeader() rawHeader {
	return rawHeader{
		marker: marker, version: 1, chunks: 3, count: -1,
		census: []rawEntry{entry(0, 0, "", ""), entry(2, 7, "data-key", "k1")},
	}
}

func (h rawHeader) bytes() []byte {
	var body []byte
	for i := range h.chunks {
		body = append(body, bytes.Repeat([]byte{byte(0xA0 + i)}, 32)...)
		body = binary.LittleEndian.AppendUint32(body, uint32(1000+i))
		body = binary.LittleEndian.AppendUint32(body, uint32(10+i))
	}
	for _, e := range h.census {
		body = append(body, e.bytes()...)
	}
	body = append(body, h.tail...)
	length := h.length
	if length == 0 {
		length = fixedLen + len(body)
	}
	declare, count := h.chunks, len(h.census)
	if h.declare != 0 {
		declare = h.declare
	}
	if h.count >= 0 {
		count = h.count
	}
	b := append([]byte(h.marker), h.version)
	b = binary.LittleEndian.AppendUint32(b, uint32(length))
	b = binary.LittleEndian.AppendUint16(b, uint16(declare))
	b = binary.LittleEndian.AppendUint16(b, uint16(count))
	b = append(b, bytes.Repeat([]byte{0x11}, 32)...) // name
	b = append(b, bytes.Repeat([]byte{0x22}, 16)...) // nonce
	b = append(b, bytes.Repeat([]byte{0x33}, 32)...) // chain ID
	return append(b, body...)
}

// atEveryBound is a header with every field at its bound at once: N index
// entries and N × 3 census entries, each with a 32-byte kind and a 255-byte ID.
func atEveryBound() rawHeader {
	h := rawHeader{marker: marker, version: 1, chunks: maxChunks, count: -1}
	kind, id := strings.Repeat("k", maxKindLen), strings.Repeat("i", maxIDLen)
	for i := range maxCensus {
		h.census = append(h.census, entry(uint16(i), 1, kind, id))
	}
	return h
}

func TestMaxHeaderLen(t *testing.T) {
	if n, ok := MaxHeaderLen(1); n != 1_036_381 || !ok {
		t.Errorf("MaxHeaderLen(1) = %d, %v, want 1036381, true", n, ok)
	}
	for _, v := range []uint8{0, 2, 255} {
		if n, ok := MaxHeaderLen(v); n != 0 || ok {
			t.Errorf("MaxHeaderLen(%d) = %d, %v, want 0, false", v, n, ok)
		}
	}
}

// A header at every bound at once parses, takes one buffer of exactly
// MaxHeaderLen(1), and re-encodes to its bytes.
func TestParseHeaderAtEveryBound(t *testing.T) {
	raw := atEveryBound().bytes()
	if len(raw) != maxHeaderLenV1 {
		t.Fatalf("the header at every bound is %d bytes, want %d", len(raw), maxHeaderLenV1)
	}
	var rec allocRecord
	h, err := parseHeader(bytes.NewReader(raw), rec.alloc)
	if err != nil {
		t.Fatalf("parseHeader at every bound: %v", err)
	}
	if !slices.Equal(rec.sizes, []int{maxHeaderLenV1}) {
		t.Errorf("allocations %v, want [%d]", rec.sizes, maxHeaderLenV1)
	}
	if len(h.index) != maxChunks || len(h.census) != maxCensus {
		t.Errorf("parsed %d index and %d census entries, want %d and %d", len(h.index), len(h.census), maxChunks, maxCensus)
	}
	if got := encodeHeader(nil, h); !bytes.Equal(got, raw) {
		t.Error("the header at every bound does not re-encode to its bytes")
	}
}

// The encoder writes D1's layout: a header built as a value encodes to the
// bytes laid out by hand, and those parse back to the same fields.
func TestEncodeHeaderLayout(t *testing.T) {
	want := validHeader().bytes()
	h := &header{version: 1}
	for i := range h.name {
		h.name[i] = 0x11
	}
	for i := range h.nonce {
		h.nonce[i] = 0x22
	}
	for i := range h.chain {
		h.chain[i] = 0x33
	}
	for i := range 3 {
		e := indexEntry{off: uint32(1000 + i), length: uint32(10 + i)}
		for j := range e.id {
			e.id[j] = byte(0xA0 + i)
		}
		h.index = append(h.index, e)
	}
	fp := [32]byte{}
	for i := range fp {
		fp[i] = 7
	}
	h.census = []censusEntry{{version: 1}, {transform: 2, version: 1, fingerprint: fp, kind: []byte("data-key"), id: []byte("k1")}}
	if got := encodeHeader(nil, h); !bytes.Equal(got, want) {
		t.Fatalf("encodeHeader =\n%x\nwant\n%x", got, want)
	}

	// The body after the header is left unread.
	r := bytes.NewReader(append(slices.Clone(want), "bodies"...))
	got, err := parseHeader(r, makeBuf)
	if err != nil {
		t.Fatalf("parseHeader: %v", err)
	}
	if r.Len() != len("bodies") {
		t.Errorf("parseHeader left %d bytes unread, want %d", r.Len(), len("bodies"))
	}
	if got.name != h.name || got.nonce != h.nonce || got.chain != h.chain || !slices.Equal(got.index, h.index) {
		t.Errorf("parsed fixed fields or index differ from those encoded")
	}
	if len(got.census) != 2 || compareCensus(&got.census[0], &h.census[0]) != 0 || compareCensus(&got.census[1], &h.census[1]) != 0 {
		t.Errorf("parsed census %+v, want %+v", got.census, h.census)
	}
}

// Each refusal is ErrCorrupt naming its field. One of the fixed part is made
// before anything is allocated; one inside the buffer after exactly one
// request, for the header length.
func TestParseHeaderRefusals(t *testing.T) {
	kind33 := entry(2, 7, strings.Repeat("k", 33), "k1")
	kindPast := entry(2, 7, "ab", "")
	kindPast.kindLen = 30
	idPast := entry(2, 7, "data-key", "x")
	idPast.idLen = 200
	for _, tc := range []struct {
		name    string
		edit    func(h *rawHeader)
		want    string
		inFixed bool
	}{
		{"wrong marker", func(h *rawHeader) { h.marker = "DFSX" }, "marker", true},
		{"version 0", func(h *rawHeader) { h.version = 0 }, "block format version 0 is unknown", true},
		{"version 2", func(h *rawHeader) { h.version = 2 }, "block format version 2 is unknown", true},
		{"length below the fixed part", func(h *rawHeader) { h.length = fixedLen - 1 }, "header length 92", true},
		{"length above the bound", func(h *rawHeader) { h.length = maxHeaderLenV1 + 1 }, "header length 1036382", true},
		{"no chunk", func(h *rawHeader) { h.chunks = 0 }, "chunk count 0", true},
		{"1,025 chunks", func(h *rawHeader) { h.declare = maxChunks + 1 }, "chunk count 1025", true},
		{"3,073 census entries", func(h *rawHeader) { h.count = maxCensus + 1 }, "census count 3073", true},
		{"an index that does not fit", func(h *rawHeader) { h.census = nil; h.chunks = 2; h.declare = 3 }, "index of 3 entries does not fit", false},
		{"a census entry past the header", func(h *rawHeader) { h.count = 3 }, "census entry 2 runs past", false},
		{"a kind length of 33", func(h *rawHeader) { h.census[1] = kind33 }, "census entry 1 kind length 33", false},
		{"a kind past the census", func(h *rawHeader) { h.census[1] = kindPast }, "census entry 1 kind runs past", false},
		{"an ID past the census", func(h *rawHeader) { h.census[1] = idPast }, "census entry 1 ID runs past", false},
		{"a census short of the header length", func(h *rawHeader) { h.tail = []byte{0} }, "census ends at", false},
		{"entries out of order", func(h *rawHeader) { slices.Reverse(h.census) }, "census entry 1 is out of order or repeated", false},
		{"an entry repeated", func(h *rawHeader) { h.census[0] = h.census[1] }, "census entry 1 is out of order or repeated", false},
		{"a fingerprint without material", func(h *rawHeader) { h.census[0].fp = 1 }, "census entry 0 has no material but a fingerprint", false},
		{"an ID without a kind", func(h *rawHeader) { h.census[0].id = "x" }, "census entry 0 has an ID but no kind", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := validHeader()
			tc.edit(&h)
			raw := h.bytes()
			var rec allocRecord
			_, err := parseHeader(bytes.NewReader(raw), rec.alloc)
			if !errors.Is(err, remote.ErrCorrupt) {
				t.Fatalf("parseHeader error %v, want ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseHeader error %q, want it to name %q", err, tc.want)
			}
			if strings.Contains(tc.want, "version") == strings.Contains(err.Error(), "malformed") {
				t.Errorf("parseHeader error %q: a version error must not read as a parse error, nor a parse error as one", err)
			}
			want := []int{len(raw)}
			if tc.inFixed {
				want = nil
			}
			if !slices.Equal(rec.sizes, want) {
				t.Errorf("allocations %v, want %v", rec.sizes, want)
			}
		})
	}
}

// A stream that ends inside the header is malformed; a reader's own error is
// returned as itself.
func TestParseHeaderReads(t *testing.T) {
	raw := validHeader().bytes()
	for _, n := range []int{0, fixedLen - 1, len(raw) - 1} {
		_, err := parseHeader(bytes.NewReader(raw[:n]), makeBuf)
		if !errors.Is(err, remote.ErrCorrupt) || !strings.Contains(err.Error(), "truncated") {
			t.Errorf("parseHeader of %d of %d bytes: %v, want a truncated header", n, len(raw), err)
		}
	}
	lost := errors.New("connection reset")
	_, err := parseHeader(iotest.ErrReader(lost), makeBuf)
	if !errors.Is(err, lost) || errors.Is(err, remote.ErrCorrupt) {
		t.Errorf("parseHeader over a failing reader: %v, want its error and not ErrCorrupt", err)
	}
}

// fixedPartValid is the fixed part's checks, written apart from parseHeader's:
// a header whose fixed part fails them must have allocated nothing.
func fixedPartValid(b []byte) (length int, ok bool) {
	if len(b) < fixedLen || string(b[:4]) != marker || b[4] != 1 {
		return 0, false
	}
	length = int(binary.LittleEndian.Uint32(b[5:]))
	chunks := binary.LittleEndian.Uint16(b[9:])
	census := binary.LittleEndian.Uint16(b[11:])
	return length, length >= fixedLen && length <= 1_036_381 && chunks >= 1 && chunks <= 1024 && census <= 3072
}

// FuzzHeader: parseHeader never panics; it allocates nothing until the fixed
// part is valid and then exactly the declared length, at most MaxHeaderLen(1);
// it reads no further than the header; and a header it accepts re-encodes to
// the bytes it came from.
func FuzzHeader(f *testing.F) {
	add := func(edit func(h *rawHeader)) {
		h := validHeader()
		edit(&h)
		f.Add(h.bytes())
	}
	add(func(h *rawHeader) {})
	add(func(h *rawHeader) { h.length = maxHeaderLenV1 + 1 })
	add(func(h *rawHeader) { h.declare = maxChunks + 1 })
	add(func(h *rawHeader) { h.count = maxCensus + 1 })
	add(func(h *rawHeader) { h.census[1].kind = strings.Repeat("k", 33) })
	add(func(h *rawHeader) { h.census[1].kind, h.census[1].kindLen = "ab", 30 })
	add(func(h *rawHeader) { h.census[1].idLen = 200 })
	add(func(h *rawHeader) {
		h.census[1].kind, h.census[1].id = strings.Repeat("k", maxKindLen), strings.Repeat("i", maxIDLen)
	})
	add(func(h *rawHeader) { slices.Reverse(h.census) })
	add(func(h *rawHeader) { h.census[0] = h.census[1] })

	f.Fuzz(func(t *testing.T, data []byte) {
		var rec allocRecord
		r := bytes.NewReader(data)
		h, err := parseHeader(r, rec.alloc)
		length, ok := fixedPartValid(data)
		switch {
		case !ok && len(rec.sizes) != 0:
			t.Fatalf("allocated %v for a header whose fixed part is invalid", rec.sizes)
		case ok && !slices.Equal(rec.sizes, []int{length}):
			t.Fatalf("allocated %v for a header of declared length %d, want exactly that", rec.sizes, length)
		}
		if err != nil {
			if errors.Is(err, remote.ErrCorrupt) {
				return
			}
			t.Fatalf("parseHeader error %v, want ErrCorrupt", err)
		}
		if read := len(data) - r.Len(); read != length {
			t.Fatalf("parseHeader read %d bytes of a %d-byte header", read, length)
		}
		if got := encodeHeader(nil, h); !bytes.Equal(got, data[:length]) {
			t.Fatalf("an accepted header re-encodes to\n%x\nnot\n%x", got, data[:length])
		}
	})
}
