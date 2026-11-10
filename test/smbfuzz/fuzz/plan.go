//go:build linux

package fuzz

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A parameter's kind decides how it is written: positional kinds appear bare
// in schema order, the rest as key=value.
type paramKind int

const (
	kPath paramKind = iota
	kSlot
	kFlags
	kMode
	kSize
	kData
	kEnum // whence, fallocate mode, rename flags, xattr flags, lock kind, flock how, part, bool
	kTime
	kDOS
	kXName
	kXValue
	kSD
	kTrustee
)

type param struct {
	name string
	kind paramKind
}

func (p param) positional() bool {
	switch p.kind {
	case kPath, kSlot, kFlags, kMode, kSD, kTrustee:
		return true
	}
	return false
}

// OpSpec describes one operation of DESIGN.md §7.
type OpSpec struct {
	Name   string
	Params []param
	Opens  bool // fills a slot: "-> user.hN"
	// Handle is true when the first parameter is a slot, so the step runs
	// as that slot's user.
	Handle bool
}

var opList = []OpSpec{
	{Name: "open", Params: []param{{"path", kPath}, {"flags", kFlags}, {"mode", kMode}}, Opens: true},
	{Name: "close", Params: []param{{"slot", kSlot}}, Handle: true},
	{Name: "pread", Params: []param{{"slot", kSlot}, {"off", kSize}, {"len", kSize}}, Handle: true},
	{Name: "pwrite", Params: []param{{"slot", kSlot}, {"off", kSize}, {"len", kSize}, {"data", kData}}, Handle: true},
	{Name: "write", Params: []param{{"slot", kSlot}, {"len", kSize}, {"data", kData}}, Handle: true},
	{Name: "lseek", Params: []param{{"slot", kSlot}, {"off", kSize}, {"whence", kEnum}}, Handle: true},
	{Name: "ftruncate", Params: []param{{"slot", kSlot}, {"len", kSize}}, Handle: true},
	{Name: "truncate", Params: []param{{"path", kPath}, {"len", kSize}}},
	{Name: "fallocate", Params: []param{{"slot", kSlot}, {"mode", kEnum}, {"off", kSize}, {"len", kSize}}, Handle: true},
	{Name: "setsparse", Params: []param{{"path", kPath}, {"on", kEnum}}},
	{Name: "fsync", Params: []param{{"slot", kSlot}}, Handle: true},
	{Name: "fdatasync", Params: []param{{"slot", kSlot}}, Handle: true},
	{Name: "copy_file_range", Params: []param{{"slot", kSlot}, {"off", kSize}, {"slot2", kSlot}, {"off2", kSize}, {"len", kSize}}, Handle: true},
	{Name: "ficlone_range", Params: []param{{"slot", kSlot}, {"off", kSize}, {"slot2", kSlot}, {"off2", kSize}, {"len", kSize}}, Handle: true},
	{Name: "mkdir", Params: []param{{"path", kPath}, {"mode", kMode}}},
	{Name: "rmdir", Params: []param{{"path", kPath}}},
	{Name: "unlink", Params: []param{{"path", kPath}}},
	{Name: "rename", Params: []param{{"path", kPath}, {"path2", kPath}, {"flags", kEnum}}},
	{Name: "link", Params: []param{{"path", kPath}, {"path2", kPath}}},
	{Name: "readdir", Params: []param{{"path", kPath}}},
	{Name: "statfs", Params: []param{{"path", kPath}}},
	// mkfiles is the tree setup's: it creates count filler entries of the
	// wide directory, FillName(from) onwards, and is never drawn.
	{Name: "mkfiles", Params: []param{{"path", kPath}, {"from", kEnum}, {"count", kEnum}}},
	{Name: "stat", Params: []param{{"path", kPath}}},
	{Name: "lstat", Params: []param{{"path", kPath}}},
	{Name: "fstat", Params: []param{{"slot", kSlot}}, Handle: true},
	{Name: "utimensat", Params: []param{{"path", kPath}, {"atime", kTime}, {"mtime", kTime}}},
	{Name: "getdosattr", Params: []param{{"path", kPath}}},
	{Name: "setdosattr", Params: []param{{"path", kPath}, {"attrs", kDOS}}},
	{Name: "getbtime", Params: []param{{"path", kPath}}},
	{Name: "setbtime", Params: []param{{"path", kPath}, {"t", kTime}}},
	{Name: "getxattr", Params: []param{{"path", kPath}, {"name", kXName}}},
	{Name: "setxattr", Params: []param{{"path", kPath}, {"name", kXName}, {"value", kXValue}, {"flags", kEnum}}},
	{Name: "removexattr", Params: []param{{"path", kPath}, {"name", kXName}}},
	{Name: "listxattr", Params: []param{{"path", kPath}}},
	{Name: "chmod", Params: []param{{"path", kPath}, {"mode", kMode}}},
	{Name: "chown", Params: []param{{"path", kPath}, {"owner", kTrustee}, {"group", kTrustee}}},
	{Name: "lock", Params: []param{{"slot", kSlot}, {"kind", kEnum}, {"off", kSize}, {"len", kSize}}, Handle: true},
	{Name: "unlock", Params: []param{{"slot", kSlot}, {"off", kSize}, {"len", kSize}}, Handle: true},
	{Name: "testlock", Params: []param{{"slot", kSlot}, {"kind", kEnum}, {"off", kSize}, {"len", kSize}}, Handle: true},
	{Name: "flock", Params: []param{{"slot", kSlot}, {"how", kEnum}}, Handle: true},
	{Name: "getacl", Params: []param{{"path", kPath}, {"part", kEnum}}},
	{Name: "setacl", Params: []param{{"path", kPath}, {"part", kEnum}, {"sd", kSD}}},
	{Name: "setowner", Params: []param{{"path", kPath}, {"owner", kTrustee}, {"group", kTrustee}}},
	{Name: "aceadd", Params: []param{{"path", kPath}, {"part", kEnum}, {"index", kEnum}, {"ace", kSD}}},
	{Name: "acedel", Params: []param{{"path", kPath}, {"part", kEnum}, {"index", kEnum}}},
	{Name: "aceedit", Params: []param{{"path", kPath}, {"part", kEnum}, {"index", kEnum}, {"mask", kEnum}, {"aceflags", kEnum}}},
}

