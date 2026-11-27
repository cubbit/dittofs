package identity_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// A section with no rows yields the defaults: NTLM on, NetBIOSName DITTOFS,
// uids and gids from 1 000 000 to 1 999 999 999 and a directory stale grace
// of 15 minutes. The defaults are read
// from the value the section is declared with, since the settings read that
// applies them to a section with no rows is not built yet.
func TestDefaultSettings(t *testing.T) {
	got := identity.DefaultSettings()
	want := identity.Settings{
		IDRange:             entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999},
		NTLM:                true,
		NetBIOSName:         "DITTOFS",
		DirectoryStaleGrace: 15 * time.Minute,
	}
	if got != want {
		t.Fatalf("default settings = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate of the defaults = %v, want nil", err)
	}
}

func TestSettingsValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*identity.Settings)
		refuse bool
	}{
		{"DITTOFS", func(s *identity.Settings) { s.NetBIOSName = "DITTOFS" }, false},
		{"NAS-01", func(s *identity.Settings) { s.NetBIOSName = "NAS-01" }, false},
		{"15 bytes", func(s *identity.Settings) { s.NetBIOSName = strings.Repeat("A", 15) }, false},
		{"16 bytes", func(s *identity.Settings) { s.NetBIOSName = strings.Repeat("A", 16) }, true},
		{"lower case", func(s *identity.Settings) { s.NetBIOSName = "dittofs" }, true},
		{"underscore", func(s *identity.Settings) { s.NetBIOSName = "NAS_01" }, true},
		{"empty", func(s *identity.Settings) { s.NetBIOSName = "" }, true},
		{"range of one ID", func(s *identity.Settings) { s.IDRange = entity.IDRange{Lo: 5, Hi: 5} }, false},
		{"range Lo above Hi", func(s *identity.Settings) { s.IDRange = entity.IDRange{Lo: 6, Hi: 5} }, true},
		{"no stale grace", func(s *identity.Settings) { s.DirectoryStaleGrace = 0 }, false},
		{"negative stale grace", func(s *identity.Settings) { s.DirectoryStaleGrace = -time.Second }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := identity.DefaultSettings()
			tc.edit(&s)
			if err := s.Validate(); (err != nil) != tc.refuse {
				t.Errorf("Validate(%+v) = %v, want refused %v", s, err, tc.refuse)
			}
		})
	}
}

// Apply of a State whose settings fail Validate is refused, and the last
// applied State stays current.
func TestApplyRefusesInvalidSettings(t *testing.T) {
	s := newServer(t)
	good := state(1)
	if err := s.Apply(good); err != nil {
		t.Fatalf("Apply of a valid State: %v", err)
	}
	want := *s.Applied()
	bad := state(2)
	bad.Settings.NetBIOSName = "dittofs.lab"
	if err := s.Apply(bad); err == nil || !strings.Contains(err.Error(), "netbios_name") {
		t.Fatalf("Apply with NetBIOSName dittofs.lab = %v, want Validate's netbios_name refusal", err)
	}
	if got := s.Applied(); got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("State after a refused Apply = %+v, want the last applied %+v", got, want)
	}
}

// Apply refuses a State whose NetBIOS name is the directory's NetBIOS domain
// with ASCII letters folded, keeping the last applied, and accepts one that
// matches only under a Unicode fold, the Kelvin sign folding to k.
func TestApplyNetBIOSNameAgainstDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, netbios, domain string
		refuse                bool
	}{
		{"same case", "CORP", "CORP", true},
		{"domain lower case", "CORP", "corp", true},
		{"domain mixed case", "CORP", "cOrP", true},
		{"Kelvin sign", "KDC", "KDC", false},
		{"other domain", "NAS", "CORP", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			if err := s.Apply(state(1)); err != nil {
				t.Fatalf("%s: Apply of a valid State: %v", tc.name, err)
			}
			want := *s.Applied()
			st := state(2)
			st.Settings.NetBIOSName = tc.netbios
			st.Directory = &directory.Record{Address: "ldaps://dc1.corp.example", Realm: "CORP.EXAMPLE", NetBIOSDomain: tc.domain}
			err := s.Apply(st)
			if (err != nil) != tc.refuse {
				t.Fatalf("%s: Apply with netbios_name %q, netbios_domain %q = %v, want refused %t", tc.name, tc.netbios, tc.domain, err, tc.refuse)
			}
			if got := s.Applied(); tc.refuse && !reflect.DeepEqual(*got, want) {
				t.Errorf("%s: State after a refused Apply = %+v, want the last applied %+v", tc.name, got, want)
			}
			if got := s.Applied(); !tc.refuse && *got.Directory != *st.Directory {
				t.Errorf("%s: applied Directory = %+v, want %+v", tc.name, got.Directory, st.Directory)
			}
		})
	}
}

// Apply refuses a State whose directory record fails Validate.
func TestApplyRefusesInvalidDirectory(t *testing.T) {
	s := newServer(t)
	st := state(1)
	st.Directory = &directory.Record{Address: "http://dc1.corp.example"}
	if err := s.Apply(st); err == nil {
		t.Fatal("Apply with directory address http:// = nil, want refused")
	}
	if got := s.Applied(); got != nil {
		t.Fatalf("State after a refused first Apply = %+v, want none", got)
	}
}

// A State whose directory record equals the current one's, through another
// pointer, keeps the serial; a changed record raises it.
func TestApplyComparesDirectoryByValue(t *testing.T) {
	s := newServer(t)
	rec := directory.Record{Address: "ldaps://dc1.corp.example", Realm: "CORP.EXAMPLE", NetBIOSDomain: "CORP"}
	st := state(1)
	a := rec
	st.Directory = &a
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	first := s.Current()
	b := rec
	st.Directory = &b
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	if got := s.Current(); got != first {
		t.Fatalf("serial after an equal directory record = %d, want %d", got, first)
	}
	c := rec
	c.Base = "DC=corp,DC=example"
	st.Directory = &c
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	if got := s.Current(); got == first {
		t.Fatalf("serial after a changed directory record = %d, want above %d", got, first)
	}
}

// Apply keeps its own copy of the directory record: editing the caller's
// record afterwards changes nothing applied, and applying the edited record
// raises the serial if valid and is refused, the last applied kept, if not.
func TestApplyCopiesDirectory(t *testing.T) {
	s := newServer(t)
	rec := &directory.Record{Address: "ldaps://dc1.corp.example", Realm: "CORP.EXAMPLE", NetBIOSDomain: "CORP"}
	st := state(1)
	st.Directory = rec
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	first := s.Current()
	rec.Address = "ldaps://dc2.corp.example"
	if got := s.Applied().Directory.Address; got != "ldaps://dc1.corp.example" {
		t.Fatalf("applied address after the caller edited its record = %q, want ldaps://dc1.corp.example", got)
	}
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	if got := s.Current(); got == first {
		t.Fatalf("serial after applying the edited record = %d, want above %d", got, first)
	}
	rec.Address = "http://evil.example"
	if err := s.Apply(st); err == nil {
		t.Fatal("Apply with directory address http:// = nil, want refused")
	}
	if got := s.Applied().Directory.Address; got != "ldaps://dc2.corp.example" {
		t.Fatalf("applied address after a refused Apply = %q, want the last applied ldaps://dc2.corp.example", got)
	}
}
