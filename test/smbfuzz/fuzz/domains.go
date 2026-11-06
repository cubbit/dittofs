package fuzz

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// This file holds the finite domains every plan parameter is drawn from
// (RFC 31 §6). A change to any table here changes what round N means, so it
// is made only together with a new generator version (gen.go).

// Users are the plan's actors, in a fixed order.
var Users = []WeightedChoice[string]{
	{"alice", 35}, {"bob", 30}, {"carol", 20}, {"admin", 15},
}

// UserNames lists the users in their fixed order.
func UserNames() []string {
	out := make([]string, len(Users))
	for i, u := range Users {
		out[i] = u.V
	}
	return out
}

// Trustees an ACE, an owner or a group may name, with the weights of
// RFC 31 §8.1. Names are SDDL aliases where one exists.
var Trustees = []WeightedChoice[string]{
	{"alice", 10}, {"bob", 10}, {"carol", 10}, {"Administrator", 10},
	{"devs", 6}, {"engineering", 6}, {"fuzzers", 6},
	{"WD", 8}, {"AU", 4}, {"CO", 5}, {"CG", 3}, {"OW", 4},
	{"DU", 3}, {"DA", 3}, {"BA", 2}, {"BU", 2}, {"SY", 1},
	{"ghost", 2}, {"unix-1000", 1},
}

// Owners are the trustees an owner or group is drawn from: every trustee
// but the ones that are meaningless as an owner on purpose stay in, since
// how each server refuses them is part of the test.
var Owners = Trustees

// Paths of a round's tree (RFC 31 §6.3). "L255" stands for a 255-byte name.
var Paths = []WeightedChoice[string]{
	{"f0", 12.5}, {"f1", 12.5}, {"d0", 12.5}, {"d1", 12.5},
	{"d0/f0", 6}, {"d0/f1", 6}, {"d0/d1", 6}, {"d0/d1/f0", 6}, {"d1/f0", 6},
	{"F0", 5}, {"D0/f0", 5},
	{"a b", 2}, {"é", 2}, {"É", 1.5}, {".hidden", 1.5},
	{"L255", 3},
}

// ExpandPath turns a plan path into the on-disk relative path.
func ExpandPath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if c == "L255" {
			parts[i] = strings.Repeat("l", 255)
		}
	}
	return strings.Join(parts, "/")
}

// Point is a named offset or length (RFC 31 §6.4).
type Point struct {
	Name  string
	Value int64
}

const (
	kib = int64(1) << 10
	mib = int64(1) << 20
)

// genericPoints are powers of two and their neighbours.
var genericPoints = []Point{
	{"0", 0}, {"1", 1}, {"2", 2},
	{"4095", 4095}, {"4096", 4096}, {"4097", 4097},
	{"2^13", 8 * kib}, {"2^14", 16 * kib}, {"2^15", 32 * kib},
	{"2^20", mib}, {"2^21", 2 * mib},
}

// dittoBoundaries are DittoFS's own size boundaries at generator version 1:
// the largest SMB I/O it advertises (one journal record per write piece),
// the chunker's bounds, the journal's carve block, the block store's block,
// and the chunk bounds RFC 2 specifies.
var dittoBoundaries = []Point{
	{"smb.io", mib},
	{"chunk.min", mib}, {"chunk.avg", 4 * mib}, {"chunk.max", 16 * mib},
	{"carve.block", 4 * mib}, {"block", 8 * mib},
	{"rfc2.min", 64 * kib}, {"rfc2.target", 256 * kib}, {"rfc2.max", mib},
}

// AllPoints lists every point, the boundaries with their -1 and +1
// neighbours, in a fixed order.
var AllPoints = func() []Point {
	out := append([]Point(nil), genericPoints...)
	for _, b := range dittoBoundaries {
		out = append(out, Point{b.Name + "-1", b.Value - 1}, b, Point{b.Name + "+1", b.Value + 1})
	}
	return out
}()

var pointByName = func() map[string]int64 {
	m := map[string]int64{}
	for _, p := range AllPoints {
		m[p.Name] = p.Value
	}
	return m
}()

// EvalSize evaluates an offset or length expression: terms joined by '+',
// each a point name, a point name followed by "-1", or a decimal number.
// "chunk.max+1" therefore evaluates as chunk.max plus 1, which is the point
// of the same name.
func EvalSize(expr string) (int64, error) {
	var total int64
	for _, term := range strings.Split(expr, "+") {
		if v, ok := pointByName[term]; ok {
			total += v
			continue
		}
		if base, ok := strings.CutSuffix(term, "-1"); ok {
			if v, ok := pointByName[base]; ok {
				total += v - 1
				continue
			}
		}
		v, err := strconv.ParseInt(term, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad size term %q in %q", term, expr)
		}
		total += v
	}
	return total, nil
}

// Modes (RFC 31 §6.5).
var Modes = []string{"0000", "0400", "0444", "0600", "0640", "0644", "0700", "0750", "0755", "0777"}

