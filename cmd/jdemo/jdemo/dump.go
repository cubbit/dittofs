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
package jdemo

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

// endState is how a segment's walk ended: the three tails a demo audience
// must never read as one another.
type endState uint8

const (
	// endUntaken: the slot past the last record is zeros (an unwritten
	// tail) or the file was never taken as a segment (a spare). Not damage.
	endUntaken endState = iota
	// endSealed: a DJS1 seal marker ends the records the walk decoded.
	endSealed
	// endTorn: bytes that are none of those — a torn write, a place
	// mismatch, a record the transcription cannot step over.
	endTorn
)

func (e endState) String() string {
	switch e {
	case endUntaken:
		return "untaken"
	case endSealed:
		return "sealed"
	case endTorn:
		return "torn"
	}
	return "unknown"
}

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
	case 10, 16: // loss, reconciled
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
// (record.go: (96 + body + 7) &^ 7). A file whose first bytes are not the
// DJH1 magic was never taken as a segment — a spare — and nothing walks.
// Each record must name the segment the header names and its own offset
// where the walk reads it (DecodeRecordHeader's place rules), so a recycled
// segment's stale bytes stop the walk instead of reading as live records.
// An untaken zero tail ends a healthy walk; a DJS1 slot is a seal; anything
// else is torn. The two checksum slots are read but not verified (see the
// file note).
func scanSegment(path string) ([]dumpRecord, endState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, endTorn, err
	}
	if len(b) < dumpSegmentHeaderSize {
		return nil, endUntaken, nil
	}
	if string(b[0:4]) != "DJH1" { // segheader.go: the segment magic
		return nil, endUntaken, nil
	}
	segID := binary.LittleEndian.Uint64(b[24:32]) // segheader.go: the id column

	var recs []dumpRecord
	end := endUntaken
	off := int64(dumpSegmentHeaderSize)
	for {
		if off+dumpRecordHeaderSize > int64(len(b)) {
			// A trailing partial slot: an unwritten zero tail is one more
			// untaken slot even when it is shorter than a header; anything
			// else is torn.
			end = endUntaken
			if off < int64(len(b)) && !allZero(b[off:]) {
				end = endTorn
			}
			break
		}
		h := b[off : off+dumpRecordHeaderSize]
		switch {
		case allZero(h): // scan.go: an untaken slot, not damage
			end = endUntaken
			goto done
		case string(h[0:4]) == "DJS1": // seal.go: the seal marker's magic
			end = endSealed
			goto done
		case string(h[0:4]) != "DJR1": // record.go: the record magic
			end = endTorn
			goto done
		}
		kind := h[4]
		if kind < 1 || kind > 16 { // kinds.go: known()
			end = endTorn
			goto done
		}
		r := dumpRecord{
			kind:    kind,
			file:    [16]byte(h[16:32]),
			off:     int64(binary.LittleEndian.Uint64(h[32:40])),
			length:  int64(binary.LittleEndian.Uint64(h[40:48])),
			seq:     binary.LittleEndian.Uint64(h[48:56]),
			epoch:   binary.LittleEndian.Uint64(h[56:64]),
			counter: binary.LittleEndian.Uint64(h[64:72]),
			tag:     binary.LittleEndian.Uint32(h[72:76]),
			recOff:  off,
		}
		// The place rules first: a record naming another segment, or its
		// own offset somewhere else, was written somewhere else.
		if binary.LittleEndian.Uint64(h[8:16]) != segID || int64(binary.LittleEndian.Uint32(h[88:92])) != off {
			end = endTorn
			goto done
		}
		body := bodySize(kind, r.length)
		if r.length < 0 || body < 0 || off+dumpRecordHeaderSize+body > int64(len(b)) {
			end = endTorn
			goto done
		}
		r.body = b[off+dumpRecordHeaderSize : off+dumpRecordHeaderSize+body]
		r.bodyOff = off + dumpRecordHeaderSize
		recs = append(recs, r)
		off += (dumpRecordHeaderSize + body + 7) &^ 7
	}
done:
	return recs, end, nil
}

// allZero answers whether every byte of b is zero — a never-taken slot.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// viewDump is the (d) view: every record of every segment, decoded, with
// the payload bytes showing — the journal's on-disk truth.
func (d *Demo) viewDump() {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		d.Printf("data dump: %v\n", err)
		return
	}
	var segs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".seg") {
			segs = append(segs, e.Name())
		}
	}
	sort.Strings(segs)
	d.Printf("records on disk (checksums not verified by this decoder — the seed is format internals)\n")
	if len(segs) == 0 {
		d.Printf("no segments yet\n")
		return
	}
	rows := 0
	for _, name := range segs {
		recs, end, err := scanSegment(filepath.Join(d.Dir, name))
		if err != nil {
			d.Printf("%s: %v\n", name, err)
			continue
		}
		if len(recs) == 0 && end == endUntaken {
			d.Printf("  %s: spare, never taken\n", name)
			continue
		}
		for _, r := range recs {
			rows++
			text := fmt.Sprintf("%-9s %-10s %s: %3d–%-3d v%-3d tag %d  seq %d  @%d",
				name, r.kindName(), fileHexShort(r.file), r.off, r.off+r.length, r.counter, r.tag, r.seq, r.recOff)
			d.Printf("  %s\n", text)
			if len(r.body) > 0 {
				d.Hexdump(r.body, 8+8) // 16 lanes
			}
		}
		switch end {
		case endUntaken: // an active segment's unwritten tail: not a note
			if len(recs) == 0 {
				d.Printf("  %s: spare, never taken\n", name)
			}
		case endSealed:
			if len(recs) == 0 {
				d.Printf("  %s: sealed empty\n", name)
			}
			d.Printf("  %s: sealed here; %d records before it\n", name, len(recs))
		case endTorn:
			d.Printf("  %s: tail torn at the next slot; %d records before it\n", name, len(recs))
		}
	}
	if rows == 0 {
		d.Printf("no records walked\n")
	}
}

// hexdump prints up to dumpBodyLimit bytes of b with an ASCII lane beside
// them; a truncated body says what remains.
const dumpBodyLimit = 512

func (d *Demo) Hexdump(b []byte, wide int) {
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
		d.Printf("      %-48s  %s\n", hexes.String(), ascii.String())
	}
	if len(b) > limit {
		d.Printf("      (+%d more bytes)\n", len(b)-limit)
	}
}

// fileHexShort renders a record's file identity the way views do.
func fileHexShort(f [16]byte) string {
	return hex.EncodeToString(f[0:8])
}
