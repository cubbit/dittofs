package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// handedOver refuses an offline join whose version, the one its params name,
// is not the machine password's current version marked HandedOver, naming
// it, so a hand-over that lost its mark, or a version written since, fails
// closed. An online join passes.
func (j *joining) handedOver(tx entity.ControlPlaneTx) error {
	if !j.offline {
		return nil
	}
	in, err := tx.Installation()
	if err != nil {
		return err
	}
	cur, ok, err := tx.Secret(in.MachinePassword, 0)
	if err != nil {
		return err
	}
	if in.MachinePassword == (entity.SecretRef{}) || !ok || cur.Version != j.handed || !cur.HandedOver {
		return joinProblem(http.StatusConflict, "", fmt.Sprintf("the machine password's version %d is not the current one marked handed over", j.handed))
	}
	return nil
}

// verify reads the account an offline join takes over the directory's bind
// credential, refusing one whose msDS-SupportedEncryptionTypes lacks either
// AES bit before any channel signs in with the handed-over password, then
// asks the pinned controller whether it accepts that password and records the
// account's key version number, read before the probe, in the cursor. The
// account's names are logged here and reported in the join's result.
func (j *joining) verify(ctx context.Context) error {
	ctx, done := j.s.bounded(ctx)
	defer done()
	dc, err := j.s.deps.DomainController.Dial(ctx, *j.st.Directory, j.c.DC)
	if err != nil {
		return j.noAnswer(ctx, err)
	}
	defer func() { _ = dc.Close() }()
	acct, err := j.accountAt(ctx, dc)
	switch {
	case errors.Is(err, netlogon.ErrNoAccount):
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("domain controller %s holds no computer account %s$", j.c.DC, j.c.Account))
	case err != nil:
		return err
	case acct.EncTypes&directory.AESEncTypes != directory.AESEncTypes:
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("the computer account %s$'s msDS-SupportedEncryptionTypes is %#x; set it to %#x", j.c.Account, acct.EncTypes, directory.AESEncTypes))
	}
	j.s.logger.Info("identity: the offline join's computer account holds these service names", "account", j.c.Account+"$", "names", acct.Names)
	pw, err := j.sealed(ctx, j.handed)
	if err != nil {
		return err
	}
	defer clear(pw)
	ok, err := probe(ctx, dc, j.c.Account, pw)
	switch {
	case err != nil:
		return j.noAnswer(ctx, err)
	case !ok:
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("domain controller %s does not accept the handed-over password of %s$", j.c.DC, j.c.Account))
	}
	j.c.Number, j.c.Phase = acct.KeyVersion, joinSeal1
	return nil
}

// closingSeal seals the offline join's closing rotation round's version,
// tagged with the number the verify read plus round, or takes the current
// version when it is one this join sealed with that tag, not handed over, so
// a crash after the seal commits appends no second version.
func (j *joining) closingSeal(ctx context.Context, round int) error {
	tag := j.c.Number + uint32(round)
	cur, err := j.s.deps.ControlPlane.Secret(ctx, j.in.MachinePassword, 0)
	if err != nil {
		return err
	}
	v := cur.Version
	if v <= j.handed || cur.HandedOver || cur.KeyVersion != tag {
		if v, err = j.sealNew(ctx, tag); err != nil {
			return err
		}
	}
	j.c.Version, j.c.Tag, j.c.Attempted, j.c.Phase = v, tag, false, joinSet1
	if round == 2 {
		j.c.Phase = joinSet2
	}
	return nil
}

// closingSet probes whether the pinned controller accepts closing rotation
// round's sealed version and sets it only if not, over a channel one version
// signs in: the handed-over version for the first closing rotation, the
// first's for the second, and no other, unlike a rotation's set, which tries
// each earlier version newest first, since the join knows which one the
// controller holds. The first turn that finds it not accepted only records
// the attempt, in the cursor, as rotation.set's decision: says, so the
// attempt is durable before the controller sees the password.
//
// decision: an offline join's first closing rotation sets its password over
// a secure channel the handed-over password authenticates, so a creator of
// the account on the network path while the join runs can learn the first
// rotation's password, and through it the second's; no key is derived from
// the handed-over password, and the second confirm proves no third set at the
// pinned controller, but nothing in the code can keep the creator off the
// path, which the operator guides say. Overturned when a deployment cannot
// keep the creator off the path: the closing rotations' passwords are then set
// over the directory's verified TLS.
func (j *joining) closingSet(ctx context.Context, round int) error {
	ctx, done := j.s.bounded(ctx)
	defer done()
	dc, accepted, err := j.probe(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()
	signer, next := j.handed, joinConfirm1
	if round == 2 {
		signer, next = j.c.First, joinConfirm2
	}
	switch {
	case accepted:
		j.c.Phase = next
		return nil
	case !j.c.Attempted:
		j.c.Attempted = true
		return nil
	}
	with, err := j.sealed(ctx, signer)
	if err != nil {
		return err
	}
	defer clear(with)
	ch, err := dc.Channel(ctx, j.c.Account, with)
	switch {
	case errors.Is(err, netlogon.ErrRejected):
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("domain controller %s no longer accepts the machine password's version %d to set version %d", j.c.DC, signer, j.c.Version))
	case err != nil:
		return j.noAnswer(ctx, err)
	}
	defer func() { _ = ch.Close() }()
	pw, err := j.sealed(ctx, j.c.Version)
	if err != nil {
		return err
	}
	defer clear(pw)
	if err := ch.SetPassword(ctx, pw); errors.Is(err, netlogon.ErrRejected) {
		return j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("domain controller %s refused the machine password's version %d: %v", j.c.DC, j.c.Version, err))
	} else if err != nil {
		// The set may have reached the controller; the next turn's probe
		// settles it.
		return j.noAnswer(ctx, err)
	}
	j.c.Phase = next
	return nil
}
