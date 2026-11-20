package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// putCredential is named like the credential write, in another file, so
// its put is checked.
func putCredential(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}
