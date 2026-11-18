package entity

import (
	"errors"
	"iter"
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
	User(p Principal) (User, bool, error) // false for a user marked deleted
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

	// DeleteSetting removes an override, so the wider scope's value is
	// effective, and raises the settings generation as a put does.
	DeleteSetting(scope SettingScope, key string, expect uint64) error
	// AllSettings reads every scope's settings in one scan, tracked.
	AllSettings() iter.Seq2[Setting, error]
	// Secret is one version of a secret, still sealed; version 0 reads the
	// current one. A tracked read, so a write that checks a reference's kind
	// and owner conflicts with a concurrent change of the secret. False when
	// no such version is held: it was pruned, or never written.
	Secret(id SecretRef, version uint64) (Secret, bool, error)
	// PutSecret writes a new version, expect naming the current one (0 for
	// the first), and raises the settings generation; refused if its Kind or
	// Owner differs from the current one's.
	PutSecret(s Secret, expect uint64) error
	// PruneSecret deletes every version below the one named, never the
	// current one.
	PruneSecret(id SecretRef, below uint64) error
}
