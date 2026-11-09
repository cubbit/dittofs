// Package adapter is the adapters' public surface: the contract the node
// drives, the sidecars it runs beside them, and what an adapter is built from.
// Both the node and the adapters' shared core import it, and it imports
// neither, so neither imports the other.
package adapter

import (
	"context"
	"iter"
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// Adapter is what the node moves through the adapter's states: configured,
// serving, draining, stopped. One server type in the adapters' core implements
// it for every protocol.
//
// decision: an interface with one implementation, against the rule that an
// interface needs two, because only the two protocol constructors may import
// the adapters' implementation under internal/adapter: the node, which drives
// every adapter, names this contract rather than the server type. Withdraw it
// if the node is ever allowed to import that implementation.
type Adapter interface {
	// Start moves configured to serving: every listener bound, or none, and
	// the error names what failed. Start after Stop is refused: a stopped
	// adapter is never started again, and the node builds a new one with New.
	Start(ctx context.Context) error

	// Reconfigure adds and removes listeners and applies live settings to
	// connections set up after it returns. s is the adapter's own protocol's
	// shape, config.NFS or config.SMB; the other protocol's is refused with an
	// error. A restart-only setting is refused with an error wrapping
	// config.ErrRestartRequired; the node applies it by Stop, New and Start.
	Reconfigure(ctx context.Context, s config.Settings) error

	// Stop moves serving to draining to stopped. ctx's deadline is the drain
	// deadline; requests still running at it are abandoned. A request refused
	// while draining is told its share moved when the service names another
	// gateway node for it, and is left unanswered otherwise; the service is
	// asked per request. Stop is idempotent: a second call returns once the
	// adapter is stopped.
	Stop(ctx context.Context) error

	// Health is ready when serving with every listener bound; configured and
	// draining report not ready.
	Health() Health

	// Connections lists the connections open now, for the API's client list.
	// It is read from the core's connection registry, never written by
	// protocol code.
	Connections() iter.Seq[ConnInfo]
}

// State is one of the adapter's states.
type State uint8

const (
	StateConfigured State = iota
	StateServing
	StateDraining
	StateStopped
)

// Health is what the node reads for readiness and its conditions.
type Health struct {
	State      State
	Ready      bool     // serving with every listener bound
	Conditions []string // the conditions this adapter raises now, by name: adapter_listener_lost
}

// ConnInfo is one open connection.
type ConnInfo struct {
	Protocol string         // "nfs", "smb"
	Remote   netip.AddrPort // the client's address
	Session  string         // the protocol's session the connection carries, as text; empty where none
	Since    time.Time      // accepted at
}

// Sidecar runs beside adapters, reaches no service and holds no client state:
// it tells clients where an adapter is, or that something happened.
//
// decision: a sidecar is the one auxiliary kind that is a type; a service
// that answers beside an adapter's protocol and shares its state (MOUNT, NLM,
// NSM) is no type, only more operation tables on its adapter's server. Folded
// into one kind, every rule would become a flag whose two values behave
// oppositely: who builds it, whether its failure stops the adapter, whether
// it may reach the service. Revisit if a third kind appears that fits neither.
type Sidecar interface {
	Name() string       // "mdns", "wsd", "portmap", "rpcbind": for logs and health
	Adapters() []string // the adapters it serves: {"nfs"}, {"smb"}, {"nfs", "smb"}
	// Sync is called with those of Adapters now serving, whenever that set or
	// their listeners change, and with none to stop. It returns once the
	// sidecar matches, and is idempotent.
	Sync(ctx context.Context, serving []Serving) error
}

// Serving is one adapter as a sidecar sees it: what to advertise or answer.
type Serving struct {
	Adapter   string                    // "nfs"
	Listeners map[string]netip.AddrPort // by service: "nfs", "mount", "nlm", "nsm"
}
