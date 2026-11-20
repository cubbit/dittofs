package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// handOver is named like the offline join's write, in another file, so its
// put is checked.
func handOver(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}
