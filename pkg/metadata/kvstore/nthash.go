package kvstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// DropNTHashes deletes every version of up to n secrets of kind nt-hash
// whose references sort after after, the zero reference starting from the
// first, and zeroes the NTHash of each user record naming one, raising its
// generation so a put holding the old one is refused. It returns the last
// reference the batch passed, the cursor the next batch starts after, how
// many secrets it deleted, and whether none is left after them.
//
// One batch is one transaction: it raises the principals generation once,
// as the position of every record it zeroes, and the settings generation
// once when it deletes a secret, so every node stops resolving them. A batch
// run again from a cursor whose batch committed finds nothing of it left and
// deletes nothing twice. A secret or a user record that does not decode
// stops the batch with an error Undecodable reports, and every later batch
// stops on it the same way until it is repaired.
//
// ponytail: the scan reads every secret of every kind past the cursor until
// it has n NT hashes, since no index names secrets by kind. Add one when
// secrets of other kinds outnumber NT hashes enough for a batch's reads to
// show in its latency.
func (c ControlPlane) DropNTHashes(
	ctx context.Context,
	after entity.SecretRef,
	n int,
) (next entity.SecretRef, dropped int, done bool, err error) {
	if n < 1 {
		return after, 0, false, merrs.NewInvalidArgumentError("a batch drops at least one NT hash")
	}

	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		var found []foundSecret

		next, found, done, err = ntHashesAfter(tx, after, n)
		if err != nil {
			return err
		}

		var pgen uint64

		for _, f := range found {
			if err := deleteRows(tx, f.keys); err != nil {
				return err
			}

			if err := zeroNTHash(tx, f, &pgen); err != nil {
				return err
			}
		}

		if len(found) > 0 {
			if _, err := raise(tx, cfgGenKey); err != nil {
				return err
			}
		}

		dropped = len(found)

		return nil
	})
	if err != nil {
		return after, 0, false, err
	}

	return next, dropped, done, nil
}

// foundSecret is one secret a batch deletes: its owner and the keys of
// every version it holds.
type foundSecret struct {
	ref   entity.SecretRef
	owner entity.SecretOwner
	keys  [][]byte
}

// ntHashesAfter scans the secrets after after for up to n of kind nt-hash,
// reading each reference's kind from its first version, since every version
// keeps the kind of the first.
func ntHashesAfter(
	tx kv.Txn,
	after entity.SecretRef,
	n int,
) (entity.SecretRef, []foundSecret, bool, error) {
	var start []byte
	if after != (entity.SecretRef{}) {
		start = secretKey(after, math.MaxUint64)
	}

	var (
		found []foundSecret
		cur   entity.SecretRef
		take  bool
	)

	next := after
	prefix := []byte{keys.KindSEC}

	for row, err := range tx.Scan(prefix, start) {
		if err != nil {
			return after, nil, false, err
		}

		id, rest, err := keys.ReadID(row.Key[len(prefix):], keys.IDLen)
		if err != nil || len(rest) != 8 {
			return after, nil, false, fmt.Errorf("%w: secret key %x", errMalformed, row.Key)
		}

		// No secret is held under the zero reference, so the first row's
		// reference always differs from cur's zero value.
		if ref := entity.SecretRef(id); ref != cur {
			if len(found) == n {
				return next, found, false, nil
			}

			s, err := decodeSecret(ref, binary.BigEndian.Uint64(rest), row.Value)
			if err != nil {
				return after, nil, false, err
			}

			cur, next, take = ref, ref, s.Kind == entity.SecretNTHash
			if take {
				found = append(found, foundSecret{ref: ref, owner: s.Owner})
			}
		}

		if take {
			f := &found[len(found)-1]
			f.keys = append(f.keys, slices.Clone(row.Key))
		}
	}

	return next, found, true, nil
}

// zeroNTHash zeroes the NTHash of the user record f's owner names when it
// still names f, read tracked, so a put holding the record's old generation
// conflicts with the batch and is refused on its retry.
func zeroNTHash(tx kv.Txn, f foundSecret, pgen *uint64) error {
	if f.owner.Record.Kind != entity.ResourceUser || len(f.owner.Record.ID) != keys.IDLen {
		return nil
	}

	id := entity.PrincipalID(f.owner.Record.ID)

	b, ok, err := get(tx, userKey(id))
	if err != nil || !ok {
		return err
	}

	u, err := decodeUser(id, b)
	if err != nil {
		return err
	}

	if u.NTHash != f.ref {
		return nil
	}

	u.NTHash, u.Generation = entity.SecretRef{}, nextVersion(tx, u.Generation)
	if u.Position, err = raisePrincipals(tx, pgen); err != nil {
		return err
	}

	return tx.Set(userKey(id), encodeUser(u))
}
