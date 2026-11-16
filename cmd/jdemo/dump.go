// This file is a transcription, not an import: the on-disk record layout it
// decodes is private to pkg/block/journal/internal/format (record.go's
// DecodeRecordHeader, kinds.go's table, segheader.go, names.go), which this
// demo binary cannot reach and must not reach. Transcribed with each
// source named at the constant; a segment the journal's own version read
// fine but this copy reads as torn is the drift this file's design accepts
// — it shows as clearly wrong output at rehearsal, never as silently wrong
// data, and nothing merges from here. The two checksum slots are NOT
// verified: they are seeded with the journal's identity, and the seed
// algorithm is format internals the demo does not copy.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The on-disk facts, from pkg/block/journal/internal/format:
const (
	dumpSegmentHeaderSize = 48 // segheader.go: SegmentHeaderSize
	dumpRecordHeaderSize  = 96 // record.go: RecordHeaderSize
)

// dumpRecord is one record as the walk read it.
type dumpRecord struct {
	kind    uint8
	file    [16]byte
	off     int64
	length  int64
	seq     uint64
	epoch   uint64
	counter uint64
	tag     uint32
	recOff  int64 // the record's own offset in its segment
	body    []byte
	bodyOff int64
}

// kindName is the kind's word, transcribed from kinds.go's table: the
// kinds are stable values 1..16, and the comments there are the names.
func (r dumpRecord) kindName() string {
	switch r.kind {
	case 1:
		return "write"
	case 2:
		return "fill"
	case 3:
		return "release"
	case 4:
		return "truncate"
	case 5:
		return "deallocate"
	case 6:
		return "delete"
	case 7:
		return "clone"
	case 8:
		return "offloaded"
	case 9:
		return "unmark"
	case 10:
		return "loss"
	case 11:
		return "hold"
	case 12:
		return "unhold"
	case 13:
		return "stamp"
	case 14:
		return "forget"
	case 15:
		return "reconcile"
	case 16:
		return "reconciled"
	}
	return fmt.Sprintf("kind%d", r.kind)
}

// bodySize is kinds.go's bodySize, for walking: write an 8-byte mtime plus
// the payload, fill the payload, loss and reconciled 16, a clone 56, hold,
// unhold and stamp 8, the rest none.
func bodySize(kind uint8, length int64) int64 {
	switch kind {
	case 1: // write
		return 8 + length
	case 2: // fill
		return length
	case 10, 15: // loss, reconcile
		return 16
	case 7: // clone
		return 56
	case 11, 12, 13: // hold, unhold, stamp
		return 8
	default:
		return 0
	}
}

// scanSegment walks one segment file record by record: the 48-byte segment
// header (segheader.go), then each record at its 8-aligned pitch
// (record.go: (96 + body + 7) &^ 7). A header that does not decode — the
// trailing tail of an active segment, a seal footer, a torn write — stops
// the walk; answer what walked, torn true, and never an error for that.
// The two checksum slots are read but not verified (see the file note).
func scanSegment(path string) ([]dumpRecord, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var recs []dumpRecord
	torn := false
	off := int64(dumpSegmentHeaderSize)
	for {
		if off+dumpRecordHeaderSize > int64(len(b)) {
			torn = off < int64(len(b)) // a trailing partial slot
			break
		}
		h := b[off : off+dumpRecordHeaderSize]
		if string(h[0:4]) != "DJR1" { // record.go: the record magic
			torn = true
			break
		}
		r := dumpRecord{
			kind:    h[4],
			file:    [16]byte(h[16:32]),
			off:     int64(binary.LittleEndian.Uint64(h[32:40])),
			length:  int64(binary.LittleEndian.Uint64(h[40:48])),
			seq:     binary.LittleEndian.Uint64(h[48:56]),
			epoch:   binary.LittleEndian.Uint64(h[56:64]),
			counter: binary.LittleEndian.Uint64(h[64:72]),
			tag:     binary.LittleEndian.Uint32(h[72:76]),
			recOff:  off,
		}
		if h[4] < 1 || h[4] > 16 { // kinds.go: known()
			torn = true
			break
		}
		body := bodySize(r.kind, r.length)
		pitch := (dumpRecordHeaderSize + body + 7) &^ 7
		if off+dumpRecordHeaderSize+body > int64(len(b)) {
			torn = true
			break
		}
		r.body = b[off+dumpRecordHeaderSize : off+dumpRecordHeaderSize+body]
		r.bodyOff = off + dumpRecordHeaderSize
		recs = append(recs, r)
		off += pitch
	}
	return recs, torn, nil
}

// viewDump is the (d) view: every record of every segment, decoded, with
// the payload bytes showing — the journal's on-disk truth.
func (d *demo) viewDump() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		d.printf("data dump: %v\n", err)
		return
	}
	var segs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".seg") {
			segs = append(segs, e.Name())
		}
	}
	sort.Strings(segs)
	d.printf("records on disk (checksums not verified by this decoder — the seed is format internals)\n")
	if len(segs) == 0 {
		d.printf("no segments yet\n")
		return
	}
	rows := 0
	for _, name := range segs {
		recs, torn, err := scanSegment(filepath.Join(d.dir, name))
		if err != nil {
			d.printf("%s: %v\n", name, err)
			continue
		}
		for _, r := range recs {
			rows++
			text := fmt.Sprintf("%-9s %-10s %s: %3d–%-3d v%-3d tag %d  seq %d  @%d",
				name, r.kindName(), fileHexShort(r.file), r.off, r.off+r.length, r.counter, r.tag, r.seq, r.recOff)
			d.printf("  %s\n", text)
			if len(r.body) > 0 {
				d.hexdump(r.body, 8+8) // 16 lanes
			}
		}
		if torn {
			d.printf("  %s: tail torn (or sealed) at the next slot; %d records before it\n", name, len(recs))
		}
	}
	if rows == 0 {
		d.printf("no records walked\n")
	}
}

// hexdump prints up to dumpBodyLimit bytes of b with an ASCII lane beside
// them; a truncated body says what remains.
const dumpBodyLimit = 512

func (d *demo) hexdump(b []byte, wide int) {
	limit := len(b)
	if limit > dumpBodyLimit {
		limit = dumpBodyLimit
	}
	for i := 0; i < limit; i += wide {
		chunk := b[i:min(i+wide, limit)]
		var hexes, ascii strings.Builder
		for _, c := range chunk {
			fmt.Fprintf(&hexes, "%02x ", c)
			if c >= 0x20 && c < 0x7f {
				ascii.WriteByte(c)
			} else {
				ascii.WriteByte('.')
			}
		}
		d.printf("      %-48s  %s\n", hexes.String(), ascii.String())
	}
	if len(b) > limit {
		d.printf("      (+%d more bytes)\n", len(b)-limit)
	}
}

// fileHexShort renders a record's file identity the way views do.
func fileHexShort(f [16]byte) string {
	return hex.EncodeToString(f[0:8])
}
