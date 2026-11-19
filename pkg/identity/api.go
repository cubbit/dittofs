package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
