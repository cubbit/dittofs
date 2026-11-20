package datapath

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// taggedWriters lists, per source file, how many call sites append a record to
// a stream, and how each one reaches the share's tally. Every record a stream
// takes lands in the set under its tag, so a call site that is not counted
// through Grant.Wrote, Capacity.Landed or the repack copy ledger leaves the
// tally below the set's figure and the share's limit fails open once the
// record's segment retires.
var taggedWriters = map[string]struct {
	sites int
	route string
}{
	"datapath/write.go":      {1, "Grant.Wrote"},
	"datapath/fill.go":       {1, "Grant.Wrote"},
	"datapath/removals.go":   {1, "Capacity.Landed"},
	"datapath/forget.go":     {1, "Capacity.Landed"},
	"durability/reappend.go": {1, "durability Config.Landed"},
	"durability/loss.go":     {1, "durability Config.Landed"},
	"reclaim/carry.go":       {4, "repack copy ledger (CopyLanded, CopiesTransfer)"},
	"reclaim/copy.go":        {2, "repack copy ledger (CopyLanded, CopiesTransfer)"},
}

// TestEveryTaggedWriterIsKnown finds the call sites that append a record to a
// stream in the journal's non-test source and compares them with the writers
// whose counting route is known. A new call site fails here until its author
// has wired it to the share's tally and added it to taggedWriters with its
// route, so the next writer cannot forget the count the way the forget record
// once did.
func TestEveryTaggedWriterIsKnown(t *testing.T) {
	root := ".."
	got := map[string]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "segment/stream.go" {
			return nil // the stream's own Append and AppendRoll
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if (sel.Sel.Name == "Append" && len(call.Args) == 2) || (sel.Sel.Name == "AppendRoll" && len(call.Args) == 3) {
				got[rel]++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the journal source: %v", err)
	}
	for file, n := range got {
		w, known := taggedWriters[file]
		if !known {
			t.Errorf("%s has %d stream append call sites, want none: wire each to the share tally (Grant.Wrote, Capacity.Landed or the copy ledger) and list the file in taggedWriters", file, n)
			continue
		}
		if n != w.sites {
			t.Errorf("%s has %d stream append call sites, want %d (counted through %s): check the new one reaches the tally, then update taggedWriters", file, n, w.sites, w.route)
		}
	}
	for file, w := range taggedWriters {
		if got[file] == 0 {
			t.Errorf("%s has no stream append call site, want %d: remove it from taggedWriters", file, w.sites)
		}
	}
}
