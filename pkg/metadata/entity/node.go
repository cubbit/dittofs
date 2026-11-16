package entity

// Node is one server process: what it registers at each start, and the
// epoch its starts raise.
type Node struct {
	ID      NodeID
	Address string
	Roles   Roles
	Binary  string // the binary version it runs
	Domain  string // its failure domain
	Epoch   uint64 // raised at every start on a single node
	// Formats is the range of versions it reads and writes of each format
	// it shares with other nodes.
	Formats map[Format]VersionRange
	Lapsed  bool // marked by a takeover: its lease at Epoch fences nothing
}

// Roles is what a node serves; both roles together is the one accepted
// value. Its value is stored.
type Roles uint8

const (
	RoleProtocol Roles = 1 << iota
	RoleStorage
)

// Format names one versioned format nodes share. Its value is stored.
type Format uint8

// VersionRange is the versions of a format a node reads and writes,
// inclusive.
type VersionRange struct{ Min, Max uint32 }

// NodeStatus is what one node reports, written only by that node: the
// counter values it has reconciled through, and the exceptions to them.
type NodeStatus struct {
	Node           NodeID
	Settings       uint64 // the settings generation reconciled through
	ShareList      uint64 // the share list version reconciled through
	Principals     uint64 // the principals generation reconciled through
	Claims         uint64 // the claim generation reconciled through
	Refused        []RefusedRecord
	RestartPending []ResourceRef // records whose restart-class field changed since it started
	Secrets        []SecretVersion
	Conditions     []byte // its health conditions
}

// RefusedRecord is a record a node did not apply: the generation it
// refused and why.
type RefusedRecord struct {
	Record     ResourceRef
	Generation uint64
	Error      []byte // the problem body
}

// SecretVersion is a secret a node resolves and the version it holds.
type SecretVersion struct {
	Secret  SecretRef
	Version uint64
}
