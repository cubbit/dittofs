package caps

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Each adapter's profile, as String() names it, matches the reviewed golden
// in testdata/profiles.golden, so a change to a profile shows in review as a
// change to that file.
func TestEachProfileIsPinned(t *testing.T) {
	want, err := os.ReadFile("testdata/profiles.golden")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range []struct {
		name string
		c    Caps
	}{
		{"NFSv3Caps", NFSv3Caps}, {"NFSv4Caps", NFSv4Caps}, {"MountCaps", MountCaps}, {"NLMCaps", NLMCaps}, {"SMBCaps", SMBCaps},
	} {
		fmt.Fprintf(&b, "%-9s %s\n", p.name, p.c)
	}
	if got := b.String(); got != string(want) {
		t.Errorf("the profiles are\n%s\nwant testdata/profiles.golden\n%s", got, want)
	}
}

// String names every set bit, nothing for none, and an unnamed bit in hex;
// Has holds only when every bit asked is set.
func TestStringAndHas(t *testing.T) {
	for _, tc := range []struct {
		c    Caps
		want string
	}{
		{0, ""},
		{PrivilegeOnEveryCall, "privilege"},
		{OpenFilesKeepNames | HostCall, "keep-names|host"},
		{WantOpenID | GrantBelongsToOpen | ClientIsMachine, "open-id|oplock|machine"},
		{HostCall << 1, "0x40"},
	} {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Caps(%#x).String() = %q, want %q", uint16(tc.c), got, tc.want)
		}
	}
	if !SMBCaps.Has(OpenFilesKeepNames|WantOpenID) || SMBCaps.Has(OpenFilesKeepNames|HostCall) || !Caps(0).Has(0) {
		t.Error("Has: SMBCaps holds keep-names and open-id together, not keep-names with host; anything holds the empty set")
	}
}
