package apiv1

import "fmt"

// MediaTypeProblem is the content type of every error body.
const MediaTypeProblem = "application/problem+json"

// Problem is an error body. Code is the refusing component's error value by
// its name, or one of the API's own codes, and absent from a 500 nothing
// names; Title and Detail are text for a person and are never parsed; Field
// and Rule name a refused value and the rule it broke; Names lists what the
// refusal names; Retryable says whether the same request may succeed later
// unchanged.
type Problem struct {
	Status    int      `json:"status"`
	Code      Code     `json:"code,omitempty"`
	Title     string   `json:"title"`
	Detail    string   `json:"detail,omitempty"`
	Field     string   `json:"field,omitempty"`
	Rule      string   `json:"rule,omitempty"`
	Names     []string `json:"names,omitempty"`
	Retryable bool     `json:"retryable"`
	RequestID string   `json:"request_id"`
}

// Error makes a Problem an error, so a client returns the body as it read it.
func (p *Problem) Error() string {
	if p.Detail == "" {
		return fmt.Sprintf("%d %s: %s", p.Status, p.Code, p.Title)
	}
	return fmt.Sprintf("%d %s: %s: %s", p.Status, p.Code, p.Title, p.Detail)
}
