package identity

import (
	"errors"
	"fmt"
	"time"

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
	// DirectoryStaleGrace is how long past its 5-minute TTL a directory
	// answer may be served while the directory fails; 0 serves none.
	DirectoryStaleGrace time.Duration `json:"directory_stale_grace" class:"live"`
}

// maxNetBIOSName is a NetBIOS name's length in bytes; the sixteenth byte of
// the wire name is its suffix, never the installation's to choose.
const maxNetBIOSName = 15

// defaultSettings is the section's value where no row sets a key.
func defaultSettings() Settings {
	return Settings{
		IDRange: entity.DefaultIDRange,
		// decision: NTLM is on by default. Workgroup deployments with local users
		// have no KDC, and SMB clients outside a domain fall back to NTLM. A
		// deployment with Kerberos everywhere turns it off. Revisit the default
		// once local users are shown to be rare (RFC 18 section 3.5).
		NTLM:                true,
		NetBIOSName:         "DITTOFS",
		DirectoryStaleGrace: 15 * time.Minute,
	}
}

// Section declares the identity section for the node's settings table.
func Section() settings.Section {
	return settings.Of("identity", settings.At(entity.ScopeInstallation), defaultSettings(),
		settings.SecretRef("Keytab", entity.SecretKeytab),
		settings.CheckTx(checkSettings))
}

// checkSettings is the identity section's write check, run inside the
// write's transaction on the section the write leaves. A write of id_range,
// a put or a delete back to the default, is refused when the range's high end
// is at or below the uid or the gid counter's high-water mark: the counters
// never fall, so the next uid or gid, past the mark, would lie past the high
// end, and every later create drawing one would fail. A write of any other
// key passes unread, so a range already used up never refuses one.
//
// decision: the marks are checked at the write only, never again at Apply,
// since a second answer read later could refuse a range the write committed
// and leave the API's range and the server's apart. The check is advisory
// against a create that read the previous range before the write committed:
// that create can still raise a counter past the new high end, and the next
// create then fails as from an exhausted range until the range is widened.
// Making it exact would take every create carrying the settings generation it
// read its range at, conflicting with every setting change; worth it only if
// such a race is ever seen outside a test.
func checkSettings(tx entity.ControlPlaneTx, w settings.Write, s Settings) error {
	if w.Key != "identity.id_range" {
		return nil
	}
	uid, gid, err := tx.IDHighWater()
	if err != nil {
		return fmt.Errorf("identity: read the ID high-water marks: %w", err)
	}
	for _, m := range []struct {
		kind string
		mark uint32
	}{{"uid", uid}, {"gid", gid}} {
		if s.IDRange.Hi <= m.mark {
			return w.Refuse(fmt.Sprintf("high end %d is at or below the last %s allocated, %d, "+
				"so no further %s could be drawn from it; set a high end above %d",
				s.IDRange.Hi, m.kind, m.mark, m.kind, m.mark))
		}
	}
	return nil
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

// Validate refuses a range whose low end is above its high end, a negative
// stale grace, and a NetBIOS name that is empty, longer than 15 bytes or
// holds a byte outside [A-Z0-9-], each error naming the field, since the
// settings check prints it after the section's own name.
func (s Settings) Validate() error {
	if s.IDRange.Lo > s.IDRange.Hi {
		return fmt.Errorf("id_range: low end %d above high end %d", s.IDRange.Lo, s.IDRange.Hi)
	}
	if s.DirectoryStaleGrace < 0 {
		return fmt.Errorf("directory_stale_grace: %v is negative", s.DirectoryStaleGrace)
	}
	if s.NetBIOSName == "" {
		return errors.New("netbios_name: empty")
	}
	if len(s.NetBIOSName) > maxNetBIOSName {
		return fmt.Errorf("netbios_name: %q is longer than %d bytes", s.NetBIOSName, maxNetBIOSName)
	}
	for i := range len(s.NetBIOSName) {
		c := s.NetBIOSName[i]
		upper, digit := 'A' <= c && c <= 'Z', '0' <= c && c <= '9'
		if !upper && !digit && c != '-' {
			return fmt.Errorf("netbios_name: %q holds %q, outside [A-Z0-9-]", s.NetBIOSName, c)
		}
	}
	return nil
}
