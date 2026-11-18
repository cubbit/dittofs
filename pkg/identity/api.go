package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// APISignIn is the management API's account check. The API calls identity
// directly, never through the filesystem: it signs in over no file protocol
// and acts on no share.
type APISignIn interface {
	// Lookup resolves a name as presented to its account's principal by the
	// name's user part, without its domain or realm, folded as names are, so
	// every spelling of one account gives one principal; the zero principal
	// for a user part with no account. It reads the store only: it never
	// queries a directory and never mints a principal. The API counts an
	// attempt against what it returns before VerifyPassword runs.
	Lookup(ctx context.Context, name string) (entity.Principal, error)
	// VerifyPassword checks p's password against its stored hash. A wrong
	// password, the zero principal, a disabled or deleted account, and an
	// account with no password all fail vfserr.ErrLogonFailure, alike;
	// vfserr.ErrUnavailable when the store cannot answer.
	VerifyPassword(ctx context.Context, p entity.Principal, password []byte) error
	// Account returns p's current record for the API's per-call check;
	// vfserr.ErrLogonFailure if disabled or deleted.
	//
	// decision: it reads the principals generation on every call, never the
	// one Apply last delivered, and anything it caches is keyed under that
	// read, so a disable or a raised not-before reaches the API within a
	// second however late the next Apply comes.
	Account(ctx context.Context, p entity.Principal) (entity.User, error)
}

// PasswordHashes is what NewHashes derives: the argon2id hash, and the NT
// hash or nil.
type PasswordHashes struct{ Password, NTHash []byte }

// NTLMSetting is the NTLM setting as a password set reads it: whether it is
// on, and the setting record's generation the write is conditional on.
type NTLMSetting struct {
	On         bool
	Generation uint64
}

// NewHashes derives what a new password is stored as: the password hash and,
// only while ntlm is on and u holds file spellings and is not a directory
// principal, the NT hash. It is pure: it reads and writes nothing. The API
// writes the result with the record, conditional on ntlm.Generation still being
// current; for a directory principal it fails vfserr.ErrInvalid.
//
// ponytail: it answers vfserr.ErrUnavailable, deriving nothing, so a caller
// wired to it before the hashing exists sets no password. Replace the body with
// the argon2id and NT hash derivation, which the principals' password checks
// are the first to need; x/crypto's argon2 and md4 then join the leaf list in
// arch_test.go, being libraries outside the module that import none of it.
func NewHashes(u entity.User, password []byte, ntlm NTLMSetting) (PasswordHashes, error) {
	return PasswordHashes{}, vfserr.ErrUnavailable
}
