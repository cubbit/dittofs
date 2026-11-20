package identity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// conditionRotationFailed is raised while the installation's last rotation
// is recorded failed (RFC 18 §9.2).
const conditionRotationFailed = "machine_password_rotation_failed"

// mismatchesSpec counts the confirms whose key version number differs from
// the tag sealed with the version.
var mismatchesSpec = metrics.Spec{
	Name: "identity_machine_key_version_mismatches_total",
	Help: "Rotation confirms whose key version number differs from the tag.",
}

// The rotation's phases, in the order its turns run them (RFC 18 §3.5).
const (
	phaseStart    = ""         // refuse a running join, prune
	phasePin      = "pin"      // pin a controller
	phasePrevious = "previous" // wait for this node to hold the current version
	phaseSeal     = "seal"     // seal the new version, or take an unconfirmed one
	phaseApplied  = "applied"  // wait for this node to hold the sealed version
	phaseSet      = "set"      // probe, record the attempt, set
	phaseConfirm  = "confirm"  // probe, read the number, record the rotation
)

var rotatePhases = []string{phaseStart, phasePin, phasePrevious, phaseSeal, phaseApplied, phaseSet, phaseConfirm}

// rotateCursor is the rotation's durable cursor, recorded as the operation's
// Step after each turn.
type rotateCursor struct {
	Phase      string    `json:"phase"`
	DC         string    `json:"dc,omitempty"`      // the pinned controller
	Tag        uint32    `json:"tag,omitempty"`     // the sealed version's key version number
	Version    uint64    `json:"version,omitempty"` // the sealed version
	Attempted  bool      `json:"attempted,omitempty"`
	Unanswered time.Time `json:"unanswered,omitzero"` // when the pinned controller first went unanswered
	Waiting    time.Time `json:"waiting,omitzero"`    // when the wait for the previous version started
}

// rotateResult is a completed rotation's Result.
type rotateResult struct {
	DC      string `json:"dc"`
	Version uint64 `json:"version"`
	Tag     uint32 `json:"tag"`
	Read    uint32 `json:"read"`
}

// machineOwner is the machine password's owner: the installation record's
// machine_password field.
func machineOwner(in entity.Installation) entity.SecretOwner {
	return entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}, Key: "machine_password"}
}

// RotateProgress is the rotation's progress, read from its cursor.
func (s *Server) RotateProgress(_ context.Context, op entity.Operation) apiv1.Progress {
	var c rotateCursor
	_ = json.Unmarshal(op.Step, &c) // no cursor yet reads as the first phase
	return apiv1.Progress{Phase: c.Phase, Done: int64(slices.Index(rotatePhases, c.Phase)), Total: int64(len(rotatePhases)), Unit: "steps"}
}

// RotateStep runs one turn of a rotate-machine-password operation on the
// installation op targets, in RFC 18 §3.5's order, from the cursor op.Step
// holds. A failure that ends the rotation is a *apiv1.Problem naming the
// controller, the version or the join that ended it; every other error is
// retried with the same cursor.
func (s *Server) RotateStep(ctx context.Context, op entity.Operation) (step.StepResult, error) {
	if op.Target.Kind != entity.ResourceInstallation {
		return step.StepResult{}, fmt.Errorf("%w: %s's target %s names no installation", step.ErrStepFailed, op.Kind, op.Target.Kind)
	}
	// decision: a cursor that does not decode, or names no phase, ends the
	// operation as a fault without recording the last rotation failed: it is a
	// programming or storage fault, not a rotation the controller or this node
	// refused, so raising machine_password_rotation_failed would misreport it
	// (RFC 18 §3.5 reports a rotation that cannot complete). Overturned if such
	// a cursor is shown to arise from a rotation's own course.
	var c rotateCursor
	if len(op.Step) > 0 {
		if err := json.Unmarshal(op.Step, &c); err != nil {
			return step.StepResult{}, fmt.Errorf("%w: the rotation's cursor: %w", step.ErrStepFailed, err)
		}
	}
	st := s.state.Load()
	if st == nil {
		return step.StepResult{}, errNotApplied
	}
	if s.deps.Secrets == nil || s.deps.DomainController == nil || s.deps.ControlPlane == nil {
		return step.StepResult{}, fmt.Errorf("%w: no secret provider, domain controller or control plane", step.ErrStepFailed)
	}
	in, err := s.deps.Store.Installation(ctx)
	if err != nil {
		return step.StepResult{}, err
	}
	r := &rotation{s: s, st: st, in: in, c: c}
	if c.Phase != phaseStart && (!in.Join.Joined || st.Directory == nil) {
		return step.StepResult{}, problem(http.StatusConflict, "", "the installation is not joined to a domain")
	}
	var res step.StepResult
	switch c.Phase {
	case phaseStart:
		err = r.start(ctx, op.Target, op.ID)
	case phasePin:
		err = r.pin(ctx)
	case phasePrevious:
		err = r.previous(ctx)
	case phaseSeal:
		err = r.seal(ctx)
	case phaseApplied:
		r.applied()
	case phaseSet:
		err = r.set(ctx)
	case phaseConfirm:
		res, err = r.confirm(ctx)
	default:
		return step.StepResult{}, fmt.Errorf("%w: the rotation's cursor names no phase %q", step.ErrStepFailed, c.Phase)
	}
	if errors.Is(err, errUnanswered) {
		err = nil
	} else if r.c.Phase != c.Phase {
		r.c.Unanswered = time.Time{} // a phase moved on was answered
	}
	if err != nil || res.Done {
		return res, err
	}
	res.Next, err = json.Marshal(r.c)
	return res, err
}

