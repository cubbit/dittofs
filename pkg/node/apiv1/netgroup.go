package apiv1

// Netgroup is one netgroup as a resource, its ID its name and its
// generation its version.
type Netgroup = Resource[NetgroupSpec, struct{}]

// NetgroupSpec is a netgroup's members: host names, addresses or CIDRs,
// replaced whole by every write.
type NetgroupSpec struct {
	Members []string `json:"members"`
}
