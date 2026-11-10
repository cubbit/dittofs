//go:build linux

package fuzz

import (
	"io"
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

type remountHooks struct{ remounts *int }

func (remountHooks) CleanRef(string) error       { return nil }
func (remountHooks) ResetDut() error             { return nil }
func (remountHooks) DutAlive() (bool, string)    { return true, "" }
func (remountHooks) Ticket(string, string) error { return nil }
func (h remountHooks) Remount() error            { *h.remounts++; return nil }

// A mount that lost server inode numbers cannot fail the inode relation: the
// round's inode divergence is not run, and both shares are mounted again.
func TestServerinoLossIsNotRun(t *testing.T) {
	const on = "//10.0.0.1/p /s/mnt/ref cifs rw,vers=3.1.1,serverino,soft 0 0\n" +
		"//127.0.0.1/p /s/mnt/dut cifs rw,vers=3.1.1,serverino,soft 0 0\n"
	const refOff = "//10.0.0.1/p /s/mnt/ref cifs rw,vers=3.1.1,noserverino,soft 0 0\n" +
		"//127.0.0.1/p /s/mnt/dut cifs rw,vers=3.1.1,serverino,soft 0 0\n"
	n := 0
	r := &Runner{env: Env{RefRoot: "/s/mnt/ref", DutRoot: "/s/mnt/dut"}, hooks: remountHooks{&n}, log: io.Discard}
	inode := func() *Result { return &Result{Diverge: &Divergence{Sig: "stat:inode", Field: "inode"}} }

	res := inode()
	r.checkServerino(res, on)
	if res.Diverge == nil || res.Err != nil || n != 0 {
		t.Fatalf("both mounts on: diverge %v, err %v, %d remounts", res.Diverge, res.Err, n)
	}
	res = inode()
	r.checkServerino(res, refOff)
	if res.Diverge != nil || res.Err == nil || n != 1 {
		t.Fatalf("ref off: diverge %v, err %v, %d remounts; want not run and one remount", res.Diverge, res.Err, n)
	}
	res = &Result{Diverge: &Divergence{Sig: "stat:size", Field: "size"}}
	r.checkServerino(res, refOff)
	if res.Diverge == nil || res.Err != nil {
		t.Fatalf("ref off: a size divergence became %v / %v", res.Diverge, res.Err)
	}
}

// No directory gets a second name by case alone: neither in the pool nor by
// a rename that changes a directory's case (poolSpecial says why).
func TestNoDirectoryCaseAlias(t *testing.T) {
	for _, d := range Pool {
		for _, o := range Pool {
			if d.Dir && o.Path != d.Path && strings.EqualFold(o.Path, d.Path) {
				t.Errorf("pool directory %s has the case alias %s", d.Path, o.Path)
			}
		}
	}
	for round := uint64(1); round <= 2000; round++ {
		p, _ := Generate(1, round)
		for _, s := range p.Steps {
			if s.Op == "rename" && !poolFile(s.Args["path"]) && s.Args["path2"] != s.Args["path"] &&
				strings.EqualFold(s.Args["path2"], s.Args["path"]) {
				t.Fatalf("round %d renames directory %s to %s", round, s.Args["path"], s.Args["path2"])
			}
		}
	}
}

// A known divergence lets the round go on only for an output difference at a
// step that changes nothing; after differing errnos, or at a step that
// changes the tree, the round ends there, graded known.
func TestKnownDivergenceEndsRoundWhenTreesMaySplit(t *testing.T) {
	known := &Known{Entries: []KnownEntry{
		{Signature: "getacl:owner:alice/bob", Reason: "r"},
		{Signature: "rename:errno:EACCES/OK", Reason: "r"},
		{Signature: "stat:errno:ENOENT/OK", Reason: "r"},
		{Signature: "setacl:readback:dacl", Reason: "r"},
		{Signature: "lseek:offset", Reason: "r"},
	}}
	r := &Runner{known: known}
	for _, c := range []struct {
		op, sig, field string
		ends           bool
	}{
		{"getacl", "getacl:owner:alice/bob", "owner", false},
		{"rename", "rename:errno:EACCES/OK", "errno", true},
		{"stat", "stat:errno:ENOENT/OK", "errno", true},
		{"setacl", "setacl:readback:dacl", "dacl", true}, // judged as the write it reads back
		{"lseek", "lseek:offset", "offset", true},
		{"stat", "stat:size", "size", true}, // not known
	} {
		res := &Result{}
		got := r.ends(res, Step{Op: c.op}, &Divergence{Sig: c.sig, Field: c.field})
		if got != c.ends {
			t.Errorf("%s: ends %v, want %v", c.sig, got, c.ends)
		}
		if isNew := res.Diverge != nil; isNew == known.Has(c.sig) {
			t.Errorf("%s: recorded as new %v", c.sig, isNew)
		}
	}
}

// A plan file's paths, in its steps and in its tree, stay inside the round
// directory: one that is absolute or climbs out with ".." is refused before
// any worker sees it.
func TestPlanPathsStayInTheRound(t *testing.T) {
	r := &Runner{env: Env{RefRoot: "/r", DutRoot: "/d"}, resolver: testResolver()}
	for _, text := range []string{
		"01 admin  stat       ../../etc\n",
		"01 admin  stat       d0/../../x\n",
		"01 admin  rename     d0 ../d0 flags=0\n",
		"01 admin  stat       /etc\n",
		"# tree file ../x len=0 data=zero\n",
	} {
		p, err := ParsePlan(strings.NewReader("# smbfuzz plan gen=1 round=1 root=open\n" + text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		steps := p.Steps
		for _, ts := range p.Tree.SetupSteps() {
			steps = append(steps, ts.Step)
		}
		refused := false
		for i, s := range steps {
			if _, err := r.resolve(1, "r1-a1", SideRef, i+1, s); err != nil {
				refused = true
			}
		}
		if !refused {
			t.Errorf("%q: resolved", text)
		}
	}
	if _, err := r.resolve(1, "r1-a1", SideRef, 1, Step{Op: "stat", Args: map[string]string{"path": "d0/.hidden"}}); err != nil {
		t.Errorf("a path inside the round: %v", err)
	}
}

// A file's case alias names the same object: removing or moving the object
// under its other name forgets what the alias saw, so a number the reference
// reuses is not related to it. And a rename that changes the object's id on
// one side diverges at the next look under the new name.
func TestInodeRelationFollowsAliasesAndRenames(t *testing.T) {
	ok := Reply{}
	stat := func(c *comparer, p string, ref, dut uint64) *Divergence {
		s := Step{User: "bob", Op: "stat", Args: map[string]string{"path": p}}
		d, _ := c.compare(s, Reply{Out: &Output{Stat: &StatOut{Type: "file", Ino: ref}}},
			Reply{Out: &Output{Stat: &StatOut{Type: "file", Ino: dut}}})
		return d
	}
	step := func(c *comparer, op, p, p2 string) {
		a := map[string]string{"path": p}
		if p2 != "" {
			a["path2"] = p2
		}
		if d, _ := c.compare(Step{User: "bob", Op: op, Args: a}, ok, ok); d != nil {
			t.Fatal(d)
		}
	}

	c := newComparer("sd", testResolver())
	if d := stat(c, "d0/F0", 7, 9); d != nil {
		t.Fatal(d)
	}
	step(c, "unlink", "d0/f0", "")
	if d := stat(c, "d0/f1", 7, 11); d != nil {
		t.Fatalf("unlinked under its other name, then reused: %+v", d)
	}

	c = newComparer("sd", testResolver())
	if d := stat(c, "F0", 7, 9); d != nil {
		t.Fatal(d)
	}
	step(c, "rename", "f0", "d1/f0")
	step(c, "unlink", "d1/f0", "")
	if d := stat(c, "f1", 7, 11); d != nil {
		t.Fatalf("moved under its other name, removed, then reused: %+v", d)
	}

	c = newComparer("sd", testResolver())
	if d := stat(c, "f0", 7, 9); d != nil {
		t.Fatal(d)
	}
	step(c, "rename", "f0", "f1")
	if d := stat(c, "f1", 7, 10); d == nil || d.Sig != "stat:inode" {
		t.Fatalf("a rename changed the id on DittoFS only: got %+v", d)
	}
}