// DataPatterns (RFC 31 §6.6).
var DataPatterns = []string{"zero", "ones", "seq", "rand"}

// Times for utimensat and setbtime (RFC 31 §6.7): seconds and nanoseconds.
var TimeSeconds = []string{"0", "1", "1000000000", "2147483647", "2147483648", "4294967296", "-11644473600"}

// TimeNanos are the nanosecond parts.
var TimeNanos = []string{"0", "1", "100", "999999999"}

// XattrNames (RFC 31 §6.8); "user.L200" stands for a 200-byte suffix.
var XattrNames = []string{"user.a", "user.b", "user.A", "user.L200"}

// ExpandXattrName returns the on-wire name.
func ExpandXattrName(n string) string {
	if n == "user.L200" {
		return "user." + strings.Repeat("x", 200)
	}
	return n
}

// XattrLens are value lengths; a value is "<pattern>:<len>".
var XattrLens = []string{"0", "1", "255", "4096", "65000"}

// Flag tables map plan names to values.

type flagName struct {
	name string
	v    uint64
}

var openFlagNames = []flagName{
	{"O_RDONLY", unix.O_RDONLY}, {"O_WRONLY", unix.O_WRONLY}, {"O_RDWR", unix.O_RDWR},
	{"O_CREAT", unix.O_CREAT}, {"O_EXCL", unix.O_EXCL}, {"O_TRUNC", unix.O_TRUNC},
	{"O_APPEND", unix.O_APPEND}, {"O_DIRECTORY", unix.O_DIRECTORY}, {"O_NOFOLLOW", unix.O_NOFOLLOW},
	{"O_SYNC", unix.O_SYNC}, {"O_DSYNC", unix.O_DSYNC},
}

var fallocNames = []flagName{
	{"0", 0}, {"KEEP_SIZE", unix.FALLOC_FL_KEEP_SIZE}, {"PUNCH_HOLE", unix.FALLOC_FL_PUNCH_HOLE},
	{"ZERO_RANGE", unix.FALLOC_FL_ZERO_RANGE}, {"COLLAPSE_RANGE", unix.FALLOC_FL_COLLAPSE_RANGE},
	{"INSERT_RANGE", unix.FALLOC_FL_INSERT_RANGE},
}

var whenceNames = []flagName{{"SEEK_END", unix.SEEK_END}, {"SEEK_DATA", unix.SEEK_DATA}, {"SEEK_HOLE", unix.SEEK_HOLE}}

var renameNames = []flagName{{"0", 0}, {"RENAME_NOREPLACE", unix.RENAME_NOREPLACE}}

var xattrFlagNames = []flagName{{"0", 0}, {"XATTR_CREATE", unix.XATTR_CREATE}, {"XATTR_REPLACE", unix.XATTR_REPLACE}}

var flockNames = []flagName{
	{"LOCK_SH", unix.LOCK_SH}, {"LOCK_EX", unix.LOCK_EX}, {"LOCK_UN", unix.LOCK_UN}, {"LOCK_NB", unix.LOCK_NB},
}

// DOS attributes (RFC 31 §6.9).
var dosAttrNames = []flagName{
	{"READONLY", 0x1}, {"HIDDEN", 0x2}, {"SYSTEM", 0x4}, {"DIRECTORY", 0x10}, {"ARCHIVE", 0x20},
	{"NORMAL", 0x80}, {"TEMPORARY", 0x100}, {"SPARSE_FILE", 0x200}, {"COMPRESSED", 0x800},
	{"OFFLINE", 0x1000}, {"NOT_CONTENT_INDEXED", 0x2000}, {"ENCRYPTED", 0x4000},
	{"INTEGRITY_STREAM", 0x8000}, {"NO_SCRUB_DATA", 0x20000},
}

// settableDOS are the attributes drawn with the higher probability.
var settableDOS = map[string]bool{
	"READONLY": true, "HIDDEN": true, "SYSTEM": true, "ARCHIVE": true, "TEMPORARY": true,
	"OFFLINE": true, "NOT_CONTENT_INDEXED": true,
}

// parseFlags parses "A|B|0x10" against a table. "0" is zero.
func parseFlags(s string, table []flagName) (uint64, error) {
	var v uint64
	for _, part := range strings.Split(s, "|") {
		if part == "" {
			continue
		}
		found := false
		for _, f := range table {
			if f.name == part {
				v |= f.v
				found = true
				break
			}
		}
		if found {
			continue
		}
		n, err := strconv.ParseUint(part, 0, 64)
		if err != nil {
			return 0, fmt.Errorf("unknown flag %q", part)
		}
		v |= n
	}
	return v, nil
}

// formatFlags writes the names of the set bits, in table order; bits with no
// name are appended in hex.
func formatFlags(v uint64, table []flagName) string {
	var parts []string
	rest := v
	for _, f := range table {
		if f.v != 0 && v&f.v == f.v && rest&f.v != 0 {
			parts = append(parts, f.name)
			rest &^= f.v
		}
	}
	if rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", rest))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}
