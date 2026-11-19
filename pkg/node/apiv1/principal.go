package apiv1

// User is one user as a resource.
type User = Resource[UserSpec, struct{}]

// UserSpec is what an operator declares of a user: its name; its uid, zero
// for none, which a create leaving it out allocates in the installation's ID
// range; its e-mail address, optional, never defaulted and naming nothing to
// a file check; and whether it is disabled, which refuses it on every
// protocol and on the API.
type UserSpec struct {
	Name     string `json:"name"`
	UID      uint32 `json:"uid,omitempty"`
	Email    string `json:"email,omitempty"`
	Disabled bool   `json:"disabled"`
}

// Group is one group as a resource.
type Group = Resource[GroupSpec, struct{}]

// GroupSpec is what an operator declares of a group: its name, and its
// gid, zero for none, which a create leaving it out allocates in the
// installation's ID range.
type GroupSpec struct {
	Name string `json:"name"`
	GID  uint32 `json:"gid,omitempty"`
}

// Membership is one member of a group as a resource, its ID the member's:
// it exists or it does not, and declares nothing.
type Membership = Resource[struct{}, struct{}]

// Grant is one principal's access to one share as a resource, its ID the
// principal's and its generation the grant's version.
type Grant = Resource[GrantSpec, struct{}]

// GrantSpec is a grant's access: none, read, read-write or admin. A grant
// of none is stored and refuses the principal, unlike no grant, which lets
// its groups' grants decide.
type GrantSpec struct {
	Access string `json:"access"`
}

// Netgroup is one netgroup as a resource, its ID its name and its
// generation its version.
type Netgroup = Resource[NetgroupSpec, struct{}]

// NetgroupSpec is a netgroup's members: host names, addresses or CIDRs,
// replaced whole by every write.
type NetgroupSpec struct {
	Members []string `json:"members"`
}

// Quota is one share's quota on one user or group as a resource, its ID the
// principal's and its generation the quota's version.
type Quota = Resource[QuotaSpec, struct{}]

// QuotaSpec is a quota's limits: refused beyond Hard; allowed beyond Soft
// for GraceSeconds, then refused; never refused beyond Advisory, which only
// raises an event. A quota whose limits are all zero limits nothing and is
// deleted.
type QuotaSpec struct {
	Hard         Limit `json:"hard"`
	Soft         Limit `json:"soft"`
	GraceSeconds int64 `json:"grace_seconds"`
	Advisory     Limit `json:"advisory"`
}

// Limit is a byte and a file count, zero in either limiting nothing on it.
type Limit struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}
