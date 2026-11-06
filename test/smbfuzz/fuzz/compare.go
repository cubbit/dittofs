package fuzz

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

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
// relative comparisons need (RFC 31 §9.4).
type comparer struct {
	profile  string
	res      *sd.Resolver
	prev     [2]map[string]int64  // side -> "path|field" -> last value
	explicit map[string]bool      // "path|field" set to an exact value by the plan
	ino      [2]map[string]uint64 // side -> path -> inode
	slotPath map[string]string
}

func newComparer(profile string, res *sd.Resolver) *comparer {
	return &comparer{
		profile:  profile,
		res:      res,
		prev:     [2]map[string]int64{{}, {}},
		explicit: map[string]bool{},
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

// forget drops what the comparer knows about a path whose identity changed.
func (c *comparer) forget(paths ...string) {
	for _, p := range paths {
		for i := range c.ino {
			delete(c.ino[i], p)
			for k := range c.prev[i] {
				if strings.HasPrefix(k, p+"|") {
					delete(c.prev[i], k)
				}
			}
		}
		for k := range c.explicit {
			if strings.HasPrefix(k, p+"|") {
				delete(c.explicit, k)
			}
		}
	}
}

func (c *comparer) compare(s Step, ref, dut Reply) (*Divergence, []string) {
	if ref.Errno != dut.Errno {
		d := div(s, "errno", errName(ref.Errno), errName(dut.Errno))
		d.Sig = fmt.Sprintf("%s:errno:%s/%s", s.Op, d.Ref, d.Dut)
		return d, nil
	}
	if ref.Errno != 0 {
		return nil, nil
	}
	path := c.pathOf(s)
	switch s.Op {
	case "open":
		c.slotPath[s.Out] = s.Args["path"]
	case "unlink", "rmdir":
		c.forget(path)
	case "rename":
		c.forget(path, s.Args["path2"])
	case "utimensat":
		if a := s.Args["mtime"]; a != "OMIT" && a != "NOW" {
			c.explicit[path+"|mtime"] = true
		}
	case "setbtime":
		c.explicit[path+"|btime"] = true
		c.explicit[path+"|ftbtime"] = true
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
				// RFC 31 §9.4 (a reported hole must read as zeros). Add it
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
			return div(s, "attrs", formatFlags(uint64(ra), dosAttrNames), formatFlags(uint64(da), dosAttrNames)), nil
		}
	case "getbtime":
		rv, dv := int64(le64(ro.Bytes)), int64(le64(do.Bytes))
		if d := c.compareTime(s, path, "ftbtime", rv, dv, 1); d != nil {
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

func (c *comparer) compareStat(s Step, path string, r, d *StatOut) (*Divergence, []string) {
	if r.Type != d.Type {
		return div(s, "type", r.Type, d.Type), nil
	}
	if r.Size != d.Size {
		return div(s, "size", fmt.Sprint(r.Size), fmt.Sprint(d.Size)), nil
	}
	if r.Nlink != d.Nlink {
		return div(s, "nlink", fmt.Sprint(r.Nlink), fmt.Sprint(d.Nlink)), nil
	}
	if c.profile == "posix" {
		if r.Perm != d.Perm {
			return div(s, "perm", fmt.Sprintf("%04o", r.Perm), fmt.Sprintf("%04o", d.Perm)), nil
		}
		if r.UID != d.UID || r.GID != d.GID {
			return div(s, "owner", fmt.Sprintf("%d:%d", r.UID, r.GID), fmt.Sprintf("%d:%d", d.UID, d.GID)), nil
		}
	}
	if dv := c.compareTime(s, path, "mtime", r.Mtime, d.Mtime, 100); dv != nil {
		return dv, nil
	}
	if dv := c.compareTime(s, path, "ctime", r.Ctime, d.Ctime, 100); dv != nil {
		return dv, nil
	}
	if r.HasBtime != d.HasBtime {
		return div(s, "btime", fmt.Sprint(r.HasBtime), fmt.Sprint(d.HasBtime)), nil
	}
	if r.HasBtime {
		if dv := c.compareTime(s, path, "btime", r.Btime, d.Btime, 100); dv != nil {
			return dv, nil
		}
	}
	// Inodes compare as a relation: paths sharing an inode on one side must
	// share it on the other.
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

// compareTime compares a timestamp relatively, or exactly after the plan
// set it. unit is the resolution both sides are truncated to.
func (c *comparer) compareTime(s Step, path, field string, r, d int64, unit int64) *Divergence {
	key := path + "|" + field
	if c.explicit[key] {
		if r/unit != d/unit {
			return div(s, field, fmt.Sprint(r), fmt.Sprint(d))
		}
		c.prev[0][key], c.prev[1][key] = r, d
		return nil
	}
	rel := func(i int, v int64) string {
		p, ok := c.prev[i][key]
		c.prev[i][key] = v
		switch {
		case !ok:
			return "first"
		case p == v:
			return "unchanged"
		case v > p:
			return "advanced"
		}
		return "went-back"
	}
	rr, dr := rel(0, r), rel(1, d)
	if rr != dr {
		d := div(s, field, rr, dr)
		d.Sig = fmt.Sprintf("%s:%s:%s/%s", s.Op, field, rr, dr)
		return d
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
	if a, b := name(rd.Owner), name(dd.Owner); a != b {
		return div(s, "owner", a, b)
	}
	if a, b := name(rd.Group), name(dd.Group); a != b {
		return div(s, "group", a, b)
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
		return div(s, "dacl", a, b)
	}
	if a, b := acl(rd, false), acl(dd, false); a != b {
		return div(s, "sacl", a, b)
	}
	return nil
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
