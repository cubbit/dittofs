package directory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/cubbit/dittofs/pkg/auth/sid"
)

// unicodePwd quotes the password and encodes it UTF-16LE, a code unit per
// character and a surrogate pair beyond the basic plane.
func TestUnicodePwd(t *testing.T) {
	for _, tc := range []struct {
		password string
		want     string
	}{
		{"ab", "\"\x00a\x00b\x00\"\x00"},
		{"é", "\"\x00\xe9\x00\"\x00"},
		{"𝄞", "\"\x00\x34\xd8\x1e\xdd\"\x00"},
	} {
		if got := unicodePwd([]byte(tc.password)); got != tc.want {
			t.Errorf("unicodePwd(%q) = %x, want %x", tc.password, got, tc.want)
		}
	}
}

// selfRelativeSD is a self-relative security descriptor's 20-byte header
// with its owner offset set to off, followed by body.
func selfRelativeSD(off uint32, body ...byte) []byte {
	sd := make([]byte, 20, 20+len(body))
	sd[0], sd[2], sd[3] = 1, 0x04, 0x80 // revision 1, SE_SELF_RELATIVE | SE_DACL_PRESENT
	binary.LittleEndian.PutUint32(sd[4:], off)
	return append(sd, body...)
}

// domainAdmins is S-1-5-21-1-2-3-512 as a SID's bytes.
var domainAdmins = []byte{1, 5, 0, 0, 0, 0, 0, 5,
	21, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 0, 2, 0, 0}

// sdOwnerCases are sdOwner's rows, the malformed ones seeding FuzzSDOwner.
var sdOwnerCases = []struct {
	name  string
	sd    []byte
	want  string // the owner, formatted; empty when an error is wanted
	error string // a substring of the error
}{
	{"nothing", nil, "", "shorter than a security descriptor's header"},
	{"short of the header", selfRelativeSD(20)[:19], "", "shorter than a security descriptor's header"},
	{"no owner", selfRelativeSD(0, domainAdmins...), "", "owner offset 0"},
	{"owner offset at the end", selfRelativeSD(48, domainAdmins...), "", "owner offset 48"},
	{"owner offset past the end", selfRelativeSD(0xffffffff, domainAdmins...), "", "owner offset 4294967295"},
	{"owner shorter than a SID", selfRelativeSD(20, domainAdmins[:7]...), "", "SID too short"},
	{"owner's sub-authorities past the end", selfRelativeSD(20, domainAdmins[:27]...), "", "too short for 5 sub-authorities"},
	{"owner after the header", selfRelativeSD(20, domainAdmins...), "S-1-5-21-1-2-3-512", ""},
	{"owner after a gap", selfRelativeSD(24, append([]byte{9, 9, 9, 9}, domainAdmins...)...), "S-1-5-21-1-2-3-512", ""},
}

// sdOwner reads the owner a self-relative security descriptor's offset
// points at, and refuses a header, an offset or a SID that does not fit.
func TestSDOwner(t *testing.T) {
	for _, tc := range sdOwnerCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("sdOwner(%x) panicked: %v", tc.sd, r)
				}
			}()
			got, err := sdOwner(tc.sd)
			switch {
			case tc.error != "" && (err == nil || !strings.Contains(err.Error(), tc.error)):
				t.Errorf("sdOwner(%x) = %s, %v, want an error containing %q", tc.sd, sid.FormatSID(&got), err, tc.error)
			case tc.error == "" && err != nil:
				t.Errorf("sdOwner(%x): %v, want %s", tc.sd, err, tc.want)
			case tc.error == "" && sid.FormatSID(&got) != tc.want:
				t.Errorf("sdOwner(%x) = %s, want %s", tc.sd, sid.FormatSID(&got), tc.want)
			}
		})
	}
}

// Whatever bytes it is given, sdOwner does not panic, and an owner it
// answers is the SID encoded at the offset the header names, inside sd.
func FuzzSDOwner(f *testing.F) {
	for _, tc := range sdOwnerCases {
		f.Add(tc.sd)
	}
	f.Fuzz(func(t *testing.T, sd []byte) {
		o, err := sdOwner(sd)
		if err != nil {
			return
		}
		off := uint64(binary.LittleEndian.Uint32(sd[4:]))
		var enc bytes.Buffer
		sid.EncodeSID(&enc, &o)
		if end := off + uint64(enc.Len()); off == 0 || end > uint64(len(sd)) || !bytes.Equal(sd[off:end], enc.Bytes()) {
			t.Fatalf("sdOwner(%x) = %s, which is not the %d bytes at offset %d", sd, sid.FormatSID(&o), enc.Len(), off)
		}
	})
}

