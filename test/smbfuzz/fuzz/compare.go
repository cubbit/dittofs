//go:build linux

package fuzz

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cubbit/dittofs/test/smbfuzz/sd"
)

// Divergence is the first difference of a round.
type Divergence struct {
	Step  string // "03" or "S"
	Sig   string // "open:errno:EACCES/OK"
	Field string
	Ref   string
	Dut   string
}

// comparer compares one round's results, keeping the per-side state the
// relative comparisons need (DESIGN.md §9.4).
type comparer struct {
	profile  string
	res      *sd.Resolver
	prev     [2]map[string]int64  // side -> "path|field" -> last value, in 100 ns units
	ino      [2]map[string]uint64 // side -> path -> inode
	slotPath map[string]string
}

func newComparer(profile string, res *sd.Resolver) *comparer {
	return &comparer{
		profile:  profile,
		res:      res,
		prev:     [2]map[string]int64{{}, {}},
		ino:      [2]map[string]uint64{{}, {}},
		slotPath: map[string]string{},
	}
}

func (c *comparer) pathOf(s Step) string {
	if p, ok := s.Args["path"]; ok {
		return p
	}
	if slot, ok := s.Args["slot"]; ok {
		return c.slotPath[slot]
	}
	return ""
}

func div(s Step, field, ref, dut string) *Divergence {
	return &Divergence{Sig: s.Op + ":" + field, Field: field, Ref: ref, Dut: dut}
}

// classed is div with the signature narrowed by class, so that a known entry
// for it names one situation rather than every difference in the field
// (DESIGN.md §12.2).
func classed(s Step, field, class, ref, dut string) *Divergence {
	d := div(s, field, ref, dut)
	d.Sig += ":" + class
	return d
}

// under maps p to its place under to when p is from or below it.
func under(p, from, to string) (string, bool) {
	if p == from {
		return to, true
	}
	if strings.HasPrefix(p, from+"/") {
		return to + p[len(from):], true
	}
	return "", false
}

// aliasOf reports whether p is of, or below it, compared without case,
// component by component.
func aliasOf(p, of string) bool {
	pc, oc := strings.Split(p, "/"), strings.Split(of, "/")
	if len(pc) < len(oc) {
		return false
	}
	for i := range oc {
		if !strings.EqualFold(pc[i], oc[i]) {
			return false
		}
	}
	return true
}

// forget drops what the comparer knows about paths whose object went away,
// every path below them, and every name that differs from them only in case.
//
// decision: names are matched without case whatever the share's case rule.
// Both shares are mounted without POSIX extensions, so both servers compare
// names without case, and a file's case alias (F0 of f0) names the object
// that went away; a stale entry for it would relate a number one server
// reuses to a name gone. On a case-sensitive server the match only forgets
// more than needed, which loses a comparison and never makes one up.
func (c *comparer) forget(paths ...string) {
	for _, p := range paths {
		for i := range c.ino {
			for k := range c.ino[i] {
				if aliasOf(k, p) {
					delete(c.ino[i], k)
				}
			}
			for k := range c.prev[i] {
				kp, _, _ := strings.Cut(k, "|")
				if aliasOf(kp, p) {
					delete(c.prev[i], k)
				}
			}
		}
	}
}

// move carries what the comparer knows about from, and every path below it,
// to to, after a rename: the objects are the same, so a later look at them
// under their new names compares against what was seen under the old ones —
// a time a rename must not move shows as unchanged. What was known under the
// case aliases of from, and under to and its aliases, is forgotten.
func (c *comparer) move(from, to string) {
	if from == to {
		return
	}
	var moved [2]map[string]uint64
	var prev [2]map[string]int64
	for i := range c.ino {
		moved[i], prev[i] = map[string]uint64{}, map[string]int64{}
		for k, v := range c.ino[i] {
			if np, ok := under(k, from, to); ok {
				delete(c.ino[i], k)
				moved[i][np] = v
			}
		}
		for k, v := range c.prev[i] {
			kp, field, _ := strings.Cut(k, "|")
			if np, ok := under(kp, from, to); ok {
				delete(c.prev[i], k)
				prev[i][np+"|"+field] = v
			}
		}
	}
	c.forget(from, to)
	for i := range c.ino {
		for k, v := range moved[i] {
			c.ino[i][k] = v
		}
		for k, v := range prev[i] {
			c.prev[i][k] = v
		}
	}
	for slot, p := range c.slotPath {
		if np, ok := under(p, from, to); ok {
			c.slotPath[slot] = np
		}
	}
}

