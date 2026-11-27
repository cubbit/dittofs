package apiv1

// Grant is one principal's access to one share as a resource, its ID the
// principal's and its generation the grant's version.
type Grant = Resource[GrantSpec, struct{}]

// GrantSpec is a grant's access: none, read, read-write or admin. A grant
// of none is stored and refuses the principal, unlike no grant, which lets
// its groups' grants decide.
type GrantSpec struct {
	Access string `json:"access"`
}
