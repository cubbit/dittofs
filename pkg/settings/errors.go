package settings

import "errors"

// The refusals Check returns, each matchable with errors.Is.
var (
	ErrUnknownKey  = errors.New("settings: unknown key")
	ErrWrongScope  = errors.New("settings: key written at a scope it does not have")
	ErrNewerSchema = errors.New("settings: record schema newer than this node reads")
	ErrForbidden   = errors.New("settings: caller's role may not write this section")
	ErrRequired    = errors.New("settings: required key has no value")
	ErrInvalid     = errors.New("settings: value refused by its section's validator")
	ErrBound       = errors.New("settings: bound setting changed while content exists")
	ErrSecretRef   = errors.New(
		"settings: reference names no secret or one of another kind or owner",
	)
	ErrFormatVersion = errors.New("settings: format version outside this node's range")

	// ErrUnproven refuses a change of a remote store's endpoint, bucket,
	// prefix or credential reference.
	//
	// decision: such a change is refused until the store's capability check
	// can find the namespace's claim at the new location (RFC 13 §5.2), since
	// a check without the claim would pass any reachable bucket; a store
	// location's first value is no change and passes. Withdraw it once both
	// the capability check and the claim are built.
	ErrUnproven = errors.New("settings: store location change not provable yet (RFC 13 §5.2)")
)
