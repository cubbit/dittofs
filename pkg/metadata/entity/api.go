package entity

import "time"

// ResourceRef names one management resource: its kind and its ID as the API
// spells it in bytes.
type ResourceRef struct {
	Kind ResourceKind
	ID   []byte
}

// ResourceKind is a management resource's kind. Its value is stored, so a
// new kind is appended and none is renumbered; the zero kind names none.
type ResourceKind uint8

const (
	ResourceInstallation ResourceKind = iota + 1
	ResourceNode
	ResourceRemoteStore
	ResourceNamespace
	ResourceShare
	ResourceGrant
	ResourceQuota
	ResourceUser
	ResourceGroup
	ResourceMembership
	ResourceNetgroup
	ResourceSetting
	ResourceSecret
	ResourceAdapter
	ResourceSnapshot
	ResourceSnapshotPolicy
	ResourceBackupLocation
	ResourceBackup
	ResourceMove
	ResourceRehome
	ResourceOperation
	ResourceAPIToken
	ResourceSession
)

var resourceKinds = []string{ResourceInstallation: "installation", ResourceNode: "node",
	ResourceRemoteStore: "remote-store", ResourceNamespace: "namespace", ResourceShare: "share",
	ResourceGrant: "grant", ResourceQuota: "quota", ResourceUser: "user", ResourceGroup: "group",
	ResourceMembership: "membership", ResourceNetgroup: "netgroup", ResourceSetting: "setting",
	ResourceSecret: "secret", ResourceAdapter: "adapter", ResourceSnapshot: "snapshot",
	ResourceSnapshotPolicy: "snapshot-policy", ResourceBackupLocation: "backup-location",
	ResourceBackup: "backup", ResourceMove: "move", ResourceRehome: "rehome",
	ResourceOperation: "operation", ResourceAPIToken: "api-token", ResourceSession: "session"}

// String is the kind's name, empty for one no constant names.
func (k ResourceKind) String() string { return enumString(resourceKinds, k) }

// AuditRecord is one management action, written in the transaction of the
// change it records and never rewritten.
type AuditRecord struct {
	ID [16]byte // minted by the write that records it
	// Time is the store time of the transaction that wrote the record,
	// taken when its committing attempt started, set by the write.
	Time time.Time
	// Actor is the account that acted; the zero principal is the schedule,
	// for work no call started.
	Actor     Principal
	ActorName string // the account's name as it was when it acted
	// Credential is the session or API token ID the actor acted under, or
	// the host for the break-glass reset; empty for the schedule.
	Credential    string
	Node          NodeID // the node that took the request
	Source        string // the client's address
	Request       []byte // method, path, request ID and idempotency key
	Target        ResourceRef
	Before, After uint64 // the target's generation
	Change        []byte // the fields changed, old and new; a secret as "set", never its value
	Outcome       []byte // applied, refused with its problem code, or the operation's ID
}

// AuditFilter selects audit records: those whose store time is in [From,
// To), To zero for no end, and, for each of Actor, Target and Node that is
// set, those naming it. The zero Actor is the schedule's.
type AuditFilter struct {
	From, To time.Time
	Actor    *Principal
	Target   *ResourceRef
	Node     *NodeID
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
