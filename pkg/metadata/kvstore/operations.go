package kvstore

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// operationLife is how long an ended operation is kept.
const operationLife = 7 * 24 * time.Hour

// PutOperation creates an operation, expect zero, or changes the one whose
// generation expect names, raising its generation.
func (c ControlPlane) PutOperation(ctx context.Context, op entity.Operation, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutOperation(op, expect) })
}

// PutOperation writes op at the transaction's store time. It refuses a kind
// as long as an operation's ID, so a node's refusal of a kind, which names
// the kind where an operation's ID would stand, is told from an operation's
// by its length. A running
// operation holds its kind and target's running index, so a second one there
// is refused with an entity.OperationRunningError naming the one that runs,
// the index read tracked so that of
// two racing starts the second conflicts and then finds it held. The write
// that ends an operation releases the index and writes its expiry row, 7
// days on.
func (c controlTx) PutOperation(op entity.Operation, expect uint64) (uint64, error) {
	if op.ID == (entity.OperationID{}) || op.Kind == "" || op.Target.Kind.String() == "" || op.State.String() == "" {
		return 0, merrs.NewInvalidArgumentError("an operation needs an ID, a kind, a target of a known kind and a known state")
	}
	if len(op.Kind) == len(op.ID) {
		return 0, merrs.NewInvalidArgumentError("an operation's kind is not as long as an operation's ID")
	}
	key := operationKey(op.ID)
	b, existed, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	var old entity.Operation
	if existed {
		if old, err = decodeOperation(op.ID, b); err != nil {
			return 0, err
		}
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	op.Started, op.Updated = c.tx.Now(), c.tx.Now()
	if existed {
		if old.State.Ended() {
			return 0, merrs.NewInvalidArgumentError("an ended operation does not change")
		}
		if old.Kind != op.Kind || old.Target.Kind != op.Target.Kind || !bytes.Equal(old.Target.ID, op.Target.ID) {
			return 0, merrs.NewInvalidArgumentError("an operation's kind and target never change")
		}
		op.Started = old.Started
	}
	running := runningKey(op.Kind, op.Target)
	switch {
	case op.State.Ended():
		if existed {
			if err := c.tx.Delete(running); err != nil {
				return 0, err
			}
		}
		if err := c.tx.Set(operationExpiryKey(op), nil); err != nil {
			return 0, err
		}
	case !existed:
		if b, held, err := get(c.tx, running); err != nil {
			return 0, err
		} else if held {
			id, err := decodeID(b)
			if err != nil {
				return 0, err
			}
			return 0, &entity.OperationRunningError{ID: entity.OperationID(id)}
		}
		if err := c.tx.Set(running, encodeID(op.ID)); err != nil {
			return 0, err
		}
	}
	op.Generation = old.Generation + 1
	return op.Generation, c.tx.Set(key, encodeOperation(op))
}

// Operation reads one operation; false when none has the ID, or it ended
// and expired.
func (c ControlPlane) Operation(ctx context.Context, id entity.OperationID) (op entity.Operation, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		op, found, err = operationIn(r, id)
		return err
	})
	return op, found, err
}

func operationIn(r kv.Reader, id entity.OperationID) (entity.Operation, bool, error) {
	b, ok, err := get(r, operationKey(id))
	if err != nil || !ok {
		return entity.Operation{}, false, err
	}
	op, err := decodeOperation(id, b)
	return op, err == nil, err
}

// Operations lists every operation in ID order, resuming after the ID
// after; the zero ID starts from the first.
func (c ControlPlane) Operations(ctx context.Context, after entity.OperationID) iter.Seq2[entity.Operation, error] {
	var cursor []byte
	if after != (entity.OperationID{}) {
		cursor = operationKey(after)
	}
	prefix := []byte{keys.KindOP}
	return pages(ctx, c.s.kv, prefix, cursor, func(_ kv.Reader, row kv.KeyValue) (entity.Operation, error) {
		return decodeOperation(entity.OperationID(row.Key[len(prefix):]), row.Value)
	})
}

