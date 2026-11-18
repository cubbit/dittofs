package kvstore

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Secret reads one version of a secret, still sealed; version 0 reads the
// current one, and a pruned one is not found.
func (c ControlPlane) Secret(
	ctx context.Context,
	id entity.SecretRef,
	version uint64,
) (entity.Secret, error) {
	return entity.Secret{}, unbuilt("Secret", 4)
}

func (c controlTx) Secret(id entity.SecretRef, version uint64) (entity.Secret, bool, error) {
	return entity.Secret{}, false, unbuilt("Secret", 4)
}

// PutSecret writes a new version, expect naming the current one (0 for the
// first), raising the settings generation so every node resolves it; refused
// if its Kind or Owner differs from the current one's.
func (c ControlPlane) PutSecret(ctx context.Context, s entity.Secret, expect uint64) error {
	return unbuilt("PutSecret", 4)
}

func (c controlTx) PutSecret(s entity.Secret, expect uint64) error {
	return unbuilt("PutSecret", 4)
}

// PruneSecret deletes every version below the one named, never the current
// one.
func (c ControlPlane) PruneSecret(ctx context.Context, id entity.SecretRef, below uint64) error {
	return unbuilt("PruneSecret", 4)
}

func (c controlTx) PruneSecret(id entity.SecretRef, below uint64) error {
	return unbuilt("PruneSecret", 4)
}
