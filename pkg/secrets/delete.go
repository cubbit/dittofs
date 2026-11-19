package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// errNoWholeDelete is Delete's answer until the metadata store can delete
// a whole secret. It wraps errors.ErrUnsupported, so a caller tells it apart
// from a refusal and the same call will not succeed on this build.
var errNoWholeDelete = fmt.Errorf(
	"secrets: Delete waits for a whole-secret delete in RFC 16 §3: %w",
	errors.ErrUnsupported,
)

// Delete destroys nothing yet: it returns errNoWholeDelete whatever it is
// given. RFC 13 §7 has it destroy every version of the secret, after
// checking the caller's kind and owner, so that a later Resolve of ref
// fails; it does so once RFC 16 §3 can delete a whole secret.
//
// decision: Delete refuses rather than report a destruction it did not
// make. RFC 16 §3 has no whole-secret delete, and the control-plane
// transaction has no secret write. Its kind and owner check, its refusal of
// a credential a store record still names, and its write inside the
// caller's audited Write come with that delete, when Delete takes the
// caller's transaction. Withdraw when RFC 16 §3 gains the delete.
func (p *Provider) Delete(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
) error {
	return errNoWholeDelete
}
