package fuzz

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// This file holds a round's object pool and the part of it that exists before
// step 1 (DESIGN.md §6.3). Every path a plan names comes from the pool; the
// tree is the subset admin creates, with content, before the first step, so
// the steps meet populated directories as well as names nothing created.

// WideDir is the pool's large directory, filled with Fill entries named
// FillName(i) that no step names: they exist to make a listing span several
// QUERY_DIRECTORY responses.
const WideDir = "w"

// FillName is the i-th filler entry of the wide directory.
func FillName(i int) string { return fmt.Sprintf("x%04d", i) }

// IsFiller reports whether rel (relative to the round directory) is a filler
// entry of the wide directory.
func IsFiller(rel string) bool {
	dir, name := path.Split(rel)
	if dir != WideDir+"/" || len(name) != 5 || name[0] != 'x' {
		return false
	}
	_, err := strconv.Atoi(name[1:])
	return err == nil
}

// PoolObj is one name of the pool and the kind of object it is meant for. The
// kind steers the draws (a readdir asks for a directory); nothing stops a step
// from creating a file where the pool expects a directory.
type PoolObj struct {
	Path string
	Dir  bool
}

// poolDepth is how many directory levels the pool has. Every level holds
// the same names, so renaming a directory to another name at its depth maps
// every name below it onto another name of the pool.
const poolDepth = 3

var (
	poolDirNames  = []string{"d0", "d1"}
	poolFileNames = []string{"f0", "f1"}
)

// Pool is every name a plan may use, parents before children, in a fixed order.
var Pool = buildPool()

// poolSpecial are the names that test one rule each rather than the tree:
// case aliases of pool names, names awkward on the wire, a 255-byte name.
// The case aliases are never created by the tree, which would make them
// collide with the names they alias.
var poolSpecial = []struct {
	obj       PoolObj
	creatable bool
}{
	{PoolObj{"F0", false}, false},
	{PoolObj{"D0", true}, false},
	{PoolObj{"D0/f0", false}, false},
	{PoolObj{"d0/F0", false}, false},
	{PoolObj{"d1/F0", false}, false},
	{PoolObj{"a b", false}, true},
	{PoolObj{"é", false}, true},
	{PoolObj{"É", false}, false},
	{PoolObj{".hidden", false}, true},
	{PoolObj{"L255", false}, true},
}

func buildPool() []PoolObj {
	var out []PoolObj
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		join := func(n string) string {
			if dir == "" {
				return n
			}
			return dir + "/" + n
		}
		for _, f := range poolFileNames {
			out = append(out, PoolObj{join(f), false})
		}
		if depth == poolDepth {
			return
		}
		for _, d := range poolDirNames {
			out = append(out, PoolObj{join(d), true})
			walk(join(d), depth+1)
		}
	}
	walk("", 0)
	for _, s := range poolSpecial {
		out = append(out, s.obj)
	}
	out = append(out,
		PoolObj{WideDir, true},
		PoolObj{WideDir + "/e0", false}, PoolObj{WideDir + "/e1", false}, PoolObj{WideDir + "/new", false})
	return out
}

func poolCreatable(p string) bool {
	for _, s := range poolSpecial {
		if s.obj.Path == p {
			return s.creatable
		}
	}
	return true
}

// TreeObj is one object created before step 1.
type TreeObj struct {
	Path string
	Dir  bool
	Len  string // files: the content's length, as a size expression
	Data string // files: the content's pattern
	Fill int    // the wide directory: how many filler entries it holds
}

// Tree is what exists in a round directory before step 1, parents before
// children.
type Tree []TreeObj

// Has reports whether p is created by the tree.
func (t Tree) Has(p string) bool {
	for _, o := range t {
		if o.Path == p {
			return true
		}
	}
	return false
}

// Without returns the tree without p and everything below it.
func (t Tree) Without(p string) Tree {
	var out Tree
	for _, o := range t {
		if o.Path == p || strings.HasPrefix(o.Path, p+"/") {
			continue
		}
		out = append(out, o)
	}
	return out
}

// Clone returns a copy.
func (t Tree) Clone() Tree { return append(Tree(nil), t...) }

// fillCounts are the wide directory's sizes. 128 entries already take
// QUERY_DIRECTORY past one response with the client's buffer; 1500 take
// several.
var fillCounts = []WeightedChoice[int]{{0, 60}, {128, 20}, {600, 15}, {1500, 5}}

