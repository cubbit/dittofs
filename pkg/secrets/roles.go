package secrets

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// Role is a wrapping key's role: each secret kind is sealed under the key of
// the role that uses it, and every node names both keys in its bootstrap.
//
// decision: the two keys stay separate although every node holds both, so a
// later split of the roles (RFC 15 §2.1) re-seals nothing and a disk-less
// protocol node is handed the protocol key alone (RFC 13 §7). Overturned if
// that split is ruled out; the keys then merge.
type Role uint8

const (
	Protocol Role = iota + 1 // authentication's secrets
	Storage                  // the remote tier's, the material provider's and the management API's
)

var roleNames = []string{
	Protocol: "protocol",
	Storage:  "storage",
}

// String names the role as the bootstrap's keys.<role> does.
func (r Role) String() string {
	if int(r) < len(roleNames) {
		return roleNames[r]
	}

	return ""
}

// kindRoles gives each secret kind the role whose key seals it, from RFC 13
// §7's table, so a kind's role is fixed data and never a caller's choice.
var kindRoles = map[entity.SecretKind]Role{
	entity.SecretNTHash:              Protocol,
	entity.SecretKeytab:              Protocol,
	entity.SecretMachinePassword:     Protocol,
	entity.SecretDirectoryCredential: Protocol,
	entity.SecretPassword:            Storage,
	entity.SecretRemoteCredential:    Storage,
	entity.SecretKey:                 Storage,
}

// roleOf is the role that seals kind; false for a kind with none.
func roleOf(kind entity.SecretKind) (Role, bool) {
	r, ok := kindRoles[kind]

	return r, ok
}
