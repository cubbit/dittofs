package fuzz

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/test/smbfuzz/sd"
)

// StepsPerPlan is the plan length (RFC 31 §5.2).
const StepsPerPlan = 12

// LatestGen is the newest generator version, the default.
const LatestGen = 1

// Generate returns round's plan for a generator version.
//
// ponytail: a released version is frozen by keeping its code and tables
// untouched, so version 2 will start as a copy of genV1 and the tables it
// reads. Upgrade to versioned tables under one shared drawing code when a
// third version makes the copies a maintenance cost.
func Generate(gen int, round uint64) (*Plan, error) {
	switch gen {
	case 1:
		return genV1(round), nil
	}
	return nil, fmt.Errorf("unknown generator version %d", gen)
}

// RoundRoots are the descriptors a round directory starts with (RFC 31 §10).
var RoundRoots = map[string]string{
	"open":       "D:(A;OICI;FA;;;WD)",
	"owner-only": "D:(A;OICIIO;FA;;;CO)(A;;0x1200a6;;;DU)(A;OICI;FA;;;DA)",
	"protected":  "D:P(A;;FA;;;alice)(A;OICI;FA;;;DA)",
	"empty":      "D:(A;OICI;FA;;;DA)",
}

// The admin-full entry on owner-only, protected and empty roots keeps the
// round removable by the admin's cleanup (RFC 31 §10); without it an
// "empty" root would make cleanup take ownership on every round.

// opWeights are RFC 31 §7.7's weights, flattened: group weight times the
// operation's share of its group.
var opWeights = func() []WeightedChoice[string] {
	groups := []struct {
		w   float64
		ops []WeightedChoice[string]
	}{
		{18, []WeightedChoice[string]{{"open", 3}, {"close", 1}}},
		{20, []WeightedChoice[string]{{"pread", 5}, {"pwrite", 6}, {"write", 2}, {"ftruncate", 2}, {"truncate", 1},
			{"fallocate", 3}, {"lseek", 1}, {"fsync", 0.5}, {"fdatasync", 0.5}, {"copy_file_range", 0.7},
			{"ficlone_range", 0.3}, {"setsparse", 0.5}}},
		{14, []WeightedChoice[string]{{"mkdir", 3}, {"rmdir", 1}, {"unlink", 2}, {"rename", 2}, {"link", 1}, {"readdir", 2}}},
		{12, []WeightedChoice[string]{{"stat", 4}, {"lstat", 1}, {"fstat", 2}, {"utimensat", 1}, {"getdosattr", 1.5},
			{"setdosattr", 2}, {"getbtime", 0.5}, {"setbtime", 0.5}, {"getxattr", 0.5}, {"setxattr", 0.5},
			{"removexattr", 0.5}, {"listxattr", 0.5}, {"chmod", 2}, {"chown", 1}}},
		{14, []WeightedChoice[string]{{"lock", 5}, {"unlock", 3}, {"testlock", 1}, {"flock", 1}}},
		{22, []WeightedChoice[string]{{"getacl", 4}, {"setacl", 4}, {"setowner", 1.5}}},
	}
	var out []WeightedChoice[string]
	for _, g := range groups {
		var sum float64
		for _, o := range g.ops {
			sum += o.W
		}
		for _, o := range g.ops {
			out = append(out, WeightedChoice[string]{o.V, g.w * o.W / sum})
		}
	}
	return out
}()

type slotRef struct{ user, slot, path string }

type lockRef struct{ path, off, len string }

type genState struct {
	r      *RNG
	slots  []slotRef
	paths  []string
	locks  []lockRef
	writes []lockRef         // ranges pwrite and fallocate named, per path
	slotOf map[string]string // slot -> path
}

