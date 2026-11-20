package kvstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// secretPrefix holds every version of a secret, one key each, the version
// big-endian so the current one sorts last.
func secretPrefix(id entity.SecretRef) []byte {
	return keys.ID([]byte{keys.KindSEC}, id[:])
}

func secretKey(id entity.SecretRef, version uint64) []byte {
	return keys.Uint64(secretPrefix(id), version)
}

// secretIn reads one version of a secret, version 0 the current one; false
// when no such version is held.
func secretIn(r kv.Reader, id entity.SecretRef, version uint64) (entity.Secret, bool, error) {
	if version != 0 {
		b, ok, err := get(r, secretKey(id, version))
		if err != nil || !ok {
			return entity.Secret{}, false, err
		}

		s, err := decodeSecret(id, version, b)

		return s, err == nil, err
	}

	// Before finds the last key strictly below its bound, so the bound sorts
	// above the key of version MaxUint64: the prefix and nine 0xff bytes.
	prefix := secretPrefix(id)
	bound := append(slices.Clone(prefix), bytes.Repeat([]byte{0xff}, 9)...)

	row, ok, err := r.Before(prefix, bound)
	if err != nil || !ok {
		return entity.Secret{}, false, err
	}

	v, err := secretVersion(prefix, row.Key)
	if err != nil {
		return entity.Secret{}, false, err
	}

	s, err := decodeSecret(id, v, row.Value)

	return s, err == nil, err
}

// secretVersion is the version a secret's key names after its prefix.
func secretVersion(prefix, key []byte) (uint64, error) {
	if len(key) != len(prefix)+8 {
		return 0, errMalformed
	}

	return binary.BigEndian.Uint64(key[len(prefix):]), nil
}

// Secret reads one version of a secret, still sealed; version 0 reads the
// current one, and a pruned one is not found.
func (c ControlPlane) Secret(
	ctx context.Context,
	id entity.SecretRef,
	version uint64,
) (entity.Secret, error) {
	var out entity.Secret

	err := c.s.kv.View(ctx, func(r kv.Reader) error {
		s, ok, err := secretIn(r, id, version)
		if err != nil {
			return err
		}

		if !ok {
			return merrs.NewNotFoundError(fmt.Sprintf("%x@%d", id[:], version), "secret")
		}

		out = s

		return nil
	})

	return out, err
}

// Secret reads as secretIn does, tracked: the version-0 read guards every
// version of the reference, since a new version is a new key that a point
// read would not see, so a write that checks a reference conflicts with a
// concurrent put. Another version is read with a Get, tracked as it is.
func (c controlTx) Secret(id entity.SecretRef, version uint64) (entity.Secret, bool, error) {
	if version == 0 {
		if err := c.tx.GuardRange(secretPrefix(id)); err != nil {
			return entity.Secret{}, false, err
		}
	}

	return secretIn(c.tx, id, version)
}

// PutSecret writes a new version, expect naming the current one (0 for the
// first), raising the settings generation so every node resolves it; refused
// if its Kind or Owner differs from the current one's.
func (c ControlPlane) PutSecret(ctx context.Context, s entity.Secret, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error {
		return tx.PutSecret(s, expect)
	})
}

// PutSecret checks and writes the version in the caller's transaction.
//
// decision: beyond a kind or owner change, a put whose version is not the
// one after expect, or with the zero reference or an undeclared kind, is
// refused. A version below the current one would be written and never read,
// one over a retained version would replace it, a secret under the zero
// reference is one a user's deletion skips, and one of an undeclared kind is
// one no read decodes. Every caller seals expect+1. Withdraw a part if the
// store ever renumbers a version, or names a secret without a reference.
func (c controlTx) PutSecret(s entity.Secret, expect uint64) error {
	switch {
	case s.ID == entity.SecretRef{}:
		return merrs.NewInvalidArgumentError("a secret needs a reference")
	case s.Kind.String() == "":
		return merrs.NewInvalidArgumentError("a secret needs a declared kind")
	case s.Version == 0 || s.Version-1 != expect:
		return merrs.NewInvalidArgumentError("a secret's new version follows the one expected")
	}

	if err := c.tx.GuardRange(secretPrefix(s.ID)); err != nil {
		return err
	}

	cur, ok, err := secretIn(c.tx, s.ID, 0)
	if err != nil {
		return err
	}

	if err := expected(cur.Version, expect); err != nil {
		return err
	}

	if ok && (cur.Kind != s.Kind || !cur.Owner.Equal(s.Owner)) {
		return merrs.NewInvalidArgumentError("a secret's new version keeps its kind and owner")
	}

	if _, err := raise(c.tx, cfgGenKey); err != nil {
		return err
	}

	return c.tx.Set(secretKey(s.ID, s.Version), encodeSecret(s))
}

// PruneSecret deletes every version below the one named, never the current
// one, in a write of its own that records no audit and checks no owner.
// Callers go through Provider.Prune inside an audited Write.
func (c ControlPlane) PruneSecret(ctx context.Context, id entity.SecretRef, below uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error {
		return tx.PruneSecret(id, below)
	})
}

// PruneSecret deletes the versions below the one named in the caller's
// transaction, and keeps the current one whatever below is. The current
// version is read tracked, so a seal that commits first makes the write
// retry and the cut is made again against the new current version.
//
// decision: a prune raises no settings generation. RFC 13 §5 and RFC 16's
// CFGGEN row say every Secret change raises it, but nothing resolves a
// version that is pruned, so no node has a reason to read again. Withdraw
// this if RFC 16 says a prune raises it.
//
// decision: a pruned version is never resolved, but only while no cache of
// opened values serves a version below the current one. Withdraw it if one
// does.
func (c controlTx) PruneSecret(id entity.SecretRef, below uint64) error {
	if err := c.tx.GuardRange(secretPrefix(id)); err != nil {
		return err
	}

	cur, ok, err := secretIn(c.tx, id, 0)
	if err != nil {
		return err
	}

	if !ok {
		return nil
	}

	below = min(below, cur.Version)

	// The keys are collected first: a key is not deleted during the scan
	// that yields it.
	var doomed [][]byte

	prefix := secretPrefix(id)

	for row, err := range c.tx.Scan(prefix, nil) {
		if err != nil {
			return err
		}

		v, err := secretVersion(prefix, row.Key)
		if err != nil {
			return err
		}

		if v >= below {
			break
		}

		doomed = append(doomed, slices.Clone(row.Key))
	}

	for _, key := range doomed {
		if err := c.tx.Delete(key); err != nil {
			return err
		}
	}

	return nil
}