// Ops indexes opList by name.
var Ops = func() map[string]*OpSpec {
	m := map[string]*OpSpec{}
	for i := range opList {
		m[opList[i].Name] = &opList[i]
	}
	return m
}()

// Step is one line of a plan.
type Step struct {
	User string
	Op   string
	Args map[string]string
	Out  string // the slot an open fills, "alice.h0"
}

// Arg returns an argument or "".
func (s Step) Arg(name string) string { return s.Args[name] }

// Clone returns a deep copy.
func (s Step) Clone() Step {
	c := s
	c.Args = make(map[string]string, len(s.Args))
	for k, v := range s.Args {
		c.Args[k] = v
	}
	return c
}

// Plan is a header and its steps.
type Plan struct {
	Gen   int
	Round uint64
	Root  string // the round root's descriptor, by name (DESIGN.md §10)
	Tree  Tree   // what exists before step 1 (DESIGN.md §6.3)
	Steps []Step
}

// Clone returns a deep copy.
func (p *Plan) Clone() *Plan {
	c := *p
	c.Tree = p.Tree.Clone()
	c.Steps = make([]Step, len(p.Steps))
	for i, s := range p.Steps {
		c.Steps[i] = s.Clone()
	}
	return &c
}

func quoteIfNeeded(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\"'=#") || strings.ContainsAny(v, "\n") {
		return strconv.Quote(v)
	}
	return v
}

// FormatStep writes one step line, numbered n.
func FormatStep(n int, s Step) string {
	spec := Ops[s.Op]
	var b strings.Builder
	fmt.Fprintf(&b, "%02d %-6s %-10s", n, s.User, s.Op)
	if spec == nil {
		return b.String() + " ?"
	}
	for _, p := range spec.Params {
		v, ok := s.Args[p.name]
		if !ok {
			continue
		}
		if p.positional() {
			b.WriteString(" " + quoteIfNeeded(v))
		} else {
			b.WriteString(" " + p.name + "=" + quoteIfNeeded(v))
		}
	}
	if s.Out != "" {
		b.WriteString(" -> " + s.Out)
	}
	return b.String()
}

