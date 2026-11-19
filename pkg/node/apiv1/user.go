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
