//go:build linux

package fuzz

import (
	"path"
	"strings"
	"testing"
)

// A rename of a pool directory to another name at its depth maps every pool
// name below it onto a pool name, so later steps can still reach what moved.
func TestPoolClosedUnderSameDepthRename(t *testing.T) {
	for _, o := range Pool {
		if !o.Dir || !strings.HasPrefix(o.Path, "d") {
			continue
		}
		targets := elsewhere(o.Path)
		if s := sibling(o.Path); s != "" {
			targets = append(targets, s)
		}
		if len(targets) == 0 {
			t.Fatalf("%s has no target at its depth", o.Path)
		}
		for _, dst := range targets {
			for _, q := range Pool {
				if strings.HasPrefix(q.Path, o.Path+"/") && !inPool(dst+strings.TrimPrefix(q.Path, o.Path)) {
					t.Errorf("rename %s -> %s: %s leaves the pool", o.Path, dst, q.Path)
				}
			}
		}
	}
}

// Every tree object's parent is created before it, the tree formats and
// parses back, and the wide directory never creates the name kept for a new
// entry.
func TestTreeShape(t *testing.T) {
	for round := uint64(1); round <= 2000; round++ {
		p, _ := Generate(1, round)
		seen := map[string]bool{".": true}
		for _, o := range p.Tree {
			if !seen[path.Dir(o.Path)] {
				t.Fatalf("round %d: %s before its parent", round, o.Path)
			}
			if !inPool(o.Path) || !poolCreatable(o.Path) || o.Path == WideDir+"/new" {
				t.Fatalf("round %d: tree creates %s", round, o.Path)
			}
			seen[o.Path] = true
		}
		q, err := ParsePlan(strings.NewReader(p.Format()))
		if err != nil || len(q.Tree) != len(p.Tree) {
			t.Fatalf("round %d: %v\n%s", round, err, p.Format())
		}
	}
}

// The case the tree exists for is reached: a rename whose source is a
// directory the tree filled with a subdirectory holding a file with content,
// and one with a file open below it.
func TestNestedDirectoryRenameIsReached(t *testing.T) {
	nested, openBelow := 0, 0
	const rounds = 2000
	for round := uint64(1); round <= rounds; round++ {
		p, _ := Generate(1, round)
		opened := map[string]bool{}
		for _, s := range p.Steps {
			if s.Op == "open" {
				opened[s.Args["path"]] = true
			}
			if s.Op != "rename" {
				continue
			}
			src := s.Args["path"]
			for _, o := range p.Tree {
				if rest, ok := strings.CutPrefix(o.Path, src+"/"); ok && strings.Contains(rest, "/") && !o.Dir && o.Len != "0" {
					nested++
					break
				}
			}
			for q := range opened {
				if strings.HasPrefix(q, src+"/") {
					openBelow++
					break
				}
			}
		}
	}
	if nested < rounds/20 || openBelow < rounds/40 {
		t.Fatalf("over %d rounds: %d renames of a directory with a nested file, %d with an open file below", rounds, nested, openBelow)
	}
}

// After a rename the comparer looks at the moved objects under their new
// names against what it saw under the old ones, so a time the rename moved
// on one side only diverges; and what was below a removed name is forgotten.
func TestComparerFollowsRename(t *testing.T) {
	c := newComparer("sd", testResolver())
	ok := Reply{}
	stat := func(p string, ref, dut float64) *Divergence {
		s := Step{User: "bob", Op: "stat", Args: map[string]string{"path": p}}
		d, _ := c.compare(s, Reply{Out: &Output{Stat: &StatOut{Type: "file", Mtime: int64(ref * 1e9), Ino: 7}}},
			Reply{Out: &Output{Stat: &StatOut{Type: "file", Mtime: int64(dut * 1e9), Ino: 9}}})
		return d
	}
	if d := stat("d0/d1/f0", 1000, 1000); d != nil {
		t.Fatal(d)
	}
	mv := Step{User: "bob", Op: "rename", Args: map[string]string{"path": "d0", "path2": "d1"}}
	if d, _ := c.compare(mv, ok, ok); d != nil {
		t.Fatal(d)
	}
	if d := stat("d1/d1/f0", 1000, 1000.3); d == nil || d.Sig != "stat:mtime:unchanged/advanced" {
		t.Fatalf("moved file: got %+v", d)
	}
	if _, ok := c.prev[0]["d0/d1/f0|mtime"]; ok {
		t.Fatal("the old name is still known")
	}
	rm := Step{User: "bob", Op: "rmdir", Args: map[string]string{"path": "d1"}}
	c.compare(rm, ok, ok)
	if len(c.ino[0]) != 0 || len(c.prev[0]) != 0 {
		t.Fatalf("names below a removed directory are still known: %v %v", c.ino[0], c.prev[0])
	}
}

// A time the plan set is the base of the next look: both sides keeping it is
// unchanged, one side ignoring it diverges.
func TestComparerSeedsSetTimes(t *testing.T) {
	c := newComparer("sd", testResolver())
	set := Step{User: "admin", Op: "utimensat", Args: map[string]string{"path": "f0", "atime": "OMIT", "mtime": "1500000000.123456700"}}
	c.compare(set, Reply{}, Reply{})
	st := Step{User: "bob", Op: "stat", Args: map[string]string{"path": "f0"}}
	kept := &StatOut{Type: "file", Mtime: 1500000000123456700}
	ignored := &StatOut{Type: "file", Mtime: 1700000000000000000}
	if d, _ := c.compare(st, Reply{Out: &Output{Stat: kept}}, Reply{Out: &Output{Stat: ignored}}); d == nil || d.Sig != "stat:mtime:unchanged/advanced" {
		t.Fatalf("got %+v", d)
	}
}

// After a descriptor write, a getacl of that path by another user is
// reached: whether the new descriptor lets them read it is compared.
func TestOtherUserReadsWrittenACL(t *testing.T) {
	reads, edits := 0, 0
	const rounds = 2000
	for round := uint64(1); round <= rounds; round++ {
		p, _ := Generate(1, round)
		writer := map[string]string{}
		for _, s := range p.Steps {
			switch s.Op {
			case "setacl", "setowner", "aceadd", "acedel", "aceedit":
				writer[s.Args["path"]] = s.User
				if s.Op != "setacl" && s.Op != "setowner" {
					edits++
				}
			case "getacl":
				if w, ok := writer[s.Args["path"]]; ok && w != s.User {
					reads++
				}
			}
		}
	}
	if reads < rounds/10 || edits < rounds/2 {
		t.Fatalf("over %d rounds: %d reads by another user after a write, %d ACE edits", rounds, reads, edits)
	}
}
