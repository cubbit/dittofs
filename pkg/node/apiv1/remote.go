package apiv1

import "encoding/json"

// Store is one remote store as a resource: its settings at its own scope,
// and its credential's reference.
type Store = Resource[StoreSpec, struct{}]

// StoreCredential is a remote store's credential: its value, which only a
// create takes and no answer ever carries, and its secret's reference and
// current version, which every read answers and the value never joins.
type StoreCredential struct {
	Ref     string `json:"ref,omitempty"`
	Version uint64 `json:"version,omitempty"`
	Value   []byte `json:"value,omitempty"`
}

// StoreSpec is what an operator declares of a remote store: its settings,
// by key, as JSON, and its credential, which a create states by value and
// every later call sets by its own route; each answer holds the credential
// as its reference and version, never its value.
type StoreSpec struct {
	Settings   map[string]json.RawMessage `json:"settings"`
	Credential *StoreCredential           `json:"credential,omitempty"`
}
