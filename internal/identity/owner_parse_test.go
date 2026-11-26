package identity

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// parseOwner reads a name only beside a non-empty domain after the last '@',
// and a number only in canonical decimal with no '@' at all.
func TestParseOwner(t *testing.T) {
	for _, c := range []struct {
		in           string
		name, domain string
		num          uint32
		form         ownerForm
	}{
		{"alice@corp.example", "alice", "corp.example", 0, ownerName},
		{"ALICE@CORP.EXAMPLE", "ALICE", "CORP.EXAMPLE", 0, ownerName},
		{"a@b@corp.example", "a@b", "corp.example", 0, ownerName},
		{"1500@corp.example", "1500", "corp.example", 0, ownerName},
		{"0", "", "", 0, ownerNumber},
		{"1500", "", "", 1500, ownerNumber},
		{"4294967295", "", "", 4294967295, ownerNumber},
		{"", "", "", 0, ownerNeither},
		{"@", "", "", 0, ownerNeither},
		{"@corp.example", "", "", 0, ownerNeither},
		{"alice@", "", "", 0, ownerNeither},
		{"alice", "", "", 0, ownerNeither},
		{"01500", "", "", 0, ownerNeither},
		{"00", "", "", 0, ownerNeither},
		{"+1500", "", "", 0, ownerNeither},
		{"-1", "", "", 0, ownerNeither},
		{" 1500", "", "", 0, ownerNeither},
		{"1500 ", "", "", 0, ownerNeither},
		{"4294967296", "", "", 0, ownerNeither},
		{"12345678901", "", "", 0, ownerNeither},
		{"0x10", "", "", 0, ownerNeither},
		{"1_000", "", "", 0, ownerNeither},
	} {
		name, domain, num, form := parseOwner(c.in)
		if name != c.name || domain != c.domain || num != c.num || form != c.form {
			t.Errorf("parseOwner(%q) = %q, %q, %d, %d; want %q, %q, %d, %d", c.in, name, domain, num, form, c.name, c.domain, c.num, c.form)
		}
	}
}

// ownerAllocBase bounds the bytes one owner-string parse allocates, whatever
// its length: a refused SID allocates nothing, and a parsed one under a
// kilobyte.
const ownerAllocBase = 4 << 10

// ownerAllocRuns is how many times the fuzzer parses each input to measure
// it. The heap counters are the whole process's, so the fuzzing engine's own
// allocations land in the window; averaged over this many parses they stay
// far below the bound.
const ownerAllocRuns = 64

// FuzzOwnerString holds the owner-string parser, and the SID parser a who's
// name goes through, to their invariants: they never panic; they allocate
// within a constant, so within any multiple of the input's length; and what
// they parse spells back to the same string, since only canonical forms
// parse.
func FuzzOwnerString(f *testing.F) {
	for _, s := range []string{
		"alice@corp.example", "ALICE@CORP.EXAMPLE", "alice@other.example", "a@b@corp.example",
		"1500@corp.example", "0", "1500", "65534", "4294967295", "4294967296", "01500", "+1500",
		"", "@", "@corp.example", "alice@", "alice", "nobody@localdomain", "alice@corp.exampl\u212a",
		strings.Repeat("a", 64<<10) + "@corp.example", strings.Repeat("1", 64<<10),
		"S-1-5-21-1-2-3-1104@corp.example", "S-1-0-0@corp.example", "S-1-1-0@corp.example",
		"S-1-3-4@CORP.EXAMPLE", "S-1-5@corp.example", "S-1-281474976710655-4294967295@corp.example",
		"S-1-281474976710656-1@corp.example", "S-1-5-4294967296@corp.example", "S-1-5-01@corp.example",
		"S-01-5-1@corp.example", "S-2-5-1@corp.example", "s-1-5-1@corp.example", "S-1-5-+1@corp.example",
		"S-1-5-@corp.example", "S-1-5-x@corp.example", "S-1-5-21-1-2-3-1104",
		"S-1-5" + strings.Repeat("-1", 15) + "@corp.example", "S-1-5" + strings.Repeat("-1", 16) + "@corp.example",
		"S-1-5" + strings.Repeat("-1", 300) + "@corp.example",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range ownerAllocRuns - 1 {
			n, _, _, _ := parseOwner(s)
			parseSID(n)
		}
		name, domain, num, form := parseOwner(s)
		d, isSID := parseSID(name)
		runtime.ReadMemStats(&after)
		if n := (after.TotalAlloc - before.TotalAlloc) / ownerAllocRuns; n > ownerAllocBase {
			t.Fatalf("parsing %d bytes allocated %d per parse, want at most %d", len(s), n, ownerAllocBase)
		}
		switch form {
		case ownerName:
			if name == "" || domain == "" {
				t.Fatalf("parseOwner(%q) read name %q in domain %q, want both non-empty", s, name, domain)
			}
			if again := name + "@" + domain; again != s {
				t.Fatalf("parseOwner(%q) read %q in %q, which spells %q", s, name, domain, again)
			}
			if isSID {
				if len(d.SubAuthorities) > 15 {
					t.Fatalf("parseSID(%q) read %d sub-authorities, want at most 15", name, len(d.SubAuthorities))
				}
				if again := sid.FormatSID(d); again != name {
					t.Fatalf("parseSID(%q) read a SID that spells %q", name, again)
				}
			}
		case ownerNumber:
			if again := strconv.FormatUint(uint64(num), 10); again != s {
				t.Fatalf("parseOwner(%q) read %d, which spells %q", s, num, again)
			}
		case ownerNeither:
		default:
			t.Fatalf("parseOwner(%q) answered form %d", s, form)
		}
	})
}

// parseSID reads only a SID's canonical string form: revision 1, at most
// fifteen sub-authorities, each number in canonical decimal and in range.
func TestParseSID(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"S-1-5-21-1-2-3-1104", true},
		{"S-1-0-0", true},
		{"S-1-5", true},
		{"S-1-281474976710655" + strings.Repeat("-4294967295", 15), true},
		{"S-1-5" + strings.Repeat("-1", 16), false},
		{"S-1-281474976710656-1", false},
		{"S-1-5-4294967296", false},
		{"S-1-5-01", false},
		{"S-1-05-1", false},
		{"S-01-5-1", false},
		{"S-2-5-1", false},
		{"s-1-5-1", false},
		{"S-1-5-+1", false},
		{"S-1-5-", false},
		{"S-1-5-x", false},
		{"S-1", false},
		{"alice", false},
		{"", false},
	} {
		d, ok := parseSID(c.in)
		if ok != c.ok {
			t.Errorf("parseSID(%q) = %v, %v; want ok %v", c.in, d, ok, c.ok)
		}
	}
}