// rotation is one turn of a rotation: the State and Installation it reads
// and the cursor it moves.
type rotation struct {
	s  *Server
	st *State
	in entity.Installation
	c  rotateCursor
}

func problem(status int, code apiv1.Code, detail string) *apiv1.Problem {
	return &apiv1.Problem{Status: status, Code: code, Title: "the machine password rotation failed", Detail: detail}
}

// errNothingPruned is the sentinel of the rotation's first write: the
// error of an attempt that pruned no version, so the write carries no
// record of a change that did not happen and commits nothing. start
// swallows it as the schedule swallows errNotDue.
var errNothingPruned = errors.New("the rotation pruned no version")

// heldVersions lists the secret's versions held from below down to the
// first one not held. Versions are written one above the current and
// pruned from the bottom, so the held ones have no gap, and the bound
// stops at 1 because version 0 reads the current one.
func heldVersions(tx entity.ControlPlaneTx, ref entity.SecretRef, below uint64) ([]uint64, error) {
	var held []uint64

	for n := below; n >= 1; n-- {
		_, ok, err := tx.Secret(ref, n)
		if err != nil {
			return nil, err
		}

		if !ok {
			break
		}

		held = append(held, n)
	}

	return held, nil
}

// missingVersions numbers the versions of held the transaction no longer
// holds: the prune's own deletes, read back inside its write.
func missingVersions(
	tx entity.ControlPlaneTx,
	ref entity.SecretRef,
	held []uint64,
) ([]uint64, error) {
	var gone []uint64

	for _, n := range held {
		_, ok, err := tx.Secret(ref, n)
		if err != nil {
			return nil, err
		}

		if !ok {
			gone = append(gone, n)
		}
	}

	return gone, nil
}