// Format writes the whole plan.
func (p *Plan) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# smbfuzz plan gen=%d round=%d root=%s\n", p.Gen, p.Round, p.Root)
	for _, o := range p.Tree {
		b.WriteString(o.format() + "\n")
	}
	for i, s := range p.Steps {
		b.WriteString(FormatStep(i+1, s) + "\n")
	}
	return b.String()
}

// tokenize splits a line on blanks, honouring Go-quoted strings, which may
// also appear as the value of key="...".
func tokenize(line string) ([]string, error) {
	var out []string
	i := 0
	for i < len(line) {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		j := i
		var tok strings.Builder
		for j < len(line) && line[j] != ' ' && line[j] != '\t' {
			if line[j] == '"' {
				q, err := strconv.QuotedPrefix(line[j:])
				if err != nil {
					return nil, fmt.Errorf("bad quoted string at %q", line[j:])
				}
				uq, _ := strconv.Unquote(q)
				tok.WriteString("\x00" + uq) // marks a quoted part
				j += len(q)
				continue
			}
			tok.WriteByte(line[j])
			j++
		}
		out = append(out, tok.String())
		i = j
	}
	return out, nil
}

// ParsePlan reads a plan file.
func ParsePlan(r io.Reader) (*Plan, error) {
	p := &Plan{Gen: 1, Root: "open"}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# tree ") {
			o, err := parseTreeLine(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", ln, err)
			}
			p.Tree = append(p.Tree, o)
			continue
		}
		if strings.HasPrefix(line, "#") {
			for _, f := range strings.Fields(line) {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					continue
				}
				switch k {
				case "gen":
					n, err := strconv.Atoi(v)
					if err != nil {
						return nil, fmt.Errorf("line %d: gen: %w", ln, err)
					}
					p.Gen = n
				case "round":
					n, err := strconv.ParseUint(v, 10, 64)
					if err != nil {
						return nil, fmt.Errorf("line %d: round: %w", ln, err)
					}
					p.Round = n
				case "root":
					p.Root = v
				}
			}
			continue
		}
		s, err := parseStep(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		p.Steps = append(p.Steps, s)
	}
	return p, sc.Err()
}

func parseStep(line string) (Step, error) {
	toks, err := tokenize(line)
	if err != nil {
		return Step{}, err
	}
	if len(toks) < 3 {
		return Step{}, fmt.Errorf("short step %q", line)
	}
	s := Step{User: toks[1], Op: toks[2], Args: map[string]string{}}
	spec := Ops[s.Op]
	if spec == nil {
		return Step{}, fmt.Errorf("unknown op %q", s.Op)
	}
	rest := toks[3:]
	if n := len(rest); n >= 2 && rest[n-2] == "->" {
		s.Out = rest[n-1]
		rest = rest[:n-2]
	}
	var positional []param
	byName := map[string]param{}
	for _, p := range spec.Params {
		byName[p.name] = p
		if p.positional() {
			positional = append(positional, p)
		}
	}
	for _, t := range rest {
		quoted := strings.HasPrefix(t, "\x00")
		if !quoted {
			if k, v, ok := strings.Cut(t, "="); ok {
				if _, known := byName[k]; known {
					s.Args[k] = strings.ReplaceAll(v, "\x00", "")
					continue
				}
			}
		}
		t = strings.ReplaceAll(t, "\x00", "")
		assigned := false
		for _, p := range positional {
			if _, done := s.Args[p.name]; !done {
				s.Args[p.name] = t
				assigned = true
				break
			}
		}
		if !assigned {
			return Step{}, fmt.Errorf("extra argument %q", t)
		}
	}
	for _, p := range spec.Params {
		if _, ok := s.Args[p.name]; !ok {
			return Step{}, fmt.Errorf("%s: missing %s", s.Op, p.name)
		}
	}
	if spec.Opens && s.Out == "" {
		return Step{}, fmt.Errorf("open without -> slot")
	}
	return s, nil
}

// SlotUser returns the user part of "alice.h0".
func SlotUser(slot string) string {
	u, _, _ := strings.Cut(slot, ".")
	return u
}
