package apiv1

import "encoding/json"

// Namespace is one namespace as a resource.
type Namespace = Resource[NamespaceSpec, struct{}]

// NamespaceSpec is what an operator declares of a namespace: the ID of the
// remote store it is created on, and its settings, by key, as JSON, the
// bound ones fixed at its creation.
type NamespaceSpec struct {
	Store    string                     `json:"store"`
	Settings map[string]json.RawMessage `json:"settings"`
}