// start refuses a join running on the installation and prunes the versions
// below the current one once the retention has passed since its confirm. The
// refusal is one write, so a join started since makes it run again; the prune
// and its audit record run inside it through tx, so a conflict that reruns
// the write drops its earlier prune and record with the rest.
func (r *rotation) start(
	ctx context.Context,
	target entity.ResourceRef,
	id entity.OperationID,
) error {
	var pruneErr error

	var rec entity.AuditRecord

	err := r.s.deps.ControlPlane.Write(ctx, entity.WriteMeta{Audit: &rec},
		func(tx entity.ControlPlaneTx) error {
			pruneErr = nil

			op, running, err := tx.RunningOperation(string(apiv1.OpJoinDomain), target)
			if err != nil {
				return err
			}

			if running {
				return problem(http.StatusConflict, apiv1.CodeOperationRunning,
					fmt.Sprintf("the join %x is running on the installation", op.ID))
			}

			in, err := tx.Installation()
			if err != nil {
				return err
			}

			if !in.Join.Joined || in.MachinePassword == (entity.SecretRef{}) {
				return problem(
					http.StatusConflict, "", "the installation is not joined to a domain")
			}

			cur, ok, err := tx.Secret(in.MachinePassword, 0)
			if err != nil {
				return err
			}

			lr := in.Join.LastRotation
			// The prune keeps the current version, so it runs only once that is
			// the confirmed one, the last rotation did not fail and the retention
			// since the confirm has passed, when every node holds it (RFC 18
			// §3.5, RFC 13 §7). A failed rotation may have failed because a node
			// lacks the current version, so the prune waits for one that
			// completes.
			//
			// ponytail: a previous version's sealed value stays in the store,
			// used by no key and no channel, until the next rotation prunes it,
			// at most the schedule's period. Prune at the retention's end when
			// a sealed value's life past its use is shown to matter.
			if !ok || lr.Failed || cur.Version != lr.Version ||
				tx.Now().Before(lr.Confirmed.Add(retention+clockSkew)) {
				return errNothingPruned
			}

			held, err := heldVersions(tx, in.MachinePassword, cur.Version-1)
			if err != nil {
				return err
			}

			// The prune runs inside this write, so a conflict that reruns it
			// drops the earlier cut and the record that named it with the
			// rest (RFC 13 §7). A cut that failed half way through rolls back
			// with the whole write.
			pruneErr = r.s.deps.Secrets.Prune(
				ctx, tx, entity.SecretMachinePassword, machineOwner(in), in.MachinePassword,
			)
			if pruneErr != nil {
				return pruneErr
			}

			pruned, err := missingVersions(tx, in.MachinePassword, held)
			if err != nil {
				return err
			}

			if len(pruned) == 0 {
				return errNothingPruned
			}

			// The record names the versions the prune dropped, its own
			// deletes read back inside this same write, so it cannot tell of
			// a drop that rolled back and holds no sealed value (RFC 23 §6,
			// RFC 13 §7). The numbers are sorted, so the record does not
			// depend on the order of the scan.
			slices.Sort(pruned)

			change, err := json.Marshal(struct {
				Pruned []uint64 `json:"pruned"`
			}{pruned})
			if err != nil {
				return err
			}

			rec = entity.AuditRecord{
				Target: entity.ResourceRef{
					Kind: entity.ResourceSecret,
					ID:   in.MachinePassword[:],
				},
				Change:  change,
				Outcome: []byte(uuid.UUID(id).String()),
			}

			return nil
		})
	// A write that pruned nothing records nothing: the sentinel is
	// swallowed as the schedule swallows errNotDue, and the phase moves on.
	if errors.Is(err, errNothingPruned) {
		err = nil
	}

	if err != nil {
		if pruneErr == nil {
			return err
		}

		// decision: a prune error fails the rotation, recorded, rather than
		// being retried, and the aborted write leaves no partial cut behind:
		// RFC 18 §3.5 reports failed a rotation that cannot start by its
		// schedule, and the next scheduled one prunes again. Overturned if a
		// transient provider error is shown to fail rotations a retry would
		// save.
		return r.fail(ctx, fmt.Sprintf("prune the machine password: %v", pruneErr))
	}

	r.c.Phase = phasePin

	return nil
}

