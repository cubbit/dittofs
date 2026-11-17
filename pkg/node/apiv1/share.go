package apiv1

// Share is one share as a resource.
type Share = Resource[ShareSpec, ShareStatus]

// ShareSpec is what an operator declares of a share: the name an SMB client
// tree-connects to, its path in the NFS namespace, and the ID of the
// namespace its content is counted in, which only a re-home changes.
type ShareSpec struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Namespace string `json:"namespace"`
}

// ShareStatus is what the system records of a share: its state, enabled,
// quiesced, removing or retired; and on how many nodes it is applied, absent
// from a write's answer, which is made before any node can have applied it.
type ShareStatus struct {
	State   string   `json:"state"`
	Applied *Applied `json:"applied,omitempty"`
}

// NewShare is the body that creates a share: its spec and its export policy.
type NewShare struct {
	Spec         ShareSpec        `json:"spec"`
	ExportPolicy ExportPolicySpec `json:"export_policy"`
}