// seed records a time the plan set, on both sides: the next look compares
// against it, so a server that ignored or altered the set diverges from one
// that kept it.
func (c *comparer) seed(path, field string, v int64) {
	c.prev[0][path+"|"+field], c.prev[1][path+"|"+field] = v, v
}

func (c *comparer) compare(s Step, ref, dut Reply) (*Divergence, []string) {
	if n := seekDataNote(s, ref, dut); n != "" {
		return nil, []string{n}
	}
	if ref.Errno != dut.Errno {
		d := div(s, "errno", errName(ref.Errno), errName(dut.Errno))
		d.Sig = fmt.Sprintf("%s:errno:%s/%s", s.Op, d.Ref, d.Dut)
		// A hang on one side is signed by the side alone (DESIGN.md §9.5):
		// what the other side answered does not make it another hang, and a
		// minimiser that kept the other side's errno in the signature would
		// reject a shrink that changed only that.
		switch errTimeout {
		case ref.Errno:
			d.Sig = s.Op + ":timeout:" + SideRef
		case dut.Errno:
			d.Sig = s.Op + ":timeout:" + SideDut
		}
		return d, nil
	}
	if ref.Errno != 0 {
		// An ACE edit that read the descriptor and failed to write it still
		// compares what it read.
		if isACEEdit(s.Op) && ref.Out != nil && dut.Out != nil {
			return c.compareSD(s, ref.Out.Bytes, dut.Out.Bytes), nil
		}
		return nil, nil
	}
	path := c.pathOf(s)
	switch s.Op {
	case "open":
		c.slotPath[s.Out] = s.Args["path"]
	case "unlink", "rmdir":
		c.forget(path)
	case "rename":
		c.move(path, s.Args["path2"])
	case "utimensat":
		if t, err := parseTime(s.Args["mtime"]); err == nil && !t.Omit && !t.Now {
			c.seed(path, "mtime", (t.Sec*1e9+t.Nsec)/100)
		}
	case "setbtime":
		if t, err := parseTime(s.Args["t"]); err == nil {
			c.seed(path, "btime", (t.Sec*1e9+t.Nsec)/100)
			c.seed(path, "ftbtime", int64(fileTime(t)))
		}
	}
	ro, do := ref.Out, dut.Out
	if ro == nil {
		ro = &Output{}
	}
	if do == nil {
		do = &Output{}
	}
	var notes []string
	switch s.Op {
	case "pread":
		if ro.N != do.N {
			return div(s, "count", fmt.Sprint(ro.N), fmt.Sprint(do.N)), nil
		}
		if ro.Hash != do.Hash {
			at := "?"
			for i := range ro.Blocks {
				if i >= len(do.Blocks) || ro.Blocks[i] != do.Blocks[i] {
					at = fmt.Sprintf("block of 64KiB #%d (offset %d)", i, int64(i)*hashBlock)
					break
				}
			}
			return div(s, "data", "sha "+ro.Hash, "sha "+do.Hash+", first difference in "+at), nil
		}
	case "pwrite", "write", "copy_file_range":
		if ro.N != do.N {
			return div(s, "count", fmt.Sprint(ro.N), fmt.Sprint(do.N)), nil
		}
	case "lseek":
		if ro.N != do.N {
			if w := s.Args["whence"]; w == "SEEK_DATA" || w == "SEEK_HOLE" {
				// ponytail: an allocation-dependent answer that differs is
				// recorded as a note without the admissibility check of
				// DESIGN.md §9.4 (a reported hole must read as zeros). Add it
				// when notes start hiding real differences.
				notes = append(notes, fmt.Sprintf("%s: ref %d, dut %d (allocation-dependent)", w, ro.N, do.N))
			} else {
				return div(s, "offset", fmt.Sprint(ro.N), fmt.Sprint(do.N)), nil
			}
		}
	case "readdir":
		if rd, dd := sillyNames(ro.Dir), sillyNames(do.Dir); !slices.Equal(rd, dd) {
			return div(s, "listing", fmtDir(rd), fmtDir(dd)), nil
		}
	case "stat", "lstat", "fstat":
		if ro.Stat == nil || do.Stat == nil {
			return div(s, "stat", fmt.Sprint(ro.Stat), fmt.Sprint(do.Stat)), nil
		}
		if d, n := c.compareStat(s, path, ro.Stat, do.Stat); d != nil {
			return d, nil
		} else {
			notes = append(notes, n...)
		}
	case "getdosattr":
		ra, da := le32(ro.Bytes), le32(do.Bytes)
		if ra != da {
			return classed(s, "attrs", formatFlags(uint64(ra^da), dosAttrNames),
				formatFlags(uint64(ra), dosAttrNames), formatFlags(uint64(da), dosAttrNames)), nil
		}
	case "getbtime":
		rv, dv := int64(le64(ro.Bytes)), int64(le64(do.Bytes))
		if d := c.compareTime(s, path, "ftbtime", rv, dv, 1); d != nil {
			return d, nil
		}
	case "aceadd", "acedel", "aceedit":
		if d := c.compareSD(s, ro.Bytes, do.Bytes); d != nil {
			return d, nil
		}
	case "getxattr", "getacl":
		if s.Op == "getacl" {
			if d := c.compareSD(s, ro.Bytes, do.Bytes); d != nil {
				return d, nil
			}
		} else if !slices.Equal(ro.Bytes, do.Bytes) {
			return div(s, "value", short(ro.Bytes), short(do.Bytes)), nil
		}
	case "listxattr":
		if !slices.Equal(ro.Names, do.Names) {
			return div(s, "names", strings.Join(ro.Names, ","), strings.Join(do.Names, ",")), nil
		}
	case "testlock":
		if fmt.Sprint(ro.Lock) != fmt.Sprint(do.Lock) {
			return div(s, "lock", fmt.Sprint(ro.Lock), fmt.Sprint(do.Lock)), nil
		}
	}
	return nil, notes
}

