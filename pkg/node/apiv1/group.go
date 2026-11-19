package apiv1

// Group is one group as a resource.
type Group = Resource[GroupSpec, struct{}]

// GroupSpec is what an operator declares of a group: its name, and its
// gid, absent for none, which a create leaving it out allocates in the
// installation's ID range, and never 0, root's.
type GroupSpec struct {
	Name string  `json:"name"`
	GID  *uint32 `json:"gid,omitempty"`
}

// Membership is one member of a group as a resource, its ID the member's:
// it exists or it does not, and declares nothing.
type Membership = Resource[struct{}, struct{}]
