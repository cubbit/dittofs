package entity

import (
	"errors"
	"time"
)

// ErrGenerationMismatch is a write whose expect is not the generation, or
// the version, of the record as stored: the caller read an older one, or
// expected none where one exists. Nothing was written.
var ErrGenerationMismatch = errors.New("entity: record changed since it was read")

// ErrIdempotencyMismatch is a request whose idempotency key its account
// already used for a request with another digest. Nothing was written.
var ErrIdempotencyMismatch = errors.New("entity: idempotency key used for another request")

// IdempotentReplay is a request whose idempotency key its account already
// used for the same request: Answer is the answer recorded then, and nothing
// was written.
type IdempotentReplay struct{ Answer []byte }

func (*IdempotentReplay) Error() string {
	return "entity: request already answered under its idempotency key"
}

// Alloc names the IDs a user or group write draws from the store's uid, gid
// and RID counters in its own transaction. An ID it does not name is written
// as the record holds it.
type Alloc struct{ UID, GID, RID bool }

// WriteMeta is what a control-plane write records beside its change, each in
// the change's own transaction. The write's function may complete the
// records through these pointers: the audit record's After and Outcome, the
// idempotency record's Answer.
type WriteMeta struct {
	Audit       *AuditRecord       // nil for a write nothing audits
	Idempotency *IdempotencyRecord // nil for a request that carries no key
}

// ControlPlaneTx is the control plane's writes and tracked reads inside one
// write's transaction. Each write checks its expect as the view's method
// does and returns the record's new generation, or version, the value a
// read after the commit reports and the next write passes as its expect; a
// quota that limits nothing is deleted and returns zero. A deletion is its
// first transaction, the mark. A read is tracked, so
// a concurrent write of what it read makes the transaction retry, and the
// write's function runs again.
type ControlPlaneTx interface {
	Setting(scope SettingScope, key string) (Setting, bool, error)
	User(p Principal) (User, bool, error)
	Share(id ShareID) (Share, bool, error)
	// EnabledAdmins counts the enabled users whose role is admin, tracked
	// over the whole set, so two writes that each remove one of the last
	// ones conflict.
	EnabledAdmins() (int, error)
	Installation() (Installation, error)
	// SetNTLMOff records a write turning identity.ntlm off at settings
	// generation gen, clearing the NT-hash deletion completed before it.
	SetNTLMOff(gen uint64) error
	// SetNTHashDeleted records the NT-hash deletion op as completed.
	SetNTHashDeleted(op OperationID) error
	SetMachineJoin(j MachineJoin) error
	// Now is the transaction's store time, fixed for its life; a retried
	// attempt gets a new one.
	Now() time.Time

	DeleteUser(p Principal, expect uint64) error
	DeleteGroup(p Principal, expect uint64) error
	DeleteShare(share ShareID, snapshots SnapshotDisposition, expect uint64) error
	PutUser(u User, expect uint64, a Alloc) (uint64, error)
	PutGroup(g Group, expect uint64, a Alloc) (uint64, error)
	PutShare(s Share, expect uint64) (uint64, error)
	SetShareState(share ShareID, from, to ShareState) error
	SetGrant(g ShareGrant, expect uint64) (uint64, error)
	DeleteGrant(share ShareID, p Principal, expect uint64) error
	PutExportPolicy(p ExportPolicy, expect uint64) (uint64, error)
	SetQuota(share ShareID, q Quota, expect uint64) (uint64, error)
	PutNetgroup(n Netgroup, expect uint64) (uint64, error)
	DeleteNetgroup(name string, expect uint64) error
	PutSetting(s Setting, expect uint64) (uint64, error)
	AddMember(m Membership) error
	RemoveMember(m Membership) error
}
