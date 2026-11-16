package apiv1

// Resource is a resource's body, with the same four parts in every resource:
// its ID, the generation its spec was last written at, the spec an operator
// declares and the status the system records.
type Resource[Spec, Status any] struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	Spec       Spec   `json:"spec"`
	Status     Status `json:"status"`
}
