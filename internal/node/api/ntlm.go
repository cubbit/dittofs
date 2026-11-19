package api

import (
	"bytes"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// ntlmKey is the setting that turns NTLM sign-in on and off.
const ntlmKey = "identity.ntlm"

// ntlmOn reports whether the installation's identity.ntlm record s, ok
// false for none, leaves NTLM on: none leaves the default, on.
func ntlmOn(s entity.Setting, ok bool) bool {
	return !ok || string(bytes.TrimSpace(s.Value)) != "false"
}

// ntlmWrite runs write, a write of key at scope inside tx returning whether it
// leaves NTLM on, and, for the installation's identity.ntlm, the rule after
// it, starting through start the NT-hash deletion the rule says the write
// starts. A write of any other key runs alone.
func ntlmWrite(tx entity.ControlPlaneTx, start func(apiv1.OpKind, entity.ResourceRef) error, scope entity.SettingScope, key string, write func() (on bool, err error)) error {
	if scope.Kind != entity.ScopeInstallation || key != ntlmKey {
		_, err := write()
		return err
	}
	old, had, err := tx.Setting(scope, key)
	if err != nil {
		return err
	}
	on, err := write()
	if err != nil {
		return err
	}
	now, _, err := tx.Setting(scope, key)
	if err != nil {
		return err
	}
	begin, err := ntlmRule(tx, ntlmOn(old, had), on, now.Position)
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
