//go:build linux

package fuzz

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// This file holds the finite domains every plan parameter is drawn from
// (DESIGN.md §6). A change to any table here changes what round N means, so it
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
// DESIGN.md §8.1. Names are SDDL aliases where one exists.
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

// ExpandPath turns a plan path into the on-disk relative path. "L255" stands
// for a 255-byte name.
func ExpandPath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if c == "L255" {
			parts[i] = strings.Repeat("l", 255)
		}
	}
	return strings.Join(parts, "/")
}

// Point is a named offset or length (DESIGN.md §6.4).
type Point struct {
	Name  string
	Value int64
}

const (
	kib = int64(1) << 10
	mib = int64(1) << 20
)

// genericPoints are small values and powers of two. Neighbours are not
// points of their own: a sum with the 1 or -1 point reaches them
// (genState.size).
var genericPoints = []Point{
	{"0", 0}, {"1", 1}, {"2", 2},
	{"4096", 4096}, {"2^13", 8 * kib}, {"2^14", 16 * kib}, {"2^15", 32 * kib},
	{"2^20", mib}, {"2^21", 2 * mib},
}

// MinusOne is the one negative point. It is only ever the second term of a
// sum, so an expression never evaluates below zero.
var MinusOne = Point{"-1", -1}

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

// AllPoints lists every non-negative point in a fixed order.
var AllPoints = append(append([]Point(nil), genericPoints...), dittoBoundaries...)

var pointByName = func() map[string]int64 {
	m := map[string]int64{}
	for _, p := range AllPoints {
		m[p.Name] = p.Value
	}
	return m
}()

// SizeTerms splits an offset or length expression into its signed terms:
// "chunk.max-1" is ["chunk.max", "-1"], "2^20+2^13" is ["2^20", "+2^13"].
// Point names hold no '+' or '-', so either sign starts a term.
func SizeTerms(expr string) []string {
	var out []string
	start := 0
	for i := 1; i < len(expr); i++ {
		if expr[i] == '+' || expr[i] == '-' {
			out = append(out, expr[start:i])
			start = i
		}
	}
	return append(out, expr[start:])
}

// JoinSizeTerms is SizeTerms' inverse; a leading '+' is dropped.
func JoinSizeTerms(terms []string) string {
	return strings.TrimPrefix(strings.Join(terms, ""), "+")
}

// EvalSize evaluates an offset or length expression: point names or decimal
// numbers, each preceded by '+' or '-' but the first.
func EvalSize(expr string) (int64, error) {
	var total int64
	for _, term := range SizeTerms(expr) {
		sign := int64(1)
		name := term
		switch {
		case strings.HasPrefix(term, "+"):
			name = term[1:]
		case strings.HasPrefix(term, "-"):
			sign, name = -1, term[1:]
		}
		v, ok := pointByName[name]
		if !ok {
			n, err := strconv.ParseInt(name, 10, 64)
			if err != nil || name == "" {
				return 0, fmt.Errorf("bad size term %q in %q", term, expr)
			}
			v = n
		}
		total += sign * v
	}
	return total, nil
}

// Modes (DESIGN.md §6.5).
var Modes = []string{"0000", "0400", "0444", "0600", "0640", "0644", "0700", "0750", "0755", "0777"}

// DataPatterns (DESIGN.md §6.6).
var DataPatterns = []string{"zero", "ones", "seq", "rand"}

// Times for utimensat and setbtime (DESIGN.md §6.7): seconds and nanoseconds.
var TimeSeconds = []string{"0", "1", "1000000000", "2147483647", "2147483648", "4294967296", "-11644473600"}

// TimeNanos are the nanosecond parts.
var TimeNanos = []string{"0", "1", "100", "999999999"}

// XattrNames (DESIGN.md §6.8); "user.L200" stands for a 200-byte suffix.
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

// DOS attributes (DESIGN.md §6.9).
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