func genV1(round uint64) *Plan {
	r := NewRNG(round)
	p := &Plan{Gen: 1, Round: round}
	p.Root = PickW(r, []WeightedChoice[string]{{"open", 40}, {"owner-only", 25}, {"protected", 20}, {"empty", 15}})
	g := &genState{r: r, slotOf: map[string]string{}}
	for i := 0; i < StepsPerPlan; i++ {
		user := PickW(r, Users)
		op := PickW(r, opWeights)
		// A handle operation drawn before any open would only hit an empty
		// slot; it becomes an open with probability 0.8 (RFC 31 §5.4).
		if spec := Ops[op]; spec.Handle && len(g.slots) == 0 && r.Chance(0.8) {
			op = "open"
		}
		p.Steps = append(p.Steps, g.step(user, op))
	}
	// At least two distinct users: redraw the last step's user from the
	// others. The step keeps its operation and arguments; a handle step whose
	// slot belonged to the old user now names a slot of the new one.
	users := map[string]bool{}
	for _, s := range p.Steps {
		users[s.User] = true
	}
	if len(users) < 2 {
		last := &p.Steps[len(p.Steps)-1]
		var others []WeightedChoice[string]
		for _, u := range Users {
			if u.V != last.User {
				others = append(others, u)
			}
		}
		nu := PickW(r, others)
		for k, v := range last.Args {
			if (k == "slot" || k == "slot2") && SlotUser(v) == last.User {
				last.Args[k] = nu + strings.TrimPrefix(v, last.User)
			}
		}
		if last.Out != "" {
			last.Out = nu + strings.TrimPrefix(last.Out, last.User)
		}
		last.User = nu
	}
	return p
}

func (g *genState) path() string {
	if len(g.paths) > 0 && g.r.Chance(0.7) {
		return Pick(g.r, g.paths)
	}
	p := PickW(g.r, Paths)
	g.paths = append(g.paths, p)
	return p
}

// slot picks a slot for a handle operation and the user who runs it.
func (g *genState) slot(user string) (string, string) {
	if len(g.slots) > 0 && g.r.Chance(0.85) {
		s := Pick(g.r, g.slots)
		return s.slot, s.user
	}
	return fmt.Sprintf("%s.h%d", user, g.r.Below(4)), user
}

// slotOfUser picks a second slot of the same user, for copy and clone.
func (g *genState) slotOfUser(user string) string {
	var mine []string
	for _, s := range g.slots {
		if s.user == user {
			mine = append(mine, s.slot)
		}
	}
	if len(mine) > 0 && g.r.Chance(0.85) {
		return Pick(g.r, mine)
	}
	return fmt.Sprintf("%s.h%d", user, g.r.Below(4))
}

func (g *genState) point(maxValue int64) string {
	var w []WeightedChoice[string]
	for _, p := range AllPoints {
		if p.Value > maxValue {
			continue
		}
		weight := 1.0
		if p.Value > 4*mib {
			weight = 0.1
		}
		w = append(w, WeightedChoice[string]{p.Name, weight})
	}
	return PickW(g.r, w)
}

// size draws one point (0.5) or a sum of two (0.5). The second term of a sum
// is 1 or -1 with probability 0.25 each, so every point is as likely to be
// met by its neighbours as alone. 0 stays alone: a sum with it is another
// point, or below zero.
func (g *genState) size(maxPoint int64) string {
	a := g.point(maxPoint)
	if a == "0" || !g.r.Chance(0.5) {
		return a
	}
	switch x := g.r.Float(); {
	case x < 0.25:
		return a + MinusOne.Name
	case x < 0.5:
		return a + "+1"
	}
	return a + "+" + g.point(maxPoint)
}

func (g *genState) offset() string { return g.size(1 << 62) }
func (g *genState) length() string { return g.size(8 * mib) }

// rangeFor draws off and len, overlapping an earlier lock on the slot's file
// with probability 0.5 (RFC 31 §5.4).
func (g *genState) rangeFor(slot string) (string, string) {
	path := g.slotOf[slot]
	var mine []lockRef
	for _, l := range g.locks {
		if l.path == path && path != "" {
			mine = append(mine, l)
		}
	}
	if len(mine) > 0 && g.r.Chance(0.5) {
		l := Pick(g.r, mine)
		switch g.r.Below(4) {
		case 0, 1:
			return l.off, l.len
		case 2:
			return l.off, g.length()
		default:
			return l.off + "+1", l.len
		}
	}
	return g.offset(), g.length()
}