// pin picks the controller the rotation reads and sets at: the record's
// address, or the first of the domain's that answers a dial.
func (r *rotation) pin(ctx context.Context) error {
	ctx, done := r.s.bounded(ctx)
	defer done()
	rec := r.st.Directory
	addrs, err := r.s.controllers(ctx, rec)
	if err != nil {
		return r.noAnswer(ctx, err)
	}
	var errs []error
	for _, a := range addrs {
		dc, err := r.s.deps.DomainController.Dial(ctx, *rec, a)
		if err == nil {
			_ = dc.Close()
			r.c.DC, r.c.Phase = a, phasePrevious
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", a, err))
	}
	return r.noAnswer(ctx, fmt.Errorf("no domain controller of %s answers: %w", rec.Realm, errors.Join(errs...)))
}

// holds reports whether this node's State holds version n.
func (r *rotation) holds(n uint64) bool {
	return slices.ContainsFunc(r.st.Machine, func(v secrets.Version) bool { return v.Number == n })
}

// previous waits until this node holds the current version, failing the
// rotation once it has waited for an hour.
//
// ponytail: "every node" is this node: the rotation waits for this node's
// State alone. Wait on every registered node's status with the cluster.
func (r *rotation) previous(ctx context.Context) error {
	cur, err := r.s.deps.ControlPlane.Secret(ctx, r.in.MachinePassword, 0)
	if err != nil {
		return err
	}
	if r.holds(cur.Version) {
		r.c.Phase, r.c.Waiting = phaseSeal, time.Time{}
		return nil
	}
	now := r.s.deps.Clock.Now()
	switch {
	case r.c.Waiting.IsZero():
		r.c.Waiting = now
	case now.Sub(r.c.Waiting) >= unanswered:
		return r.fail(ctx, fmt.Sprintf("this node does not hold the machine password's version %d after %v", cur.Version, unanswered))
	}
	return nil
}

// seal takes the current version when it is unconfirmed and was not handed
// over, a failed rotation's or this one's sealed before a crash, so at most
// one unconfirmed version is in force; otherwise it seals a new version
// tagged with the pinned controller's key version number plus one.
func (r *rotation) seal(ctx context.Context) error {
	cp := r.s.deps.ControlPlane
	ref := r.in.MachinePassword
	cur, err := cp.Secret(ctx, ref, 0)
	if err != nil {
		return err
	}
	if cur.Version > r.in.Join.LastRotation.Version && !cur.HandedOver {
		r.c.Version, r.c.Tag, r.c.Phase = cur.Version, cur.KeyVersion, phaseApplied
		return nil
	}
	acct, err := r.account(ctx)
	if err != nil {
		return err
	}
	pw := []byte(rand.Text() + rand.Text() + rand.Text() + rand.Text())
	defer clear(pw)
	sec, err := r.s.deps.Secrets.Seal(ctx, secrets.Protocol, entity.SecretMachinePassword, machineOwner(r.in), ref, pw,
		secrets.Meta{KeyVersion: acct.KeyVersion + 1})
	if err != nil {
		return err
	}
	// decision: identity writes the machine account's records itself, the
	// sealed version beside Installation.MachinePassword in one write, since
	// the version must be durable before any controller sees its password.
	// Overturned if another component takes the machine account's writes.
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(sec, sec.Version-1); err != nil {
			return err
		}
		return tx.SetMachinePassword(ref)
	})
	if err != nil {
		return err
	}
	r.c.Version, r.c.Tag, r.c.Phase = sec.Version, sec.KeyVersion, phaseApplied
	return nil
}

// applied waits until this node holds the sealed version.
//
// decision: this wait has no bound, unlike the previous version's, because
// RFC 18 §3.5 reports failed only a rotation that cannot start; the sealed
// version is in force already, so waiting costs nothing but the rotation's
// time. Bound it if a node that never applies a version is shown to hold the
// schedule back.
//
// ponytail: "every node" is this node, as at the previous version's wait.
func (r *rotation) applied() {
	if r.holds(r.c.Version) {
		r.c.Phase = phaseSet
	}
}

// versions are the machine password's versions, current first.
func (r *rotation) versions(ctx context.Context) ([]secrets.Version, error) {
	return r.s.deps.Secrets.Versions(ctx, secrets.Protocol, entity.SecretMachinePassword, machineOwner(r.in), r.in.MachinePassword)
}

// value is the sealed version's password among vs. A version no longer held,
// pruned behind the running rotation, fails it, recorded (RFC 18 §3.5).
func (r *rotation) value(ctx context.Context, vs []secrets.Version) ([]byte, error) {
	i := slices.IndexFunc(vs, func(v secrets.Version) bool { return v.Number == r.c.Version })
	if i < 0 {
		return nil, r.fail(ctx, fmt.Sprintf("the machine password's version %d is no longer held", r.c.Version))
	}
	return vs[i].Value, nil
}

