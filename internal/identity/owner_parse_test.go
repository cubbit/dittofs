package identity

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
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
// its length: it allocates nothing itself, and this is the fuzzer's noise.
const ownerAllocBase = 4 << 10

// FuzzOwnerString holds the owner-string parser to its invariants: it never
// panics; it allocates within a constant, so within any multiple of its
// input's length; and what it parses spells back to the same string, since
// only canonical forms parse.
func FuzzOwnerString(f *testing.F) {
	for _, s := range []string{
		"alice@corp.example", "ALICE@CORP.EXAMPLE", "alice@other.example", "a@b@corp.example",
		"1500@corp.example", "0", "1500", "65534", "4294967295", "4294967296", "01500", "+1500",
		"", "@", "@corp.example", "alice@", "alice", "nobody@localdomain", "alice@corp.exampl\u212a",
		strings.Repeat("a", 64<<10) + "@corp.example", strings.Repeat("1", 64<<10),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		name, domain, num, form := parseOwner(s)
		runtime.ReadMemStats(&after)
		if n := after.TotalAlloc - before.TotalAlloc; n > ownerAllocBase {
			t.Fatalf("parsing %d bytes allocated %d, want at most %d", len(s), n, ownerAllocBase)
		}
		switch form {
		case ownerName:
			if name == "" || domain == "" {
				t.Fatalf("parseOwner(%q) read name %q in domain %q, want both non-empty", s, name, domain)
			}
			if again := name + "@" + domain; again != s {
				t.Fatalf("parseOwner(%q) read %q in %q, which spells %q", s, name, domain, again)
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
