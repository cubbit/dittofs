package secrets

import "github.com/cubbit/dittofs/pkg/secrets/secreterr"

// The provider's refusals, each matchable with errors.Is; package secreterr
// says what each refuses.
var (
	ErrWrongRole   = secreterr.ErrWrongRole
	ErrWrongKind   = secreterr.ErrWrongKind
	ErrKindChanged = secreterr.ErrKindChanged
	ErrUnproven    = secreterr.ErrUnproven
)