// set probes whether the pinned controller accepts the sealed version and
// sets it only if not, over a channel an earlier version, newest first,
// signs in, so a version is never set twice. The first turn that finds it
// not accepted only records the attempt, so the attempt is durable before
// the controller sees the password.
func (r *rotation) set(ctx context.Context) error {
	ctx, done := r.s.bounded(ctx)
	defer done()
	p, err := r.probeSealed(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = p.dc.Close() }()
	vs, pw, dc, account := p.vs, p.pw, p.dc, accountName(r.in.Join.Account)
	switch {
	case p.accepted:
		r.c.Phase = phaseConfirm
		return nil
	case !r.c.Attempted:
		// decision: the attempt is recorded in the cursor, which the runner
		// commits before the next turn sends the password, rather than in a
		// write of its own: the cursor outlives a restart as well, and a
		// failed rotation keeps its version whether or not it was sent.
		// Overturned if anything but this rotation must read the attempt.
		r.c.Attempted = true
		return nil
	}
	var ch netlogon.Channel
	for _, v := range vs {
		if v.Number >= r.c.Version || v.Meta.HandedOver {
			continue
		}
		if ch, err = dc.Channel(ctx, account, v.Value); err == nil {
			break
		} else if !errors.Is(err, netlogon.ErrRejected) {
			return r.noAnswer(ctx, err)
		}
	}
	if ch == nil {
		return r.fail(ctx, fmt.Sprintf("domain controller %s accepts none of the machine password's versions", r.c.DC))
	}
	defer func() { _ = ch.Close() }()
	if err := ch.SetPassword(ctx, pw); errors.Is(err, netlogon.ErrRejected) {
		return r.fail(ctx, fmt.Sprintf("domain controller %s refused the machine password's version %d: %v", r.c.DC, r.c.Version, err))
	} else if err != nil {
		// The set may have reached the controller; the next turn's probe
		// settles it.
		return r.noAnswer(ctx, err)
	}
	r.c.Phase = phaseConfirm
	return nil
}

// sealedProbe is what probeSealed answers: the machine password's versions,
// the sealed version's password among them, the pinned controller still open
// and whether it accepts that password.
type sealedProbe struct {
	vs       []secrets.Version
	pw       []byte
	dc       netlogon.DC
	accepted bool
}

// probeSealed opens the pinned controller and asks whether it accepts the
// sealed version; the caller closes the controller. A controller that cannot
// be asked answers noAnswer's error.
func (r *rotation) probeSealed(ctx context.Context) (sealedProbe, error) {
	vs, err := r.versions(ctx)
	if err != nil {
		return sealedProbe{}, err
	}
	pw, err := r.value(ctx, vs)
	if err != nil {
		return sealedProbe{}, err
	}
	dc, err := r.s.deps.DomainController.Dial(ctx, *r.st.Directory, r.c.DC)
	if err != nil {
		return sealedProbe{}, r.noAnswer(ctx, err)
	}
	accepted, err := probe(ctx, dc, accountName(r.in.Join.Account), pw)
	if err != nil {
		_ = dc.Close()
		return sealedProbe{}, r.noAnswer(ctx, err)
	}
	return sealedProbe{vs: vs, pw: pw, dc: dc, accepted: accepted}, nil
}

// probe asks dc whether it accepts password for account: an error only when
// it could not be asked.
func probe(ctx context.Context, dc netlogon.DC, account string, password []byte) (bool, error) {
	ch, err := dc.Channel(ctx, account, password)
	if err == nil {
		_ = ch.Close()
		return true, nil
	}
	if errors.Is(err, netlogon.ErrRejected) {
		return false, nil
	}
	return false, err
}

// confirm asks the pinned controller whether it accepts the sealed version,
// failing the rotation if not, then reads its key version number: a number
// other than the tag is logged and counted, and the rotation completes
// either way, writing the last rotation and the account's names as it ends.
func (r *rotation) confirm(ctx context.Context) (step.StepResult, error) {
	ctx, done := r.s.bounded(ctx)
	defer done()
	p, err := r.probeSealed(ctx)
	if err != nil {
		return step.StepResult{}, err
	}
	defer func() { _ = p.dc.Close() }()
	if !p.accepted {
		return step.StepResult{}, r.fail(ctx, fmt.Sprintf("domain controller %s no longer accepts the machine password's version %d", r.c.DC, r.c.Version))
	}
	acct, err := r.accountAt(ctx, p.dc)
	if err != nil {
		return step.StepResult{}, r.noAnswer(ctx, err)
	}
	if acct.KeyVersion != r.c.Tag {
		r.s.logger.Warn("identity: the confirmed machine password's key version number differs from its tag",
			"dc", r.c.DC, "version", r.c.Version, "tag", r.c.Tag, "read", acct.KeyVersion)
		r.s.mismatches.WithLabelValues().Inc()
	}
	c := r.c
	result, err := json.Marshal(rotateResult{DC: c.DC, Version: c.Version, Tag: c.Tag, Read: acct.KeyVersion})
	if err != nil {
		return step.StepResult{}, err
	}
	next, err := json.Marshal(c)
	if err != nil {
		return step.StepResult{}, err
	}
	return step.StepResult{Next: next, Done: true, Result: result, End: func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		j := in.Join
		now := tx.Now()
		j.Names = acct.Names
		j.LastRotation.At, j.LastRotation.Failed = now, false
		j.LastRotation.Tag, j.LastRotation.Read = c.Tag, acct.KeyVersion
		j.LastRotation.Version, j.LastRotation.Confirmed = c.Version, now
		return tx.SetMachineJoin(j)
	}}, nil
}