// wellKnownDN answers the DN after the GUID it is asked for, among other
// GUIDs, and nothing for a value with no GUID, a GUID of another length or
// no DN.
func TestWellKnownDN(t *testing.T) {
	const users = "B:32:A9D1CA15768811D1ADED00C04FD8D5CD:CN=Users,DC=corp,DC=example"
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{"the Computers GUID among others", []string{users,
			"B:32:aa312825768811d1aded00c04fd8d5cd:OU=Servers,DC=corp,DC=example"}, "OU=Servers,DC=corp,DC=example"},
		{"a DN holding colons", []string{"B:32:" + computersGUID + ":CN=a:b,DC=corp"}, "CN=a:b,DC=corp"},
		{"no Computers GUID", []string{users}, ""},
		{"no GUID", []string{"B:32:CN=Computers,DC=corp,DC=example"}, ""},
		{"a GUID one digit short", []string{"B:31:" + computersGUID[1:] + ":CN=Computers,DC=corp"}, ""},
		{"no DN", []string{"B:32:" + computersGUID + ":"}, ""},
		{"none", nil, ""},
	} {
		got, ok := wellKnownDN(tc.values, computersGUID)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: wellKnownDN(%q) = %q, %v, want %q", tc.name, tc.values, got, ok, tc.want)
		}
	}
}

// hostAliases answers every alias a host= value lists, its spacing trimmed,
// and none from another service's value.
func TestHostAliases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mappings []string
		want     []string
	}{
		{"several aliases", []string{"host=alerter,cifs,http"}, []string{"alerter", "cifs", "http"}},
		{"spacing and case", []string{" HOST = cifs , http ,"}, []string{"cifs", "http"}},
		{"another service's value", []string{"ldap=gc", "host=cifs"}, []string{"cifs"}},
		{"no host=", []string{"ldap=gc", "cifs"}, nil},
		{"none", nil, nil},
	} {
		if got := hostAliases(tc.mappings); !slices.Equal(got, tc.want) {
			t.Errorf("%s: hostAliases(%q) = %q, want %q", tc.name, tc.mappings, got, tc.want)
		}
	}
}

// refused marks a refused credential or right ErrRefused, keeping the LDAP
// error, and leaves any other error alone.
func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		code uint16
		want bool
	}{
		{ldap.LDAPResultInvalidCredentials, true},
		{ldap.LDAPResultInsufficientAccessRights, true},
		{ldap.ErrorEmptyPassword, true},
		{ldap.LDAPResultUnavailable, false},
		{ldap.LDAPResultEntryAlreadyExists, false},
	} {
		err := refused(fmt.Errorf("bind: %w", ldap.NewError(tc.code, errors.New("from the server"))))
		if errors.Is(err, ErrRefused) != tc.want || !ldap.IsErrorWithCode(err, tc.code) {
			t.Errorf("refused(LDAP result %d) = %v, want ErrRefused %v and the result code kept", tc.code, err, tc.want)
		}
	}
}

// A 32-bit attribute is read signed or unsigned, and a value neither reading
// holds is refused rather than wrapped.
func TestAttrUint32(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want uint32
		ok   bool
	}{
		{"0", 0, true},
		{"24", 24, true},
		{"4294967295", 0xffffffff, true},
		{"-1", 0xffffffff, true},
		{"-2147483648", 0x80000000, true},
		{"4294967296", 0, false},
		{"-2147483649", 0, false},
		{"x", 0, false},
	} {
		got, err := attrUint32(tc.v)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("attrUint32(%q) = %d, %v; want %d, ok %v", tc.v, got, err, tc.want, tc.ok)
		}
	}
}

// StrictlyUnder admits a DN below the base, case and spacing aside, and
// refuses the base itself, a sibling, a parent, an empty base and a DN that
// does not parse.
func TestStrictlyUnder(t *testing.T) {
	const base = "DC=corp,DC=example"
	for _, tc := range []struct {
		dn, base string
		want     bool
	}{
		{"OU=Servers,DC=corp,DC=example", base, true},
		{"ou=Servers, dc=CORP,dc=example", base, true},
		{"OU=NAS,OU=Servers,DC=corp,DC=example", base, true},
		{base, base, false},
		{"OU=Servers,DC=other,DC=example", base, false},
		{"DC=example", base, false},
		{"OU=Servers,DC=corp,DC=example", "", false},
		{"not a dn", base, false},
	} {
		if got := StrictlyUnder(tc.dn, tc.base); got != tc.want {
			t.Errorf("StrictlyUnder(%q, %q) = %v, want %v", tc.dn, tc.base, got, tc.want)
		}
	}
}
