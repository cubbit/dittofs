package api

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// rotate is a route rewriting the signing key itself: it reads the
// installation, then the key's current version, and puts the next one.
func rotate(tx entity.ControlPlaneTx) error {
	in, err := tx.Installation()
	if err != nil {
		return err
	}
	cur, ok, err := tx.Secret(in.SigningKey, 0)
	if err != nil || !ok {
		return err
	}
	cur.Kind = entity.SecretKey
	cur.Version++
	return tx.PutSecret(cur, cur.Version-1)
}

// dropKey drops a secret's versions, which only the rotation may.
func dropKey(tx entity.ControlPlaneTx, ref entity.SecretRef) error {
	return tx.PruneSecret(ref, 1)
}

// repoint names another secret as the signing key, which writes the key
// without its field, its kind or a put.
func repoint(tx entity.ControlPlaneTx, ref entity.SecretRef) error {
	return tx.SetSigningKey(ref)
}
