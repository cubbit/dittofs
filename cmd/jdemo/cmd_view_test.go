package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestRenderIndexEntries(t *testing.T) {
	fileHex := "64656d6f"
	es := []indexRow{
		{file: fileHex, entry: journal.IndexEntry{
			Kind:     journal.EntryHeld,
			Extent:   journal.Extent{Off: 0, Len: 5},
			Version:  journal.Version{Counter: 1},
			Tag:      1,
			Location: journal.RecordRef{Segment: 3, Offset: 4096},
		}},
		{file: fileHex, entry: journal.IndexEntry{
			Kind:      journal.EntryMarker,
			Extent:    journal.Extent{Off: 8, Len: 2},
			Version:   journal.Version{Counter: 7, Epoch: 0},
			Tag:       2,
			Offloaded: false,
		}},
		{file: fileHex, entry: journal.IndexEntry{
			Kind:      journal.EntryRetained,
			Extent:    journal.Extent{Off: 16, Len: 3},
			Version:   journal.Version{Counter: 4},
			Tag:       1,
			Offloaded: true,
		}},
	}
	var buf bytes.Buffer
	renderIndexRows(&buf, es)
	out := buf.String()
	for _, want := range []struct {
		field, val string
	}{
		{"held row present as the first word of a line", "held"},
		{"the held extent's range", "0–5"},
		{"the held entry's version", "v1"},
		{"the held entry's tag", "1"},
		{"the record location's segment and offset", "3/4096"},
		{"the retained row", "retained"},
		{"the retained entry's offloaded bit", "yes"},
		{"the marker row", "marker"},
	} {
		if !strings.Contains(out, want.val) {
			t.Errorf("renderIndexEntries: output lacks %s (%q):\n%s", want.field, want.val, out)
		}
	}
}

func TestRenderSegments(t *testing.T) {
	segs := []journal.SegmentInfo{
		{ID: 1, Stream: 0, Sealed: true, Allocated: 268435456, HeldBytes: 5, RetainedBytes: 9},
		{ID: 2, Stream: 1, Failed: true, Damaged: true, Allocated: 1, HeldBytes: 2, RetainedBytes: 0},
	}
	var buf bytes.Buffer
	renderSegments(&buf, segs)
	out := buf.String()
	for _, want := range []string{"1", "0", "sealed", "268435456", "2", "failed", "damaged"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderSegments: output lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderVerify(t *testing.T) {
	rep := verifyJSON{Unsupported: 1, Unindexed: 1, BadFooters: 1, Mismatch: 123}
	var buf bytes.Buffer
	renderVerify(&buf, rep)
	out := buf.String()
	for _, want := range []string{"1 unsupported", "1 unindexed", "1 bad footer", "123"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderVerify: output lacks %q:\n%s", want, out)
		}
	}
}

func TestViewerRoutes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Dir: dir, SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown(context.Background())

	file := hex.EncodeToString([]byte("viewer-file-test"))
	out, err := exec(t, socket, "share", "--tag", "1")
	if err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	out, err = exec(t, socket, "write-at", "--tag", "1", file, "0", "--str", "hello")
	if err != nil {
		t.Fatalf("write-at: %v (out %q)", err, out)
	}

	// Stats is implemented and reaches the real journal: the write shows in
	// the totals and in share 1's row.
	out, err = exec(t, socket, "stats")
	if err != nil {
		t.Fatalf("stats: %v (out %q)", err, out)
	}
	for _, want := range []string{"used", "held", "spares"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats: output lacks a %q row:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "held") {
			val, cerr := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "held")), 10, 64)
			if cerr != nil || val < 5 {
				t.Errorf("stats held row: got %q, want ≥ 5 after a 5-byte write", line)
			}
		}
	}
	if !strings.Contains(out, "share 1") {
		t.Errorf("stats: per-share table lacks share 1:\n%s", out)
	}

	// The Inspector is declared-but-not-built on this base. File() and
	// Verify() answer ErrNotImplemented, which the viewer surfaces
	// verbatim; All() and Segments() have no error to carry, so they
	// answer nothing — an empty view, printed as such. The rebase that
	// lands T7.6 and E9a flips File/Verify to read-outs and All/Segments
	// to asserted tables.
	if _, err := exec(t, socket, "index", "--file", file); err == nil ||
		!strings.Contains(err.Error(), "not implemented") {
		t.Errorf("index --file: got err %v, want the journal's not-implemented verbatim", err)
	}
	if _, err := exec(t, socket, "verify"); err == nil ||
		!strings.Contains(err.Error(), "not implemented") {
		t.Errorf("verify: got err %v, want the journal's not-implemented verbatim", err)
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"index all", []string{"index"}, "no index entries"},
		{"segments", []string{"segments"}, "no segments"},
	} {
		out, err := exec(t, socket, c.args...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: got out %q err %v, want the empty stub view %q", c.name, out, err, c.want)
		}
	}
}

func TestRenderStatsRowsAreInTagOrder(t *testing.T) {
	// Two panes refreshing during the talk must show the same rows in the
	// same order every time: a map's iteration is not an order, so the
	// renderer makes one.
	st := journal.Stats{Shares: map[journal.ShareTag]journal.ShareStats{
		2: {HeldBytes: 2},
		1: {HeldBytes: 1},
		3: {HeldBytes: 3},
	}}
	var buf bytes.Buffer
	renderStats(&buf, st)
	out := buf.String()
	last := -1
	for _, tag := range []string{"share 1", "share 2", "share 3"} {
		at := strings.Index(out, tag)
		if at < 0 {
			t.Fatalf("renderStats lacks %s:\n%s", tag, out)
		}
		if at < last {
			t.Errorf("renderStats shows %s before the previous row; rows must be in tag order:\n%s", tag, out)
		}
		last = at
	}
}

func TestRenderIndexRowsCarryTheFile(t *testing.T) {
	var a, b [16]byte
	copy(a[:], "index-file-aaaa")
	copy(b[:], "index-file-bbbb")
	rows := []indexRow{
		{file: hex.EncodeToString(a[:8]), entry: journal.IndexEntry{
			Kind: journal.EntryHeld, Extent: journal.Extent{Off: 0, Len: 5}, Version: journal.Version{Counter: 1}, Tag: 1}},
		{file: hex.EncodeToString(b[:8]), entry: journal.IndexEntry{
			Kind: journal.EntryHeld, Extent: journal.Extent{Off: 0, Len: 2}, Version: journal.Version{Counter: 2}, Tag: 1}},
	}
	var buf bytes.Buffer
	renderIndexRows(&buf, rows)
	out := buf.String()
	if !strings.Contains(out, hex.EncodeToString(a[:8])) || !strings.Contains(out, hex.EncodeToString(b[:8])) {
		t.Errorf("renderIndexRows: a file's rows lose which file they belong to:\n%s", out)
	}
}
