package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// handOver is named like the offline join's write, in another file, so its
// put is checked.
func handOver(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}

// secretSetting is named like the credential write's type, in another file,
// so its put is checked.
type secretSetting struct{ kind entity.SecretKind }

func (secretSetting) put(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}
