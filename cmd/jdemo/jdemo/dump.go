// The (d) view reads the journal's segment files with the journal's own
// scanner (pkg/block/journal/walk, a thin seam over the format package), so
// what it shows is the layout the journal itself reads: a record that does
// not verify is not shown, a tail is classified by what the scan found, and
// nothing here decodes a byte of its own.
package jdemo

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal/walk"
)

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
		seg, err := walk.File(filepath.Join(d.Dir, name), [16]byte(st.ID))
		if err != nil {
			d.Printf("%s: %v\n", name, err)
			continue
		}
		if seg.Spare || (len(seg.Records) == 0 && seg.Tail == walk.Untaken) {
			d.Printf("  %s: spare, never taken\n", name)
			continue
		}
		for _, r := range seg.Records {
			rows++
			d.Printf("  %-9s %-10s %s: %3d–%-3d v%-3d tag %d  seq %d  @%d\n",
				name, r.Kind, fileHexShort(r.File), r.Off, r.Off+r.Len, r.Counter, r.Tag, r.Seq, r.RecOff)
			if len(r.Body) > 0 {
				d.Hexdump(r.Body, 8+8) // 16 lanes
			}
		}
		switch seg.Tail {
		case walk.Sealed:
			if len(seg.Records) == 0 {
				d.Printf("  %s: sealed empty\n", name)
			}
			d.Printf("  %s: sealed here; %d records before it\n", name, len(seg.Records))
		case walk.Torn:
			d.Printf("  %s: tail torn at the next slot; %d records before it\n", name, len(seg.Records))
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
