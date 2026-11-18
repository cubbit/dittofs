package identity_test

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// A section with no rows yields the defaults: NTLM on, NetBIOSName DITTOFS
// and uids and gids from 1 000 000 to 1 999 999 999. The defaults are read
// from the value the section is declared with, since the settings read that
// applies them to a section with no rows is not built yet.
func TestDefaultSettings(t *testing.T) {
	got := identity.DefaultSettings()
	want := identity.Settings{
		IDRange:     entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999},
		NTLM:        true,
		NetBIOSName: "DITTOFS",
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
	if got := s.Applied(); got == nil || *got != want {
		t.Fatalf("State after a refused Apply = %+v, want the last applied %+v", got, want)
	}
}
