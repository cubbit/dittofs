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

// ErrOperationRunning is an operation started on a target while another of
// its kind runs there. Nothing was written.
var ErrOperationRunning = errors.New("entity: an operation of this kind already runs on this target")

// OperationRunningError is ErrOperationRunning naming the operation that
// runs: ID is the one a caller polls instead of starting its own.
type OperationRunningError struct{ ID OperationID }

func (e *OperationRunningError) Error() string { return ErrOperationRunning.Error() }

func (e *OperationRunningError) Is(target error) bool { return target == ErrOperationRunning }

// IdempotentReplay is a request whose idempotency key its account already
// used for the same request: Answer is the answer recorded then, and nothing
// was written.
type IdempotentReplay struct{ Answer []byte }

func (*IdempotentReplay) Error() string {
	return "entity: request already answered under its idempotency key"
}

// IDRange is the span uids and gids are allocated from, both ends included.
type IDRange struct{ Lo, Hi uint32 }

// Alloc names the IDs a user or group create draws from the store's uid, gid
// and RID counters in its own transaction. An ID it does not name is written
// as the record holds it. Range is the span a named uid or gid is drawn from,
// as the caller read the installation's range setting; a RID is appended to
// the installation's domain SID. A write that is not a create names none.
//
// decision: a missing uid or gid is allocated, never filled with a default:
// a default makes every principal without one the same principal to every
// check that reads a uid. So the write names what the store allocates, and a
// zero ID is never read as "allocate one". Overturned only by a client
// protocol that must derive a uid from another spelling and cannot be given
// an allocated one.
//
// decision: the caller passes the range rather than the store reading the
// setting, which would couple the store to that setting's encoding. A caller
// that read the range before a change commits still allocates from the old
// one, and since the counters never go back, a range ending at or below a
// counter's last ID allocates nothing more: every create naming it fails
// ErrNoSpace until the range is widened. Withdraw it if the store comes to
// decode settings itself.
type Alloc struct {
	UID, GID, RID bool
	Range         IDRange
}

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
	User(p Principal) (User, bool, error) // false for a user or machine marked deleted, or a record of another kind than p's
	Share(id ShareID) (Share, bool, error)
	// ExportPolicy is the share's export policy; the default one, version
	// zero, when none is stored.
	ExportPolicy(share ShareID) (ExportPolicy, error)
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
	// SetMachinePassword names the secret holding the computer account's
	// password, in the write that seals its version.
	SetMachinePassword(ref SecretRef) error
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
	// PutOperation creates an operation, expect zero, or changes the one
	// whose generation expect names. Its kind and target never change, and
	// an ended one does not change at all.
	PutOperation(op Operation, expect uint64) (uint64, error)
	// RunningOperation is the operation of kind running on target; false
	// when none runs.
	RunningOperation(kind string, target ResourceRef) (Operation, bool, error)
	AddMember(m Membership) error
	RemoveMember(m Membership) error

	Namespace(id NamespaceID) (Namespace, bool, error)
	// StoreNamed reports whether any namespace names the remote store,
	// tracked over the store's whole index, so a namespace created on it
	// concurrently conflicts.
	StoreNamed(store [16]byte) (bool, error)
	// NamespaceNamed reports whether any share names the namespace, tracked
	// over the whole set, so a share created into it concurrently conflicts.
	NamespaceNamed(ns NamespaceID) (bool, error)
	PutNamespace(n Namespace, expect uint64) (uint64, error)
	DeleteNamespace(id NamespaceID, expect uint64) error
	Session(id [16]byte) (Session, bool, error)
	// SessionByRefresh finds the session a refresh hash was issued to,
	// whether it is the session's current hash or one it replaced.
	SessionByRefresh(hash [32]byte) (Session, bool, error)
	// PutSession creates s; UpdateSession rewrites an existing one, keeping
	// every refresh hash it had before indexed beside s.Refresh. Either
	// refuses a refresh hash indexed to another session.
	PutSession(s Session) error
	UpdateSession(s Session) error
	// DeleteSession deletes the session and every refresh hash it had; an
	// absent session is already deleted.
	DeleteSession(id [16]byte) error
	APIToken(id [16]byte) (APIToken, bool, error)
	APITokenByHash(hash [32]byte) (APIToken, bool, error)
	// PutAPIToken creates t; a token is never rewritten, and its hash names
	// no other token.
	PutAPIToken(t APIToken) error
	DeleteAPIToken(id [16]byte) error
	SignInFailures(key SignInKey) (SignInFailures, bool, error)
	// PutSignInFailures writes the count and moves its expiry with it: a
	// source's 15 min after Failed, a pair's 24 h after Last; an account's
	// does not expire.
	PutSignInFailures(key SignInKey, f SignInFailures) error
	DeleteSignInFailures(key SignInKey) error
}
