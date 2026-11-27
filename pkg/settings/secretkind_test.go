package settings_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// kindFields is a per-thing protocol section's fields as these tests declare
// them: its credential a secret's reference, which no real section of that
// form declares yet.
type kindFields struct {
	valid
	Credential entity.SecretRef `json:"credential" class:"live"`
}

// kindTable is a table with a secret's reference at each of the two key forms
// a section of one thing takes: the directory's, an installation section's,
// and a protocol's, a per-thing one.
func kindTable() settings.Table {
	return settings.Table{
		dirTable()[0],
		settings.Of("protocols", settings.PerThing(entity.ScopeInstallation, "protocol"),
			kindFields{}, settings.SecretRef("Credential", entity.SecretRemoteCredential)),
	}
}

// The table names a secret's reference by the key as the write and the read
// spell it: a reference key answers its declared kind, a per-thing one
// through the key as its section declares it, and every other key answers
// none: a plain field's, a reference's with the thing missing, an extra
// part, and one no section takes.
func TestTableSecretKind(t *testing.T) {
	for _, tc := range []struct {
		key   string
		kind  entity.SecretKind
		named bool
	}{
		{"identity.directory.bind_credential", entity.SecretDirectoryCredential, true},
		{"protocols.nfs.credential", entity.SecretRemoteCredential, true},
		{"identity.directory.bind_name", 0, false},
		{"protocols.credential", 0, false},
		{"protocols.nfs.credential.extra", 0, false},
		{"identity.directory.bind_credential.extra", 0, false},
		{"no.such.key", 0, false},
	} {
		kind, named := kindTable().SecretKind(tc.key)

		if named != tc.named || kind != tc.kind {
			t.Errorf(
				"SecretKind(%q) = (%v, %v), want (%v, %v)",
				tc.key,
				kind,
				named,
				tc.kind,
				tc.named,
			)
		}
	}
}