// account reads the computer account at the pinned controller.
func (r *rotation) account(ctx context.Context) (netlogon.Account, error) {
	ctx, done := r.s.bounded(ctx)
	defer done()
	dc, err := r.s.deps.DomainController.Dial(ctx, *r.st.Directory, r.c.DC)
	if err != nil {
		return netlogon.Account{}, r.noAnswer(ctx, err)
	}
	defer func() { _ = dc.Close() }()
	a, err := r.accountAt(ctx, dc)
	if err != nil {
		return netlogon.Account{}, r.noAnswer(ctx, err)
	}
	return a, nil
}

// accountAt reads the computer account at dc, bound with the directory
// record's bind credential.
func (r *rotation) accountAt(ctx context.Context, dc netlogon.DC) (netlogon.Account, error) {
	pw, err := r.s.bindPassword(ctx, r.st.Directory.BindCredential)
	if err != nil {
		return netlogon.Account{}, err
	}
	defer clear(pw)
	return dc.Account(ctx, identity.JoinCredential{User: r.st.Directory.BindName, Password: pw}, accountName(r.in.Join.Account))
}

// errUnanswered ends a turn whose controller did not answer within the
// hour: RotateStep records the cursor, and the next turn asks again.
var errUnanswered = errors.New("identity: the rotation's domain controller did not answer")

// noAnswer records when the pinned controller first went unanswered and
// fails the rotation, naming it, once it has gone unanswered for an hour;
// below the hour it answers errUnanswered.
//
// decision: the hour covers every call to the controller, the pin and the
// number read before the seal among them, not only the set and the confirm
// RFC 18 §3.5 names, so no controller call retries forever. Overturned if a
// rotation must wait out a domain with no controller for longer.
func (r *rotation) noAnswer(ctx context.Context, err error) error {
	now := r.s.deps.Clock.Now()
	if r.c.Unanswered.IsZero() {
		r.c.Unanswered = now
		r.s.logger.Warn("identity: the rotation's domain controller did not answer", "dc", r.c.DC, "err", err)
	}
	if now.Sub(r.c.Unanswered) < unanswered {
		return errUnanswered
	}
	name := r.c.DC
	if name == "" {
		name = r.st.Directory.Realm
	}
	return r.fail(ctx, fmt.Sprintf("domain controller %s unanswered for %v: %v", name, unanswered, err))
}

// fail records the last rotation failed, moving its time and leaving the
// version and the confirm as they were, then answers the problem that fails
// the operation.
func (r *rotation) fail(ctx context.Context, detail string) error {
	// The turn's bound may have ended ctx; the record is still written.
	err := r.s.deps.ControlPlane.Write(context.WithoutCancel(ctx), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		j := in.Join
		j.LastRotation.At, j.LastRotation.Failed = tx.Now(), true
		return tx.SetMachineJoin(j)
	})
	if err != nil {
		return err
	}
	return problem(http.StatusServiceUnavailable, apiv1.CodeUnavailable, detail)
}

// bounded is ctx ended once channelBound passes on the server's clock, so a
// controller that stops answering ends the turn rather than holding it.
func (s *Server) bounded(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	bound := s.deps.Clock.After(channelBound)
	go func() {
		select {
		case <-bound:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// rotationConditions appends machine_password_rotation_failed while the
// applied State's last rotation is recorded failed: only a completed
// rotation clears it, never a channel set up or a restart.
func (s *Server) rotationConditions(dst []health.Condition) []health.Condition {
	st := s.state.Load()
	if st == nil || !st.Join.Joined || !st.Join.LastRotation.Failed {
		return dst
	}
	return append(dst, health.Condition{
		Name:    conditionRotationFailed,
		Subject: health.Subject{Scope: health.ScopeInstallation},
		Cause:   fmt.Errorf("%w: the machine password's last rotation failed", vfserr.ErrUnavailable),
		Detail:  fmt.Sprintf("the machine password's last rotation failed at %s", st.Join.LastRotation.At.Format(time.RFC3339)),
	})
}