// RunningOperation is the operation of kind running on target; false when
// none runs.
func (c ControlPlane) RunningOperation(ctx context.Context, kind string, target entity.ResourceRef) (op entity.Operation, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		op, found, err = runningIn(r, runningKey(kind, target))
		return err
	})
	return op, found, err
}

// RunningOperation reads the running index tracked, so a start of kind on
// target committed after the read makes the transaction retry.
func (c controlTx) RunningOperation(kind string, target entity.ResourceRef) (entity.Operation, bool, error) {
	return runningIn(c.tx, runningKey(kind, target))
}

// RunningOperations lists every operation not yet ended, through the running
// index, in the order of its kind and target, resuming after the operation
// after; the zero operation starts from the first.
func (c ControlPlane) RunningOperations(ctx context.Context, after entity.Operation) iter.Seq2[entity.Operation, error] {
	var cursor []byte
	if after.Kind != "" {
		cursor = runningKey(after.Kind, after.Target)
	}
	return pages(ctx, c.s.kv, []byte{keys.KindOPR}, cursor, func(r kv.Reader, row kv.KeyValue) (entity.Operation, error) {
		return indexedIn(r, row.Value)
	})
}

// runningIn reads the operation the running index row at key names; false
// when there is no row.
func runningIn(r kv.Reader, key []byte) (entity.Operation, bool, error) {
	b, ok, err := get(r, key)
	if err != nil || !ok {
		return entity.Operation{}, false, err
	}
	op, err := indexedIn(r, b)
	return op, err == nil, err
}

// indexedIn reads the operation a running index row's value names.
func indexedIn(r kv.Reader, b []byte) (entity.Operation, error) {
	id, err := decodeID(b)
	if err != nil {
		return entity.Operation{}, err
	}
	op, found, err := operationIn(r, entity.OperationID(id))
	if err == nil && !found {
		err = errors.New("kvstore: running-operation index names no operation")
	}
	return op, err
}

func operationKey(id entity.OperationID) []byte { return keys.ID([]byte{keys.KindOP}, id[:]) }

func runningKey(kind string, target entity.ResourceRef) []byte {
	return appendTarget(keys.String([]byte{keys.KindOPR}, []byte(kind)), target)
}

// operationExpiryKey is an ended operation's row in the expiry index: 7 days
// after its end, then its tag and ID.
func operationExpiryKey(op entity.Operation) []byte {
	return expiryKey(op.Updated.Add(operationLife), keys.XPOperation, op.ID[:])
}

func encodeOperation(op entity.Operation) []byte {
	w := putResource(codec.NewWriter(v1).String(op.Kind), op.Target)
	putPrincipal(w, op.Account).Uint8(uint8(op.State)).Fixed(op.Claim.Node[:]).Uint64(op.Claim.Epoch)
	putTime(w, op.Claim.Expires).Bytes(op.Step).Bytes(op.Params)
	putTime(putTime(w, op.Started), op.Updated).Bytes(op.Error).Bytes(op.Result).Uint64(op.Generation)
	return w.Encode()
}

func decodeOperation(id entity.OperationID, b []byte) (entity.Operation, error) {
	r, err := open(b)
	if err != nil {
		return entity.Operation{}, err
	}
	known := true
	op := entity.Operation{ID: id, Kind: r.String(), Target: readResource(r, &known)}
	op.Account, op.State = readPrincipal(r), entity.OpState(r.Uint8())
	copy(op.Claim.Node[:], r.Fixed(16))
	op.Claim.Epoch, op.Claim.Expires = r.Uint64(), readTime(r)
	op.Step, op.Params = cloneOrNil(r.Bytes()), cloneOrNil(r.Bytes())
	op.Started, op.Updated = readTime(r), readTime(r)
	op.Error, op.Result, op.Generation = cloneOrNil(r.Bytes()), cloneOrNil(r.Bytes()), r.Uint64()
	if err := r.Done(); err != nil {
		return op, err
	}
	if !known || op.State.String() == "" {
		return op, errMalformed
	}
	return op, nil
}
