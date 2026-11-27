package settings

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The settings structs the per-thing tests declare sections over, the
// per-thing ones named by a key RFC 13 writes.
type (
	journalSpare struct {
		valid
		SpareTarget uint32 `json:"spare_target" class:"live"`
	}
	adapterPending struct {
		valid
		PendingTimeout uint32 `json:"pending_timeout" class:"live"`
	}
	flatSettings struct {
		valid
		MaxFiles uint32 `json:"max_files" class:"live"`
	}
)

// thingID is the journal ID the per-thing tests write, in hex as a key holds
// it.
const thingID = "3f2a00000000000000000000000000a1"

// A per-thing key names its thing once, after the section's prefix: a
// journal's key takes the journal's ID after its first part, and an
// adapter's key already carries the adapter, so its protocol is the thing
// and the rest is not split again. A key with no thing, or a thing with no
// field, declares nothing, and a section that is not per thing keeps its key
// as written.
func TestResolveThing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	node := entity.ScopeNode
	idx := Table{
		Of("journal", PerThing(node, "journal"), journalSpare{}),
		Of("adapters", PerThing(node, "adapter"), adapterPending{}),
		Of("test.flat", At(entity.ScopeInstallation), flatSettings{}),
		Of("identity", installation, identityMode{}),
		Of("identity.oidc", installation, oidcIssuer{}),
	}.index()

	cases := []struct {
		key      string
		section  string // "" for no section
		declared string
		thing    string
	}{
		{"journal." + thingID + ".spare_target", "journal", "journal.spare_target", thingID},
		{"adapters.smb.pending_timeout", "adapters", "adapters.pending_timeout", "smb"},
		{"adapters.smb.tls.mode", "adapters", "adapters.tls.mode", "smb"},
		{"journal.spare_target", "journal", "", ""},
		{"journal." + thingID, "journal", "", ""},
		{"journal..spare_target", "journal", "", ""},
		{"journal." + thingID + ".", "journal", "", ""},
		{"journal", "journal", "", ""},
		{"test.flat.max_files", "test.flat", "test.flat.max_files", ""},
		{"identity.oidc.issuer", "identity.oidc", "identity.oidc.issuer", ""},
		{"identity.mode", "identity", "identity.mode", ""},
		{"other.key", "", "", ""},
	}

	for _, c := range cases {
		sec, declared, thing, found := idx.resolve(c.key)

		section := ""
		if found {
			section = sec.prefix
		}

		if section != c.section || declared != c.declared || thing != c.thing {
			t.Errorf("resolve(%q) = section %q, key %q, thing %q; want %q, %q, %q",
				c.key, section, declared, thing, c.section, c.declared, c.thing)
		}
	}
}

// thingKey writes the form splitThing reads back.
func TestThingKeyRoundTrip(t *testing.T) {
	prof.Watch(t, prof.Options{})

	key := thingKey("journal", "journal.spare_target", thingID)
	if want := "journal." + thingID + ".spare_target"; key != want {
		t.Fatalf("thingKey = %q, want %q", key, want)
	}

	thing, declared, ok := splitThing("journal", key)
	if !ok || thing != thingID || declared != "journal.spare_target" {
		t.Errorf("splitThing(%q) = %q, %q, %v; want %q, journal.spare_target, true",
			key, thing, declared, ok, thingID)
	}

	// An adapter's key already carries the adapter: its round trip gives
	// the key back as written, the protocol named once.
	const adapter = "adapters.smb.pending_timeout"

	thing, declared, ok = splitThing("adapters", adapter)
	if !ok {
		t.Fatalf("splitThing(%q) does not split", adapter)
	}

	if key := thingKey("adapters", declared, thing); key != adapter {
		t.Errorf("thingKey(splitThing(%q)) = %q, want it back", adapter, key)
	}
}
