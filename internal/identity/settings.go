package identity

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// Settings is the identity section, the installation's identity.* keys.
type Settings struct {
	// IDRange is the span a uid or gid missing from a created principal is
	// allocated from. A change applies to the next allocation and moves no
	// existing ID.
	IDRange entity.IDRange `json:"id_range" class:"live"`
	// NTLM is whether NTLMv2 sign-in is accepted.
	NTLM bool `json:"ntlm" class:"live"`
	// NetBIOSName is the installation's own NetBIOS name, which an SMB
	// CHALLENGE advertises as its computer name.
	NetBIOSName string `json:"netbios_name" class:"live"`
	// Keytab names the keytab, a Secret of kind keytab, never the keytab
	// itself. Its versions reach the server as State.Keytab.
	Keytab entity.SecretRef `json:"keytab" class:"live"`
}

// maxNetBIOSName is a NetBIOS name's length in bytes; the sixteenth byte of
// the wire name is its suffix, never the installation's to choose.
const maxNetBIOSName = 15

// defaultSettings is the section's value where no row sets a key.
func defaultSettings() Settings {
	return Settings{
		IDRange: entity.IDRange{Lo: 1_000_000, Hi: 1_999_999_999},
		// decision: NTLM is on by default. Workgroup deployments with local users
		// have no KDC, and SMB clients outside a domain fall back to NTLM. A
		// deployment with Kerberos everywhere turns it off. Revisit the default
		// once local users are shown to be rare (RFC 18 section 3.5).
		NTLM:        true,
		NetBIOSName: "DITTOFS",
	}
}

// Section declares the identity section for the node's settings table.
func Section() settings.Section {
	return settings.Of("identity", settings.At(entity.ScopeInstallation), defaultSettings(),
		settings.SecretRef("Keytab", entity.SecretKeytab))
}

// DirectorySection declares the identity.directory section, the directory
// record, which only an admin writes, whole, its bind password a Secret named
// by reference. Address has no default, so a snapshot holding a row of the
// section but no address is refused rather than validated as an empty one.
//
// decision: a section with no rows is no directory, State.Directory nil, and
// the reader that fills the State maps it so: the declared default is never
// decoded or validated alone, since an empty address fails Validate. Overturned
// if the settings read ever validates a section with no rows from its default.
func DirectorySection() settings.Section {
	return settings.Of("identity.directory", settings.At(entity.ScopeInstallation),
		directory.Record{Schema: directory.SchemaAD},
		settings.Required("Address"),
		settings.WriteRole(entity.RoleAdmin),
		settings.SecretRef("BindCredential", entity.SecretDirectoryCredential))
}

// Validate refuses a range whose low end is above its high end, and a NetBIOS
// name that is empty, longer than 15 bytes or holds a byte outside
// [A-Z0-9-].
func (s Settings) Validate() error {
	if s.IDRange.Lo > s.IDRange.Hi {
		return fmt.Errorf("identity: id_range low end %d above high end %d", s.IDRange.Lo, s.IDRange.Hi)
	}
	if s.NetBIOSName == "" {
		return errors.New("identity: netbios_name is empty")
	}
	if len(s.NetBIOSName) > maxNetBIOSName {
		return fmt.Errorf("identity: netbios_name %q is longer than %d bytes", s.NetBIOSName, maxNetBIOSName)
	}
	for i := range len(s.NetBIOSName) {
		c := s.NetBIOSName[i]
		upper, digit := 'A' <= c && c <= 'Z', '0' <= c && c <= '9'
		if !upper && !digit && c != '-' {
			return fmt.Errorf("identity: netbios_name %q holds %q, outside [A-Z0-9-]", s.NetBIOSName, c)
		}
	}
	return nil
}
