package apiv1

import (
	"encoding/json"
	"time"
)

// AuditRecord is one management action: when it committed, who acted, by
// name and credential, on which node and from where, the request, its
// target with the target's generation before and after, the spec fields it
// changed, and its outcome: applied, refused with its problem code, or the
// operation it started.
type AuditRecord struct {
	ID         string          `json:"id"`
	Time       time.Time       `json:"time"`
	Actor      Principal       `json:"actor"`
	ActorName  string          `json:"actor_name"`
	Credential string          `json:"credential"`
	Node       string          `json:"node"`
	Source     string          `json:"source"`
	Request    json.RawMessage `json:"request"`
	Target     Ref             `json:"target"`
	Before     uint64          `json:"before"`
	After      uint64          `json:"after"`
	Change     json.RawMessage `json:"change,omitempty"`
	Outcome    string          `json:"outcome"`
}
