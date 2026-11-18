package apiv1

// Page is one page of a listing. Next is the opaque cursor that resumes the
// listing after this page, and is absent when no item follows.
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next,omitempty"`
}
