package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// handOver is the offline join's own declaration, whose put is exempt.
func handOver(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}

// joiner has a method named like the offline join's write, which is checked.
type joiner struct{}

func (joiner) handOver(tx entity.ControlPlaneTx, s entity.Secret) error {
	return tx.PutSecret(s, 0)
}
