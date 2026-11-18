package apiv1

// Health is a node's readiness word, ready, starting, draining or
// not_ready, and every health condition it holds, its own and the
// installation's.
type Health struct {
	Ready      string      `json:"ready"`
	Conditions []Condition `json:"conditions"`
}

// Condition is one health condition: its name, what it is about, by scope
// and the subject's ID and name, its cause and its detail.
type Condition struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	ID      string `json:"id,omitempty"`
	Subject string `json:"subject,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Detail  string `json:"detail,omitempty"`
}
