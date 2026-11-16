package api

import (
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// right is what a route asks of its caller, one per row of the role table.
type right uint8

const (
	public       right = iota // anyone, authenticated or not
	view                      // read every resource but accounts, tokens and the audit; health detail, operations, node status, open connections
	shares                    // shares, grants, export policies, quotas, snapshots, policies, backups, restores
	stores                    // remote stores, namespaces, adapters, settings, netgroups, and the secrets they name
	principals                // users and groups that hold no role
	accounts                  // accounts with a role, roles, others' service tokens
	drain                     // drain a node
	installation              // raise a format version, decommission a node, state a location lost or a journal range unreadable, re-home, move, rotate the signing key
	audit                     // read the audit, and the access, admin and logon event streams
	quotaEvents               // read the quota event stream
	self                      // own password, own tokens, sign out
)

// roles is the role table: for each right, the least role that may read
// what it guards and the least that may hold it.
var roles = [...]struct{ read, hold entity.APIRole }{
	public:       {entity.RoleNone, entity.RoleNone},
	view:         {entity.RoleViewer, entity.RoleViewer},
	shares:       {entity.RoleViewer, entity.RoleOperator},
	stores:       {entity.RoleViewer, entity.RoleOperator},
	principals:   {entity.RoleViewer, entity.RoleOperator},
	accounts:     {entity.RoleAdmin, entity.RoleAdmin},
	drain:        {entity.RoleViewer, entity.RoleOperator},
	installation: {entity.RoleViewer, entity.RoleAdmin},
	audit:        {entity.RoleAdmin, entity.RoleAdmin},
	quotaEvents:  {entity.RoleOperator, entity.RoleOperator},
	self:         {entity.RoleViewer, entity.RoleViewer},
}

// allow refuses c a row needing r: a caller that may not read what r guards
// as if it did not exist, so an account's existence is not revealed, and one
// that may read it but not hold r 403.
func allow(c auth.Caller, r right) error {
	switch {
	case c.Role < roles[r].read:
		return errNotFound
	case c.Role < roles[r].hold:
		return &refusal{code: apiv1.CodeForbidden, detail: "the caller's role does not permit this call"}
	}
	return nil
}
