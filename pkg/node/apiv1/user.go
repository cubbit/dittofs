package apiv1

// User is one user as a resource.
type User = Resource[UserSpec, struct{}]

// UserSpec is what an operator declares of a user: its name; its uid, absent
// for none, which a create leaving it out allocates in the installation's ID
// range, and never 0, root's; its e-mail address, optional, never defaulted and naming nothing to
// a file check; whether it is disabled, which refuses it on every
// protocol and on the API; its Kerberos name, absent for none, which a
// write links for the directory user SID the directory holds under it; and
// its role in the management API, absent for none, which only an admin
// gives or changes. A user created with a role and no uid or Kerberos name
// is an API-only account, given no uid and no SID.
type UserSpec struct {
	Name     string  `json:"name"`
	UID      *uint32 `json:"uid,omitempty"`
	Email    string  `json:"email,omitempty"`
	Disabled bool    `json:"disabled"`
	Kerberos string  `json:"kerberos,omitempty"`
	Role     Role    `json:"role,omitempty"`
}

// Role is an account's role in the management API, each holding the rights
// of the one before it.
type Role string

// The roles, in increasing order.
const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)