// seekDataNote returns the note for a SEEK_DATA answered ENXIO on one side
// and with an offset on the other, the same allocation difference as two
// different offsets (DESIGN.md §9.4): Samba reports ext4's allocation, where
// written zeros are data, DittoFS reports an all-zero cluster as a hole, and
// the client turns a sparse file's empty allocated range into ENXIO. SEEK_HOLE
// is not excused: its ENXIO means an offset at or past the size, which both
// sides agree on.
func seekDataNote(s Step, ref, dut Reply) string {
	if s.Op != "lseek" || s.Args["whence"] != "SEEK_DATA" {
		return ""
	}
	enxio := int(unix.ENXIO)
	if (ref.Errno != enxio || dut.Errno != 0) && (ref.Errno != 0 || dut.Errno != enxio) {
		return ""
	}
	answer := func(r Reply) string {
		if r.Errno == enxio {
			return "ENXIO"
		}
		if r.Out == nil {
			return "0"
		}
		return fmt.Sprint(r.Out.N)
	}
	return fmt.Sprintf("SEEK_DATA: ref %s, dut %s (allocation-dependent)", answer(ref), answer(dut))
}

func (c *comparer) compareStat(s Step, path string, r, d *StatOut) (*Divergence, []string) {
	if r.Type != d.Type {
		return div(s, "type", r.Type, d.Type), nil
	}
	// The object's type narrows a field's signature: a directory's size
	// and a file's are different situations.
	if r.Size != d.Size {
		return classed(s, "size", r.Type, fmt.Sprint(r.Size), fmt.Sprint(d.Size)), nil
	}
	if r.Nlink != d.Nlink {
		return classed(s, "nlink", r.Type, fmt.Sprint(r.Nlink), fmt.Sprint(d.Nlink)), nil
	}
	if c.profile == "posix" {
		if r.Perm != d.Perm {
			return classed(s, "perm", r.Type, fmt.Sprintf("%04o", r.Perm), fmt.Sprintf("%04o", d.Perm)), nil
		}
		if r.UID != d.UID || r.GID != d.GID {
			return classed(s, "owner", r.Type, fmt.Sprintf("%d:%d", r.UID, r.GID), fmt.Sprintf("%d:%d", d.UID, d.GID)), nil
		}
	}
	if dv := c.compareTime(s, path, "mtime", r.Mtime, d.Mtime, 100); dv != nil {
		return dv, nil
	}
	// decision: ctime is not compared. Samba reports a file's ChangeTime as
	// its modification time, while DittoFS reports when the file last
	// changed, metadata included, as Windows does; every ACL, attribute or
	// rename step and every first look at a tree object would diverge on a
	// difference of semantics, not a bug. Compare it again against a reference
	// whose ChangeTime moves on metadata changes (a Windows server).
	if r.HasBtime != d.HasBtime {
		return div(s, "btime", fmt.Sprint(r.HasBtime), fmt.Sprint(d.HasBtime)), nil
	}
	if r.HasBtime {
		if dv := c.compareTime(s, path, "btime", r.Btime, d.Btime, 100); dv != nil {
			return dv, nil
		}
	}
	// Inodes compare as a relation: paths sharing an inode on one side must
	// share it on the other. A path's own earlier number, carried by a
	// rename, is one of them: it is related before the look overwrites it,
	// so a rename that changed the object's id on one side diverges.
	if pr, ok := c.ino[0][path]; ok {
		if pd, ok := c.ino[1][path]; ok && (pr == r.Ino) != (pd == d.Ino) {
			return div(s, "inode", fmt.Sprintf("same as before: %v", pr == r.Ino),
				fmt.Sprintf("same as before: %v", pd == d.Ino)), nil
		}
	}
	c.ino[0][path], c.ino[1][path] = r.Ino, d.Ino
	for other, ri := range c.ino[0] {
		if other == path {
			continue
		}
		di, ok := c.ino[1][other]
		if !ok {
			continue
		}
		if (ri == r.Ino) != (di == d.Ino) {
			return div(s, "inode", fmt.Sprintf("same as %s: %v", other, ri == r.Ino),
				fmt.Sprintf("same as %s: %v", other, di == d.Ino)), nil
		}
	}
	var notes []string
	if (r.Blocks == 0) != (d.Blocks == 0) || r.Blocks != d.Blocks {
		notes = append(notes, fmt.Sprintf("blocks: ref %d, dut %d (allocation-dependent)", r.Blocks, d.Blocks))
	}
	return nil, notes
}

