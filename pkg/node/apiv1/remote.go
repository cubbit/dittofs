package apiv1

import "encoding/json"

// Store is one remote store as a resource: its settings at its own scope,
// and its credential's reference.
type Store = Resource[StoreSpec, struct{}]

// StoreSpec is what an operator declares of a remote store: its settings,
// by key, as JSON, and its credential, which only the credential's own call
// sets and every read answers as its reference and version.
type StoreSpec struct {
	Settings   map[string]json.RawMessage `json:"settings"`
	Credential *SecretRef                 `json:"credential,omitempty"`
}
