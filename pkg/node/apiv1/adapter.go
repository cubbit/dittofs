package apiv1

import "encoding/json"

// Adapter is one protocol's adapter as a resource: its ID is the protocol.
type Adapter = Resource[AdapterSpec, AdapterStatus]

// AdapterSpec is what an operator declares of an adapter: whether it is
// enabled, and its settings, by field below adapters.<protocol>, as JSON.
type AdapterSpec struct {
	Enabled  bool                       `json:"enabled"`
	Settings map[string]json.RawMessage `json:"settings"`
}

// AdapterStatus is the adapter on each node that reports it.
type AdapterStatus struct {
	Nodes []AdapterOnNode `json:"nodes"`
}

// AdapterOnNode is the adapter on one node: its state and the settings
// generation it runs.
type AdapterOnNode struct {
	Node       string       `json:"node"`
	State      AdapterState `json:"state"`
	Generation uint64       `json:"generation"`
}

// AdapterState is one of an adapter's states, a closed set.
type AdapterState string

const (
	AdapterConfigured AdapterState = "configured"
	AdapterServing    AdapterState = "serving"
	AdapterDraining   AdapterState = "draining"
	AdapterStopped    AdapterState = "stopped"
)

// adapterStates is the closed set of adapter states, each once.
var adapterStates = []AdapterState{AdapterConfigured, AdapterServing, AdapterDraining, AdapterStopped}
