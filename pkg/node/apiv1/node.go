package apiv1

// Node is one node as it registered at its last start: its ID, the address
// it serves on, the binary version it runs, its failure domain, and whether
// a takeover marked it lapsed.
type Node struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Binary  string `json:"binary"`
	Domain  string `json:"domain"`
	Lapsed  bool   `json:"lapsed"`
}