// timeSlack is how far apart two first looks at a time may be, in 100 ns
// units. Each step runs on the reference and then on DittoFS, so an object
// the plan creates gets its times on the two sides a fraction of a second
// apart; one second covers that without hiding a time that is wrong.
const timeSlack = 10_000_000

// compareTime compares a timestamp (DESIGN.md §9.4). A first look at it
// compares the two values within timeSlack; every later look compares how it
// moved since the previous one on its own side — unchanged, advanced or went
// back — because the two servers stamp the same event at slightly different
// instants. A time the plan set (utimensat, setbtime, the tree's setup) is
// recorded as the previous value on both sides, so "unchanged" after it means
// both kept exactly what was set. unit converts the value to 100 ns units.
func (c *comparer) compareTime(s Step, path, field string, r, d int64, unit int64) *Divergence {
	key := path + "|" + field
	r, d = r/unit, d/unit
	pr, okr := c.prev[0][key]
	pd, okd := c.prev[1][key]
	c.prev[0][key], c.prev[1][key] = r, d
	if !okr || !okd {
		if diff := r - d; diff > timeSlack || diff < -timeSlack {
			dv := div(s, field, fmt.Sprint(r), fmt.Sprint(d))
			dv.Sig = fmt.Sprintf("%s:%s:apart", s.Op, field)
			return dv
		}
		return nil
	}
	rel := func(p, v int64) string {
		switch {
		case p == v:
			return "unchanged"
		case v > p:
			return "advanced"
		}
		return "went-back"
	}
	rr, dr := rel(pr, r), rel(pd, d)
	if rr != dr {
		dv := div(s, field, rr, dr)
		dv.Sig = fmt.Sprintf("%s:%s:%s/%s", s.Op, field, rr, dr)
		return dv
	}
	return nil
}

