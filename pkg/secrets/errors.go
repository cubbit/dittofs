package secrets

import "errors"

var (
	// ErrWrongRole is a call naming a role other than the one its kind is
	// sealed under.
	ErrWrongRole = errors.New("secrets: wrong role for the secret's kind")
	// ErrWrongKind is a reference whose stored secret is of another kind or
	// owner than the caller expects.
	ErrWrongKind = errors.New("secrets: secret of another kind or owner")
	// ErrKindChanged is a new version naming another kind or owner than the
	// stored one.
	ErrKindChanged = errors.New("secrets: a new version changes the kind or owner")
	// ErrUnproven is a new version of a remote store's credential, refused
	// whatever its value.
	//
	// decision: a remote store's credential changes only once the store's
	// capability check finds the namespace's claim at the location it
	// reaches (RFC 13 §5.2). Neither is built, and a check without the claim
	// would pass any reachable bucket, so the change is refused and fails
	// closed. Withdraw when both are built.
	ErrUnproven = errors.New("secrets: credential change cannot be proven yet (RFC 13 §5.2)")
)