// drawTree draws the tree: each directory of the pool exists with
// probability 0.85 at the first level and 0.7 below, if its parent exists;
// each file with probability 0.55; each creatable special name with 0.3.
// A file's length is 0 with probability 0.2, a size up to 2^21 (plus a
// second term) otherwise, and with probability 0.05 any write length; its
// content is a §6.6 pattern.
func (g *genState) drawTree() Tree {
	r := g.r
	var t Tree
	exists := map[string]bool{"": true, ".": true}
	special := map[string]bool{}
	for _, s := range poolSpecial {
		special[s.obj.Path] = true
	}
	fill := PickW(r, fillCounts)
	for _, o := range Pool {
		parent := path.Dir(o.Path)
		if !exists[parent] || !poolCreatable(o.Path) {
			continue
		}
		var p float64
		switch {
		case o.Path == WideDir+"/new":
			p = 0 // the wide directory's name for a new entry
		case o.Path == WideDir:
			p = 0.3
			if fill > 0 {
				p = 1
			}
		case special[o.Path]:
			p = 0.3
		case o.Dir && !strings.Contains(o.Path, "/"):
			p = 0.85
		case o.Dir:
			p = 0.7
		case parent == WideDir:
			p = 0.8
		default:
			p = 0.55
		}
		if !r.Chance(p) {
			continue
		}
		exists[o.Path] = true
		obj := TreeObj{Path: o.Path, Dir: o.Dir}
		switch {
		case o.Path == WideDir:
			obj.Fill = fill
		case !o.Dir:
			switch x := r.Float(); {
			case x < 0.2:
				obj.Len = "0"
			case x < 0.25:
				obj.Len = g.length()
			default:
				obj.Len = g.size(2 * mib)
			}
			obj.Data = g.data()
		}
		t = append(t, obj)
	}
	return t
}

// TreeStepBase numbers the tree's setup steps (SetupSteps).
const TreeStepBase = 100

// Fixed times the setup gives object k, so a step's first look at a tree
// object compares exact values on both sides rather than two creation times.
func treeMtime(k int) string { return fmt.Sprintf("%d.123456700", 1500000000+1000*k) }
func treeBtime(k int) string { return fmt.Sprintf("%d.0", 1400000000+1000*k) }

// fillChunk is how many filler entries one setup step creates. It keeps one
// side's step short enough that both sides create entry i within the time
// slack of a first comparison (§9.4).
const fillChunk = 100

// SetupStep is one step of the tree's setup and the step number its data
// pattern uses.
type SetupStep struct {
	N    int
	Step Step
}

// SetupSteps are the admin steps that build the tree, in order: every
// object is created (a file with its content), the wide directory is
// filled, then each object's times are set, deepest first, so a later
// object's creation does not move an earlier one's directory time. Object k
// is written as step TreeStepBase+k, so its bytes differ from every plan
// step's and every other object's under the same pattern.
func (t Tree) SetupSteps() []SetupStep {
	var steps []SetupStep
	add := func(k int, op, out string, kv ...string) {
		s := Step{User: "admin", Op: op, Args: map[string]string{}, Out: out}
		for i := 0; i+1 < len(kv); i += 2 {
			s.Args[kv[i]] = kv[i+1]
		}
		steps = append(steps, SetupStep{TreeStepBase + k, s})
	}
	for k, o := range t {
		if o.Dir {
			add(k, "mkdir", "", "path", o.Path, "mode", "0777")
			for from := 0; from < o.Fill; from += fillChunk {
				add(k, "mkfiles", "", "path", o.Path, "from", strconv.Itoa(from), "count", strconv.Itoa(min(fillChunk, o.Fill-from)))
			}
			continue
		}
		add(k, "open", "admin.tree", "path", o.Path, "flags", "O_WRONLY|O_CREAT|O_EXCL", "mode", "0644")
		if o.Len != "0" && o.Len != "" {
			add(k, "pwrite", "", "slot", "admin.tree", "off", "0", "len", o.Len, "data", o.Data)
		}
		add(k, "close", "", "slot", "admin.tree")
	}
	for k := len(t) - 1; k >= 0; k-- {
		add(k, "utimensat", "", "path", t[k].Path, "atime", treeMtime(k), "mtime", treeMtime(k))
		add(k, "setbtime", "", "path", t[k].Path, "t", treeBtime(k))
	}
	return steps
}

func (o TreeObj) format() string {
	var b strings.Builder
	if o.Dir {
		b.WriteString("# tree dir " + quoteIfNeeded(o.Path))
		if o.Fill > 0 {
			fmt.Fprintf(&b, " fill=%d", o.Fill)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "# tree file %s len=%s data=%s", quoteIfNeeded(o.Path), o.Len, o.Data)
	return b.String()
}

func parseTreeLine(line string) (TreeObj, error) {
	toks, err := tokenize(strings.TrimSpace(strings.TrimPrefix(line, "#")))
	if err != nil {
		return TreeObj{}, err
	}
	if len(toks) < 3 || toks[0] != "tree" || (toks[1] != "dir" && toks[1] != "file") {
		return TreeObj{}, fmt.Errorf("bad tree line %q", line)
	}
	o := TreeObj{Dir: toks[1] == "dir", Path: strings.ReplaceAll(toks[2], "\x00", "")}
	for _, t := range toks[3:] {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return TreeObj{}, fmt.Errorf("bad tree attribute %q", t)
		}
		switch k {
		case "len":
			if _, err := EvalSize(v); err != nil {
				return TreeObj{}, err
			}
			o.Len = v
		case "data":
			o.Data = v
		case "fill":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return TreeObj{}, fmt.Errorf("bad fill %q", v)
			}
			o.Fill = n
		default:
			return TreeObj{}, fmt.Errorf("unknown tree attribute %q", k)
		}
	}
	if !o.Dir && o.Len == "" {
		o.Len, o.Data = "0", "zero"
	}
	return o, nil
}
