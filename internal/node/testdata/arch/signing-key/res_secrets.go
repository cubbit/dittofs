package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

type secretSetting struct{}

// put is the credential write's own declaration, whose put is exempt.
func (secretSetting) put(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}

// A closure at package level, after the credential write, is checked.
var _ = func(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}

// creds has a method named like the credential write, which is checked.
type creds struct{}

func (creds) put(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}

// put at top level, named like the method, is checked.
func put(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}
