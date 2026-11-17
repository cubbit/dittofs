package apiv1

// Share is one share as a resource.
type Share = Resource[ShareSpec, ShareStatus]

// ShareSpec is what an operator declares of a share: the name an SMB client
// tree-connects to, its path in the NFS namespace, the ID of the namespace
// its content is counted in, which only a re-home changes, and its case
// sensitivity, sensitive or insensitive, bound when the share is created and
// sensitive when the create leaves it empty.
type ShareSpec struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Namespace       string `json:"namespace"`
	CaseSensitivity string `json:"case_sensitivity"`
}

// ShareStatus is what the system records of a share: its state, enabled,
// quiesced, removing or retired; on how many nodes it is applied; and how
// many of the nodes reporting a status refused it. A write's answer, made
// before any node can have applied the share, carries neither count.
type ShareStatus struct {
	State   string   `json:"state"`
	Applied *Applied `json:"applied,omitempty"`
	Refused int      `json:"refused,omitempty"`
}

// NewShare is the body that creates a share: its spec and its export policy.
type NewShare struct {
	Spec         ShareSpec        `json:"spec"`
	ExportPolicy ExportPolicySpec `json:"export_policy"`
}
