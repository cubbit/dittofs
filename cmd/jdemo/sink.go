package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// writeSink persists o's dirty extents under dir, reading each through the
// offer's own reader at its file offsets, and returns the extents it wrote:
// the demo's stand-in for the remote tier's offload commit. One file per
// extent, named by zero-padded range so a pane's ls sorts by offset:
// <dir>/<file hex>/<from>-<to>.dat.
func writeSink(dir string, o journal.Offer) ([]journal.Extent, error) {
	if len(o.Dirty) == 0 {
		return nil, nil
	}
	fileDir := filepath.Join(dir, hexOf(o.ID))
	if err := os.MkdirAll(fileDir, 0o755); err != nil {
		return nil, fmt.Errorf("offload: %s: %w", fileDir, err)
	}
	wrote := make([]journal.Extent, 0, len(o.Dirty))
	for _, d := range o.Dirty {
		b := make([]byte, d.Extent.Len)
		if _, err := o.Offered.ReadAt(b, d.Extent.Off); err != nil && err != io.EOF {
			return wrote, fmt.Errorf("offload: reading %d–%d of %s: %w",
				d.Extent.Off, d.Extent.Off+d.Extent.Len, hexOf(o.ID), err)
		}
		name := filepath.Join(fileDir, pad16(d.Extent.Off)+"-"+pad16(d.Extent.Off+d.Extent.Len)+".dat")
		if err := os.WriteFile(name, b, 0o644); err != nil {
			return wrote, fmt.Errorf("offload: %s: %w", name, err)
		}
		wrote = append(wrote, d.Extent)
	}
	return wrote, nil
}

// pad16 renders a file offset as the 16-digit name fragment the sink gives
// it.
func pad16(n int64) string {
	const digits = "0123456789"
	var out [16]byte
	for i := 15; i >= 0; i-- {
		out[i] = digits[n%10]
		n /= 10
	}
	return string(out[:])
}

// hexOf renders a file id as the 32 hex chars its offload files live under.
func hexOf(id journal.FileID) string {
	b := id.Bytes()
	return hex.EncodeToString(b[:])
}
