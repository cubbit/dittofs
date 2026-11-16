package apiv1

// Role is an API account's role: what it may read and change through the
// API. An account with no role holds none of these.
type Role string

// The roles, each holding every right of the one before it.
const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// roles is the closed set of roles, each once.
var roles = []Role{RoleViewer, RoleOperator, RoleAdmin}
