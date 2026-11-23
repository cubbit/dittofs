package api

import (
	"context"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// ntlmRight is the right a write of the installation's identity.ntlm asks,
// and so the right that starts the NT-hash deletion, which only that write
// starts; a cancel of the deletion asks it too (RFC 23 §4.1).
const ntlmRight = auth.Installation

// ntlmOn reports whether the installation's identity.ntlm record s, ok
// false for none, leaves NTLM on, as auth.NTLM decodes it, refusing a value
// that is no bool.
func ntlmOn(s entity.Setting, ok bool) (bool, error) {
	n, err := auth.NTLM(s, ok)
	return n.On, err
}

// ntlmWrite runs write, a write of key at scope inside tx, and, for the
// installation's identity.ntlm, the rule after it, reading whether NTLM was
// and is on from the record before and after, starting through start the
// NT-hash deletion the rule says the write starts. That write is refused 403
// to a caller not holding the installation right. A write of any other key
// runs alone.
func ntlmWrite(ctx context.Context, tx entity.ControlPlaneTx, start func(apiv1.OpKind, entity.ResourceRef) error, scope entity.SettingScope, key string, write func() error) error {
	// decision: the admin gate and the rule cover identity.ntlm at the
	// installation scope only; the settings table's scope check is what
	// refuses, or leaves inert, the key at any other scope. Overturned if the
	// table admits identity.ntlm at another scope that identity reads.
	if scope.Kind != entity.ScopeInstallation || key != auth.NTLMKey {
		return write()
	}
	// decision: identity.ntlm takes the installation right, an admin's, as
	// the keytab's writes do, rather than the operator's right over
	// settings: it decides whether NTLM sign-in is accepted at all, and
	// turning it off starts the irreversible deletion of every NT hash.
	// Overturned if the role table gives sign-in settings to an operator.
	if c, _ := auth.CallerFrom(ctx); !holds(c, ntlmRight) {
		return &refusal{code: apiv1.CodeForbidden, detail: "only an admin writes " + auth.NTLMKey}
	}
	old, had, err := tx.Setting(scope, key)
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	now, has, err := tx.Setting(scope, key)
	if err != nil {
		return err
	}
	was, err := ntlmOn(old, had)
	if err != nil {
		return err
	}
	on, err := ntlmOn(now, has)
	if err != nil {
		return err
	}
	begin, err := ntlmRule(tx, was, on, now.Position) // a delete leaves no record, and NTLM on, so no Off
	if err != nil || !begin {
		return err
	}
	in, err := tx.Installation()
	if err != nil {
		return err
	}
	return start(apiv1.OpNTHashDelete, installationRef(in))
}

// ntlmRule is the rule for turning NTLM back on, run in the transaction of a
// write of the installation's identity.ntlm after the write: was is whether
// NTLM was on before it, on whether it is on after it, and gen the settings
// generation it raised. It reads Installation.NTLMOff and the running
// NT-hash deletion tracked in tx, and returns whether the write starts one.
//
//   - A write leaving NTLM on is refused 409 operation_running while Off is
//     set and Deleted is zero: no deletion has completed since the last
//     turn-off.
//   - A write turning it off from on sets Off to gen and clears Deleted, and
//     starts a deletion unless one runs.
//   - A write setting it off while it is already off changes neither, and
//     starts a deletion only when Deleted is zero and none runs, which is
//     how a failed one is finished.
//
// ponytail: a turn-on refused with no deletion running names none, since a
// transaction lists no operations, so the failed deletion is found in the
// operation list rather than in the refusal. Name it once the store keeps the
// last deletion's ID beside NTLMOff.
func ntlmRule(tx entity.ControlPlaneTx, was, on bool, gen uint64) (start bool, err error) {
	in, err := tx.Installation()
	if err != nil {
		return false, err
	}
	running, runs, err := tx.RunningOperation(string(apiv1.OpNTHashDelete), installationRef(in))
	if err != nil {
		return false, err
	}
	deleted := in.NTLMOff.Deleted != (entity.OperationID{})
	switch {
	case on:
		if in.NTLMOff.Off == 0 || deleted {
			return false, nil
		}
		if runs {
			return false, &entity.OperationRunningError{ID: running.ID}
		}
		return false, &refusal{code: apiv1.CodeOperationRunning, detail: "the last deletion of the stored NT hashes failed: set identity.ntlm off again to start another, and on once it has completed"}
	case was:
		return !runs, tx.SetNTLMOff(gen)
	}
	return !deleted && !runs, nil
}