// compareSD compares a descriptor part by part, so a signature names the
// part that differs: owner, group, dacl, sacl or control.
func (c *comparer) compareSD(s Step, rb, db []byte) *Divergence {
	rd, rerr := sd.Unmarshal(rb)
	dd, derr := sd.Unmarshal(db)
	if rerr != nil || derr != nil {
		if !slices.Equal(rb, db) {
			return div(s, "sd-unparseable", c.formatSD(rb), c.formatSD(db))
		}
		return nil
	}
	name := func(x *sd.SID) string {
		if x == nil {
			return "-"
		}
		return c.res.Name(*x)
	}
	// An owner or group difference is signed by the two trustees, which are
	// normalised names (§9.3), and an ACL difference by the part of the
	// first entry that differs.
	if a, b := name(rd.Owner), name(dd.Owner); a != b {
		return classed(s, "owner", a+"/"+b, a, b)
	}
	if a, b := name(rd.Group), name(dd.Group); a != b {
		return classed(s, "group", a+"/"+b, a, b)
	}
	acl := func(d *sd.SD, dacl bool) string {
		x := &sd.SD{Control: d.Control}
		if dacl {
			x.DACL = d.DACL
			x.Control &^= sd.SACLPresent | sd.SACLProtected | sd.SACLAutoInherited | sd.SACLAutoInheritReq
		} else {
			x.SACL = d.SACL
			x.Control &^= sd.DACLPresent | sd.DACLProtected | sd.DACLAutoInherited | sd.DACLAutoInheritReq
		}
		return c.res.Format(x)
	}
	if a, b := acl(rd, true), acl(dd, true); a != b {
		return classed(s, "dacl", c.aclDiff(rd.DACL, dd.DACL), a, b)
	}
	if a, b := acl(rd, false), acl(dd, false); a != b {
		return classed(s, "sacl", c.aclDiff(rd.SACL, dd.SACL), a, b)
	}
	return nil
}

// aclDiff names what differs between two ACLs whose formatted forms differ:
// one is null, the first entry that differs (its type, flags, mask or
// trustee), the number of entries, or else the control flags.
func (c *comparer) aclDiff(r, d *sd.ACL) string {
	if (r == nil) != (d == nil) {
		return "null"
	}
	if r == nil {
		return "control"
	}
	for i := range min(len(r.ACEs), len(d.ACEs)) {
		a, b := r.ACEs[i], d.ACEs[i]
		switch {
		case a.Type != b.Type:
			return "type"
		case a.Flags != b.Flags:
			return "flags"
		case a.Mask != b.Mask:
			return "mask"
		case c.res.Name(a.SID) != c.res.Name(b.SID):
			return "trustee"
		case !slices.Equal(a.Raw, b.Raw):
			return "body"
		}
	}
	if len(r.ACEs) != len(d.ACEs) {
		return "count"
	}
	return "control"
}

func (c *comparer) formatSD(b []byte) string {
	d, err := sd.Unmarshal(b)
	if err != nil {
		return "unparseable:" + hex.EncodeToString(b)
	}
	return c.res.Format(d)
}

// sillyNames replaces the counter in the name the kernel client gives a file
// unlinked while open (".__smb005B"): the counter is the client's, not the
// server's, and differs between the two mounts.
func sillyNames(es []DirEnt) []DirEnt {
	out := make([]DirEnt, len(es))
	for i, e := range es {
		if strings.HasPrefix(e.Name, ".__smb") {
			e.Name = ".__smb*"
		}
		out[i] = e
	}
	slices.SortStableFunc(out, func(a, b DirEnt) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func fmtDir(es []DirEnt) string {
	var parts []string
	for _, e := range es {
		parts = append(parts, e.Name+"("+e.Type+")")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func short(b []byte) string {
	if len(b) > 32 {
		return fmt.Sprintf("%d bytes %x…", len(b), b[:32])
	}
	return fmt.Sprintf("%d bytes %x", len(b), b)
}

func le32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func le64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func isACEEdit(op string) bool { return op == "aceadd" || op == "acedel" || op == "aceedit" }

// IsSDWrite reports whether op writes a security descriptor: the runner reads
// the result back after each one (DESIGN.md §9.1).
func IsSDWrite(op string) bool {
	return op == "setacl" || op == "setowner" || isACEEdit(op)
}
