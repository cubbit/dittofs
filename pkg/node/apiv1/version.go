package apiv1

// Version is the body of GET /api/version: the major versions the server
// serves, and nothing else.
type Version struct {
	Majors []string `json:"majors"`
}
