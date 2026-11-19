package api

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// An address is optional and taken only bare and valid, and never on a
// directory principal, one holding a directory entry's ID, which no write
// through the store can yet make, so the rule is tested here.
func TestEmailFor(t *testing.T) {
	local := entity.User{IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "5000"}}}
	dir := entity.User{IDs: entity.ProtocolIDs{{Scheme: entity.SchemeDir, Value: "entry-1"}}}
	for _, tc := range []struct {
		name  string
		u     entity.User
		email string
		ok    bool
	}{
		{"none on a local user", local, "", true},
		{"a bare address", local, "alice@example.com", true},
		{"not an address", local, "alice", false},
		{"a display name", local, "Alice <alice@example.com>", false},
		{"none on a directory principal", dir, "", true},
		{"an address on a directory principal", dir, "alice@example.com", false},
	} {
		if err := emailFor(tc.u, tc.email); (err == nil) != tc.ok {
			t.Errorf("%s: emailFor(%q) = %v, want accepted %v", tc.name, tc.email, err, tc.ok)
		}
	}
}