// readRange draws a pread's off and len. On a file an earlier pwrite or
// fallocate named, by any user through any slot, it starts with probability
// 0.6 where that range starts (0.5), one byte into it (0.25) or at its last
// byte (0.25), and reads that range's length or a fresh one with probability
// 0.5 each. Otherwise it is rangeFor's draw.
func (g *genState) readRange(slot string) (string, string) {
	path := g.slotOf[slot]
	var mine []lockRef
	for _, w := range g.writes {
		if w.path == path && path != "" {
			mine = append(mine, w)
		}
	}
	if len(mine) == 0 || !g.r.Chance(0.6) {
		return g.rangeFor(slot)
	}
	w := Pick(g.r, mine)
	off := w.off
	switch g.r.Below(4) {
	case 0, 1:
	case 2:
		off = w.off + "+1"
	default:
		if last := w.off + "+" + w.len + "-1"; mustEval(last) >= 0 {
			off = last
		}
	}
	if g.r.Chance(0.5) {
		return off, w.len
	}
	return off, g.length()
}

func mustEval(expr string) int64 {
	v, err := EvalSize(expr)
	if err != nil {
		panic(err)
	}
	return v
}

func (g *genState) data() string { return Pick(g.r, DataPatterns) }

func (g *genState) time() string {
	return Pick(g.r, TimeSeconds) + "." + Pick(g.r, TimeNanos)
}

func (g *genState) step(user, op string) Step {
	r := g.r
	s := Step{User: user, Op: op, Args: map[string]string{}}
	a := s.Args
	handle := func() string {
		slot, u := g.slot(user)
		s.User = u
		a["slot"] = slot
		return slot
	}
	switch op {
	case "open":
		path := g.path()
		a["path"] = path
		a["flags"] = g.openFlags()
		a["mode"] = Pick(r, Modes)
		s.Out = fmt.Sprintf("%s.h%d", user, r.Below(4))
		g.slots = append(g.slots, slotRef{user, s.Out, path})
		g.slotOf[s.Out] = path
	case "close", "fsync", "fdatasync", "fstat":
		handle()
	case "pread":
		slot := handle()
		a["off"], a["len"] = g.readRange(slot)
	case "pwrite":
		slot := handle()
		a["off"], a["len"] = g.rangeFor(slot)
		a["data"] = g.data()
		g.writes = append(g.writes, lockRef{g.slotOf[slot], a["off"], a["len"]})
	case "write":
		handle()
		a["len"] = g.length()
		a["data"] = g.data()
	case "lseek":
		handle()
		a["off"] = g.offset()
		a["whence"] = PickW(r, []WeightedChoice[string]{{"SEEK_END", 40}, {"SEEK_DATA", 30}, {"SEEK_HOLE", 30}})
	case "ftruncate":
		slot := handle()
		_, a["len"] = g.rangeFor(slot)
	case "truncate":
		a["path"] = g.path()
		a["len"] = g.offset()
	case "fallocate":
		slot := handle()
		a["mode"] = PickW(r, []WeightedChoice[string]{{"0", 20}, {"KEEP_SIZE", 15}, {"PUNCH_HOLE|KEEP_SIZE", 30},
			{"ZERO_RANGE", 15}, {"ZERO_RANGE|KEEP_SIZE", 10}, {"COLLAPSE_RANGE", 5}, {"INSERT_RANGE", 5}})
		a["off"], a["len"] = g.rangeFor(slot)
		g.writes = append(g.writes, lockRef{g.slotOf[slot], a["off"], a["len"]})
	case "setsparse":
		s.User = "admin"
		a["path"] = g.path()
		a["on"] = Pick(r, []string{"true", "false"})
	case "copy_file_range", "ficlone_range":
		slot := handle()
		a["off"], a["len"] = g.rangeFor(slot)
		a["slot2"] = g.slotOfUser(s.User)
		a["off2"] = g.offset()
	case "mkdir":
		a["path"] = g.path()
		a["mode"] = Pick(r, Modes)
	case "rmdir", "unlink", "readdir", "stat", "lstat", "getdosattr", "getbtime", "listxattr":
		a["path"] = g.path()
	case "rename":
		a["path"] = g.path()
		a["path2"] = g.path()
		a["flags"] = PickW(r, []WeightedChoice[string]{{"0", 75}, {"RENAME_NOREPLACE", 25}})
	case "link":
		a["path"] = g.path()
		a["path2"] = g.path()
	case "utimensat":
		a["path"] = g.path()
		a["atime"] = g.timeOrSpecial()
		a["mtime"] = g.timeOrSpecial()
	case "setdosattr":
		a["path"] = g.path()
		a["attrs"] = g.dosAttrs()
	case "setbtime":
		a["path"] = g.path()
		a["t"] = g.time()
	case "getxattr", "removexattr":
		a["path"] = g.path()
		a["name"] = Pick(r, XattrNames)
	case "setxattr":
		a["path"] = g.path()
		a["name"] = Pick(r, XattrNames)
		a["value"] = g.data() + ":" + Pick(r, XattrLens)
		a["flags"] = Pick(r, []string{"0", "XATTR_CREATE", "XATTR_REPLACE"})
	case "chmod":
		a["path"] = g.path()
		a["mode"] = Pick(r, Modes)
	case "chown":
		a["path"] = g.path()
		a["owner"] = g.ownerOrKeep()
		a["group"] = g.ownerOrKeep()
	case "lock", "testlock":
		slot := handle()
		a["kind"] = PickW(r, []WeightedChoice[string]{{"shared", 45}, {"excl", 55}})
		a["off"], a["len"] = g.rangeFor(slot)
		if op == "lock" {
			g.locks = append(g.locks, lockRef{g.slotOf[slot], a["off"], a["len"]})
		}
	case "unlock":
		slot := handle()
		a["off"], a["len"] = g.rangeFor(slot)
	case "flock":
		handle()
		a["how"] = PickW(r, []WeightedChoice[string]{{"LOCK_SH|LOCK_NB", 40}, {"LOCK_EX|LOCK_NB", 40}, {"LOCK_UN", 20}})
	case "getacl":
		a["path"] = g.path()
		a["part"] = PickW(r, []WeightedChoice[string]{{"dacl", 30}, {"ntsd", 50}, {"ntsd_full", 20}})
	case "setacl":
		a["path"] = g.path()
		part := PickW(r, []WeightedChoice[string]{{"dacl", 30}, {"ntsd", 50}, {"ntsd_full", 20}})
		a["part"] = part
		a["sd"] = g.descriptor(part)
	case "setowner":
		a["path"] = g.path()
		a["owner"] = PickW(r, Owners)
		a["group"] = PickW(r, Owners)
	default:
		panic("fuzz: no generator for " + op)
	}
	return s
}

