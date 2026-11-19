package entity

import "strconv"

// Node is one server process: what it registers at each start, and the
// epoch its starts raise.
type Node struct {
	ID      NodeID
	Address string
	Roles   NodeRoles
	Binary  string // the binary version it runs
	Domain  string // its failure domain
	Epoch   uint64 // raised at every start on a single node
	// Formats is the range of versions it reads and writes of each format
	// it shares with other nodes.
	Formats map[Format]VersionRange
	Lapsed  bool // marked by a takeover: its lease at Epoch fences nothing
}

// NodeRoles is what a node serves; both roles together is the one accepted
// value. Its value is stored.
type NodeRoles uint8

const (
	NodeRoleProtocol NodeRoles = 1 << iota
	NodeRoleStorage
)

// Format names one versioned format nodes share. Its value is stored.
type Format uint8

// The formats nodes share, each with an active version the installation
// writes and a range each node registers (RFC 13 §5.5). Zero is no format.
const (
	FormatStore    Format = iota + 1 // the metadata store format, in \x00format (RFC 16 §4.6)
	FormatJournal                    // the journal format (RFC 28 §2)
	FormatBlock                      // the block format (RFC 4 §3.5)
	FormatSettings                   // the settings record schema (RFC 13 §2.3)
)

// String names the format for errors and series labels: its name, or its
// number for a format this binary does not know.
func (f Format) String() string {
	switch f {
	case FormatStore:
		return "store"
	case FormatJournal:
		return "journal"
	case FormatBlock:
		return "block"
	case FormatSettings:
		return "settings"
	default:
		return strconv.Itoa(int(f))
	}
}

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
	Adapters       []AdapterStatus
}

// AdapterStatus is one of a node's adapters as the node reports it: its
// protocol, its state, and the settings generation it runs.
type AdapterStatus struct {
	Protocol   string
	State      AdapterState
	Generation uint64
}

// AdapterState is one of an adapter's states. Its value is stored, so a new
// state is appended and none is renumbered; the zero state names none.
type AdapterState uint8

const (
	AdapterConfigured AdapterState = iota + 1
	AdapterServing
	AdapterDraining
	AdapterStopped
)

var adapterStates = []string{
	AdapterConfigured: "configured", AdapterServing: "serving",
	AdapterDraining: "draining", AdapterStopped: "stopped",
}

func (s AdapterState) String() string                { return enumString(adapterStates, s) }
func (s AdapterState) MarshalText() ([]byte, error)  { return enumMarshal(adapterStates, s) }
func (s *AdapterState) UnmarshalText(b []byte) error { return enumUnmarshal(adapterStates, s, b) }

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
