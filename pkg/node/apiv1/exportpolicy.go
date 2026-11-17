package apiv1

// ExportPolicy is a share's export policy as a resource: its ID is the
// share's, its generation the policy's version.
type ExportPolicy = Resource[ExportPolicySpec, struct{}]

// ExportPolicySpec is a share's admission and identity-mapping rule: the
// flavours SECINFO answers, in preference order, which admit nobody; the
// proof every caller must give, none, claimed, secret or ticket; the
// protection its messages need, none, integrity or privacy; whether every
// caller is held to read-only; the client rules, the first match deciding
// and none matching refusing, an empty list admitting every client; whose
// identity is squashed, none, root or all; and the share's SMB flags.
type ExportPolicySpec struct {
	Flavors       []string     `json:"flavors"`
	MinProof      string       `json:"min_proof"`
	MinProtection string       `json:"min_protection"`
	ReadOnly      bool         `json:"read_only"`
	Clients       []ClientRule `json:"clients"`
	Squash        string       `json:"squash"`
	SMB           SMBShare     `json:"smb"`
}

// ClientRule admits clients by a CIDR or by a netgroup, exactly one of them,
// with an access of none, read or read-write, never wider than the grant.
type ClientRule struct {
	CIDR     string `json:"cidr,omitempty"`
	Netgroup string `json:"netgroup,omitempty"`
	Access   string `json:"access"`
}

// SMBShare is a share's SMB flags: whether every message is sealed, whether
// share enumeration leaves it out, and whether its handles are continuously
// available.
type SMBShare struct {
	Encrypt               bool `json:"encrypt"`
	Hidden                bool `json:"hidden"`
	ContinuouslyAvailable bool `json:"continuously_available"`
}
