// The (d) view reads the journal's segment files with the journal's own
// scanner, re-exported by pkg/block/journal/walk for this throwaway demo
// branch, so what it shows is the layout the journal reads: a record that
// does not verify is not shown. This file decodes no byte of its own.
package jdemo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal/walk"
)

// dumpSeg is what reading one segment file found: the real scan, the body
// bytes behind each scanned record (the scan drops payloads), and whether the
// walk ended on bytes that are neither a record, a seal marker nor zeros.
type dumpSeg struct {
	Spare  bool // never taken as a segment: a zero header
	Scan   walk.Scan
	Bodies [][]byte
	Torn   bool
}

// readSegment scans the segment file at path under the journal identity id.
func readSegment(path string, id walk.JournalID) (dumpSeg, error) {
	f, err := os.Open(path)
	if err != nil {
		return dumpSeg{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return dumpSeg{}, err
	}
	hdr := make([]byte, walk.SegmentHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil && !errors.Is(err, io.EOF) {
		return dumpSeg{}, err
	}
	h, err := walk.DecodeSegmentHeader(hdr, id)
	if err != nil {
		if strings.Trim(string(hdr), "\x00") == "" {
			return dumpSeg{Spare: true}, nil
		}
		return dumpSeg{}, err
	}
	sc, scanErr := walk.ScanSegment(f, st.Size(), id, h.ID)
	seg := dumpSeg{Scan: sc, Torn: scanErr != nil}
	for _, r := range sc.Ranges {
		if r.Kind == walk.RangeBad {
			seg.Torn = true
		}
	}
	for _, r := range sc.Records {
		body := make([]byte, r.Header.BodySize())
		if _, err := f.ReadAt(body, r.Header.Offset+walk.RecordHeaderSize); err != nil && len(body) > 0 {
			return dumpSeg{}, err
		}
		seg.Bodies = append(seg.Bodies, body)
	}
	return seg, nil
}

// viewDump is the (d) view: every record of every segment, decoded, with
// the payload bytes showing — the journal's on-disk truth.
func (d *Demo) viewDump() {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		d.Printf("data dump: %v\n", err)
		return
	}
	st, err := LoadState(StatePath(d.Dir))
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
	d.Printf("records on disk, as the journal's own scanner reads them (checksums verified)\n")
	if len(segs) == 0 {
		d.Printf("no segments yet\n")
		return
	}
	rows := 0
	for _, name := range segs {
		seg, err := readSegment(filepath.Join(d.Dir, name), st.ID)
		if err != nil {
			d.Printf("%s: %v\n", name, err)
			continue
		}
		recs := seg.Scan.Records
		if seg.Spare || (len(recs) == 0 && !seg.Scan.Sealed && !seg.Torn) {
			d.Printf("  %s: spare, never taken\n", name)
			continue
		}
		for i, r := range recs {
			rows++
			h := r.Header
			d.Printf("  %-9s %-10s %s: %3d–%-3d v%-3d tag %d  seq %d  @%d\n",
				name, h.Kind, fileHexShort(h.File.Bytes()), h.Extent.Off, h.Extent.Off+h.Extent.Len,
				h.Version.Counter, h.Tag, h.Seq, h.Offset)
			if len(seg.Bodies[i]) > 0 {
				d.Hexdump(seg.Bodies[i], 8+8) // 16 lanes
			}
		}
		switch {
		case seg.Scan.Sealed:
			if len(recs) == 0 {
				d.Printf("  %s: sealed empty\n", name)
			}
			d.Printf("  %s: sealed here; %d records before it\n", name, len(recs))
		case seg.Torn:
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
