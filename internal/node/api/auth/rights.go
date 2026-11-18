package auth

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// Right is what a route asks of its caller, one per row of the role table.
type Right uint8

const (
	Public       Right = iota // anyone, authenticated or not
	View                      // read every resource but accounts, tokens and the audit; health detail, operations, node status, open connections
	Shares                    // shares, grants, export policies, quotas, snapshots, policies, backups, restores
	Stores                    // remote stores, namespaces, adapters, settings, netgroups, and the secrets they name
	Principals                // users and groups that hold no role
	Accounts                  // accounts with a role, roles, others' service tokens
	Drain                     // drain a node
	Installation              // raise a format version, decommission a node, state a location lost or a journal range unreadable, re-home, move, rotate the signing key
	Audit                     // read the audit, and the access, admin and logon event streams
	QuotaEvents               // read the quota event stream
	Self                      // own password, own tokens, sign out
)

// roles is the role table: for each right, the least role that may read
// what it guards and the least that may hold it.
var roles = [...]struct{ read, hold entity.APIRole }{
	Public:       {entity.RoleNone, entity.RoleNone},
	View:         {entity.RoleViewer, entity.RoleViewer},
	Shares:       {entity.RoleViewer, entity.RoleOperator},
	Stores:       {entity.RoleViewer, entity.RoleOperator},
	Principals:   {entity.RoleViewer, entity.RoleOperator},
	Accounts:     {entity.RoleAdmin, entity.RoleAdmin},
	Drain:        {entity.RoleViewer, entity.RoleOperator},
	Installation: {entity.RoleViewer, entity.RoleAdmin},
	Audit:        {entity.RoleAdmin, entity.RoleAdmin},
	QuotaEvents:  {entity.RoleOperator, entity.RoleOperator},
	Self:         {entity.RoleViewer, entity.RoleViewer},
}

// Allowed reports whether c may read what r guards, and whether it may hold
// r. The API answers a caller that may not read as though the resource did
// not exist, so an account's existence is not revealed.
func Allowed(c Caller, r Right) (read, hold bool) {
	return c.Role >= roles[r].read, c.Role >= roles[r].hold
}