func (g *genState) openFlags() string {
	r := g.r
	parts := []string{PickW(r, []WeightedChoice[string]{{"O_RDONLY", 40}, {"O_WRONLY", 25}, {"O_RDWR", 35}})}
	creat := r.Chance(0.5)
	if creat {
		parts = append(parts, "O_CREAT")
	}
	if (creat && r.Chance(0.3)) || (!creat && r.Chance(0.05)) {
		parts = append(parts, "O_EXCL")
	}
	for _, f := range []struct {
		n string
		p float64
	}{{"O_TRUNC", 0.2}, {"O_APPEND", 0.15}, {"O_DIRECTORY", 0.1}, {"O_NOFOLLOW", 0.05}, {"O_SYNC", 0.05}, {"O_DSYNC", 0.05}} {
		if r.Chance(f.p) {
			parts = append(parts, f.n)
		}
	}
	return strings.Join(parts, "|")
}

func (g *genState) timeOrSpecial() string {
	switch g.r.Below(10) {
	case 0:
		return "OMIT"
	case 1:
		return "NOW"
	}
	return g.time()
}

func (g *genState) ownerOrKeep() string {
	if g.r.Chance(0.3) {
		return "-1"
	}
	return PickW(g.r, Owners)
}

func (g *genState) dosAttrs() string {
	if g.r.Chance(0.1) {
		return "0"
	}
	var parts []string
	for _, f := range dosAttrNames {
		p := 0.05
		if settableDOS[f.name] {
			p = 0.25
		}
		if g.r.Chance(p) {
			parts = append(parts, f.name)
		}
	}
	if g.r.Chance(0.05) {
		parts = append(parts, "0x400000")
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

// descriptor draws a security descriptor for a set of the given part
// (RFC 31 §8), as SDDL with symbolic trustees.
func (g *genState) descriptor(part string) string {
	r := g.r
	var b strings.Builder
	if part != "dacl" && r.Chance(0.5) {
		b.WriteString("O:" + PickW(r, Owners))
		b.WriteString("G:" + PickW(r, Owners))
	}
	b.WriteString("D:")
	if r.Chance(0.3) {
		b.WriteString("P")
	}
	if r.Chance(0.1) {
		b.WriteString("AR")
	}
	if r.Chance(0.3) {
		b.WriteString("AI")
	}
	switch {
	case r.Chance(0.03):
		b.WriteString("NO_ACCESS_CONTROL")
	case r.Chance(0.03):
		// empty DACL
	default:
		n := g.aceCount()
		for i := 0; i < n; i++ {
			b.WriteString(g.ace(false))
		}
	}
	if part == "ntsd_full" && !r.Chance(0.3) {
		b.WriteString("S:")
		if r.Chance(0.3) {
			b.WriteString("P")
		}
		if r.Chance(0.3) {
			b.WriteString("AI")
		}
		n := int(Pick(r, []int{0, 1, 2, 3, 4, 8, 16}))
		for i := 0; i < n; i++ {
			b.WriteString(g.ace(true))
		}
	}
	return b.String()
}

func (g *genState) aceCount() int {
	r := g.r
	switch x := r.Below(10); {
	case x == 0:
		return Pick(r, []int{128, 256, 512, 1000})
	case x == 1:
		return Pick(r, []int{8, 12, 16, 32, 64, 100})
	}
	return Pick(r, []int{1, 2, 3, 4, 5, 6})
}

var rightBits = []uint32{
	0x1, 0x2, 0x4, 0x8, 0x10, 0x20, 0x40, 0x80, 0x100,
	0x10000, 0x20000, 0x40000, 0x80000, 0x100000,
}

func (g *genState) ace(sacl bool) string {
	r := g.r
	if sacl && r.Chance(0.15) {
		label := Pick(r, []string{"LW", "ME", "HI", "SI"})
		var mask uint32
		for _, bit := range []uint32{1, 2, 4} {
			if r.Chance(0.5) {
				mask |= bit
			}
		}
		return fmt.Sprintf("(ML;;0x%x;;;%s)", mask, label)
	}
	typ := "A"
	if sacl {
		typ = "AU"
	} else if r.Chance(0.4) {
		typ = "D"
	}
	var mask uint32
	for _, bit := range rightBits {
		if r.Chance(0.35) {
			mask |= bit
		}
	}
	if mask == 0 {
		mask = Pick(r, rightBits)
	}
	if r.Chance(0.1) {
		mask |= Pick(r, []uint32{0x10000000, 0x80000000, 0x40000000, 0x20000000})
	}
	if r.Chance(0.03) {
		mask |= 0x200
	}
	var flags byte
	for _, f := range []struct {
		bit byte
		p   float64
	}{{sd.ObjectInherit, 0.3}, {sd.ContainerInherit, 0.3}, {sd.NoPropagateInherit, 0.15}, {sd.InheritOnly, 0.15}, {sd.Inherited, 0.1}} {
		if r.Chance(f.p) {
			flags |= f.bit
		}
	}
	if sacl {
		if r.Chance(0.6) {
			flags |= sd.SuccessfulAccess
		}
		if r.Chance(0.6) {
			flags |= sd.FailedAccess
		}
	}
	return fmt.Sprintf("(%s;%s;0x%x;;;%s)", typ, aceFlagText(flags), mask, PickW(r, Trustees))
}

func aceFlagText(f byte) string {
	names := []struct {
		bit byte
		n   string
	}{{sd.ObjectInherit, "OI"}, {sd.ContainerInherit, "CI"}, {sd.NoPropagateInherit, "NP"},
		{sd.InheritOnly, "IO"}, {sd.Inherited, "ID"}, {sd.SuccessfulAccess, "SA"}, {sd.FailedAccess, "FA"}}
	var b strings.Builder
	for _, n := range names {
		if f&n.bit != 0 {
			b.WriteString(n.n)
		}
	}
	return b.String()
}
