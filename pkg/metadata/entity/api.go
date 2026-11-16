package entity

import "time"

// ResourceRef names one management resource: its kind, such as "share" or
// "user", and its ID as the API spells it in bytes.
type ResourceRef struct {
	Kind string
	ID   []byte
}

// AuditRecord is one management action, written in the transaction of the
// change it records and never rewritten.
type AuditRecord struct {
	ID [16]byte // minted by the write that records it
	// Time is the store time of the commit, set by the write.
	Time time.Time
	// Actor is the account that acted; the zero principal is the schedule,
	// for work no call started.
	Actor         Principal
	Node          NodeID // the node that took the request
	Source        string // the client's address
	Request       []byte // method, path, request ID and idempotency key
	Target        ResourceRef
	Before, After uint64 // the target's generation
	Change        []byte // the fields changed, old and new; a secret as "set", never its value
	Outcome       []byte // applied, refused with its problem code, or the operation's ID
}

// IdempotencyRecord is the answer first given to one account's request under
// one idempotency key, kept so a retry gets that answer and changes nothing.
type IdempotencyRecord struct {
	Account Principal
	Key     string
	Digest  [32]byte  // of the request's method, path and body
	Answer  []byte    // status and body as first answered
	Expires time.Time // set by the write: 24 h after the request's store time
}
