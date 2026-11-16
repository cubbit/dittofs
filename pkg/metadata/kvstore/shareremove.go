package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

var (
	// ErrRehoming is a share whose namespace a re-home is moving.
	ErrRehoming = errors.New("kvstore: share is being re-homed")
	// ErrMoving is a share one of whose namespaces a move has paused.
	ErrMoving = errors.New("kvstore: a namespace of the share is paused by a move")
)

// The passes of a share's removal, in order, as its state record's Step
// names them, with the key the pass resumes after in its Cursor: a crash
// resumes the removal at the pass and the key the record holds. Each pass
// belongs to one step of RFC 16 §2.3.2 (removalSteps).
const (
	stepMarked   uint8 = iota + 1 // being removed; name and path free
	stepLive                      // every shard's primary ends its live state
	stepForget                    // every shard forgets the tag and drops its files
	stepGrants                    // the share's grants with their GR keys
	stepSettings                  // its share-scoped settings
	stepClients                   // the client rows naming its shards
	stepRecords                   // its S‖id records, but the four the last pass deletes
	stepShards                    // its shards' records, but the root's
	stepLast                      // the root's shard, info, nsg and st
)

// removalSteps is the step of RFC 16 §2.3.2 each pass belongs to, which
// ContinueRemoval reports.
var removalSteps = [...]uint8{
	stepMarked: 1, stepLive: 3, stepForget: 4, stepGrants: 5, stepSettings: 5, stepClients: 5,
	stepRecords: 6, stepShards: 6, stepLast: 6,
}

// DeleteShare starts the removal of the share whose generation expect
// names, and returns once the share is being removed: its name and path are free for a new share, ShareList moves, and
// the control plane's calls into it and Root answer stale. A handle into it
// still resolves here: the filesystem service gates every call on the share
// state, with the grant (see resolveIn). Removal is one-way; a second call
// is stale.
//
// The mark refuses a share a cut is freezing, one with a re-home running, one
// any of whose namespaces a move has paused,
// one with snapshots when the call neither deletes nor detaches them, and,
// when it deletes them, one any use record still holds. Snapshot and use
// records are found under a range guard, and the re-home record by a tracked
// read, so a cut, a re-home or a use record that starts concurrently
// conflicts with the mark in either commit order: each of those reads the
// state the mark writes.
//
// The rest of the removal runs in the background: ContinueRemoval's batches,
// RecordShardRemoval for step 3 and ContinueShardRemoval for step 4.
func (c ControlPlane) DeleteShare(ctx context.Context, share entity.ShareID, snapshots entity.SnapshotDisposition, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteShare(share, snapshots, expect) })
}

func (c controlTx) DeleteShare(share entity.ShareID, snapshots entity.SnapshotDisposition, expect uint64) error {
	if snapshots.String() == "" {
		return merrs.NewInvalidArgumentError("unknown snapshot disposition")
	}
	stKey := shareKey(share, keys.SSt)
	b, ok, err := get(c.tx, stKey)
	if err != nil {
		return err
	}
	if !ok {
		return merrs.NewNotFoundError(share.String(), "share")
	}
	st, err := decodeState(b)
	if err != nil {
		return err
	}
	// ponytail: a running cut does not refuse the removal with ErrDelay,
	// because no cut is recorded yet. Read the share's cut record here
	// when snapshots land.
	if st.State == entity.ShareRemoving || st.State == entity.ShareRetired {
		return entity.ErrStale
	}
	if err := noPausedNamespace(c.tx, share); err != nil {
		return err
	}
	if _, ok, err := get(c.tx, shareKey(share, keys.SRh)); err != nil {
		return err
	} else if ok {
		return ErrRehoming
	}
	if snapshots == entity.SnapshotsRefuse {
		if any, err := guardedAny(c.tx, shareKey(share, keys.SSnap)); err != nil || any {
			if err == nil {
				err = merrs.NewConflictError("delete share", "the share has snapshots; delete or detach them")
			}
			return err
		}
	}
	if snapshots == entity.SnapshotsDelete {
		if any, err := guardedAny(c.tx, shareKey(share, keys.SUse)); err != nil || any {
			if err == nil {
				err = merrs.NewConflictError("delete share", "a backup, clone, restore, copy or move holds a snapshot")
			}
			return err
		}
	}
	ib, ok, err := get(c.tx, shareKey(share, keys.SInfo))
	if err != nil {
		return err
	}
	if !ok {
		return merrs.NewNotFoundError(share.String(), "share")
	}
	info, err := decodeShareInfo(share, ib)
	if err != nil {
		return err
	}
	if err := expected(info.Generation, expect); err != nil {
		return err
	}
	if err := c.tx.Set(stKey, encodeState(shareState{State: entity.ShareRemoving, Step: stepMarked, Snapshots: snapshots})); err != nil {
		return err
	}
	if err := c.tx.Delete(nameKey(keys.NXShare, info.Name)); err != nil {
		return err
	}
	if err := c.tx.Delete(pathKey(info.Path)); err != nil {
		return err
	}
	_, err = raise(c.tx, shareListKey)
	return err
}

// guardedAny reports whether any record lies under prefix, guarding the
// range so a record added after this snapshot aborts the caller.
func guardedAny(tx kv.Txn, prefix []byte) (bool, error) {
	if err := tx.GuardRange(prefix); err != nil {
		return false, err
	}
	for _, err := range tx.Scan(prefix, nil) {
		return err == nil, err
	}
	return false, nil
}

// noPausedNamespace refuses a share any of whose namespaces a move has
// paused, reading the share's namespaces and each pause with tracking.
func noPausedNamespace(tx kv.Txn, share entity.ShareID) error {
	b, ok, err := get(tx, namespacesKey(share))
	if err != nil || !ok {
		return err
	}
	nss, err := decodeNamespaces(b)
	if err != nil {
		return err
	}
	for _, g := range nss.Namespaces {
		if _, paused, err := get(tx, gcPauseKey(g.Namespace)); err != nil {
			return err
		} else if paused {
			return ErrMoving
		}
	}
	return nil
}

// errRemovalOrder is a shard's removal step asked for before the step it
// follows: a shard's forget before every shard is offloaded, or its files'
// drop before it has forgotten the tag.
var errRemovalOrder = errors.New("kvstore: a share removal's step is not due yet")

// ContinueRemoval runs one bounded batch of a share's removal, from the pass
// and cursor its state record holds, and reports the step of RFC 16 §2.3.2
// the removal stands at after it. At step 3 the node ends each shard's live
// state and records it with RecordShardRemoval; at step 4 it forgets the tag
// on each shard, records that, and drops the shard's files with
// ContinueShardRemoval. Neither moves on until every shard has: each batch of
// those two steps reads its shards and their rm rows with tracking and
// advances its cursor past the shards found there, and only the batch that
// finds the last one moves the record to the next step. done is the removal
// finished, its state record gone; a share with no state record answers done,
// so an operation whose last batch committed before a crash ends.
func (n Nodes) ContinueRemoval(ctx context.Context, share entity.ShareID) (step uint8, done bool, err error) {
	err = n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		step, done, err = n.s.removalBatch(tx, share)
		return err
	})
	return step, done, err
}

func (s *Store) removalBatch(tx kv.Txn, share entity.ShareID) (step uint8, done bool, err error) {
	st, ok, err := removingState(tx, share)
	if err != nil || !ok {
		return 0, err == nil, err
	}
	n, finished := max(1, (s.k-2)/batchItemKeys), false
	switch st.Step {
	case stepMarked:
		// ponytail: step 2 has nothing to do, since no snapshot record exists
		// until RFC 12's snapshots land, so a detach ends as a delete does.
		// Delete or detach the share's snapshots here when they do.
		finished = true
	case stepLive:
		st.Cursor, finished, err = walk(tx, shardsPrefix(share), st.Cursor, n, shardReached(tx, share, stepOffloaded))
	case stepForget:
		st.Cursor, finished, err = walk(tx, shardsPrefix(share), st.Cursor, n, shardReached(tx, share, stepDone))
	case stepGrants:
		st.Cursor, finished, err = walk(tx, shareKey(share, keys.SG), st.Cursor, n, func(row kv.KeyValue) (bool, error) {
			p, _, err := keys.ReadID(row.Key[len(shareKey(share, keys.SG)):], keys.IDLen)
			if err != nil {
				return false, err
			}
			return false, deleteKeys(tx, row.Key, grantRevKey(entity.PrincipalID(p), share))
		})
	case stepSettings:
		// ponytail: no secret is deleted with the settings: this store holds
		// no secret record yet, and a setting's value is opaque to it. Delete
		// each secret only these settings reference, in the batch that deletes
		// the last of them, once the secrets view lands.
		dropped := false
		st.Cursor, finished, err = walk(tx, scopePrefix(shareScope(share)), st.Cursor, n, func(row kv.KeyValue) (bool, error) {
			dropped = true
			return false, tx.Delete(row.Key)
		})
		if err == nil && dropped {
			_, err = raise(tx, cfgGenKey)
		}
	case stepClients:
		// ponytail: no move record is deleted, as nothing writes one yet.
		// Delete each MV record naming one of the share's shards in this
		// step once moves between shards land.
		st.Cursor, finished, err = dropClientRows(tx, share, st.Cursor, n)
	case stepRecords:
		kept := [][]byte{shareKey(share, keys.SSt), shareKey(share, keys.SInfo), namespacesKey(share)}
		st.Cursor, finished, err = walk(tx, sharePrefix(share), st.Cursor, n, func(row kv.KeyValue) (bool, error) {
			if bytes.HasPrefix(row.Key, shardsPrefix(share)) || slices.ContainsFunc(kept, func(k []byte) bool { return bytes.Equal(k, row.Key) }) {
				return false, nil
			}
			return false, tx.Delete(row.Key)
		})
	case stepShards:
		st.Cursor, finished, err = dropShards(tx, share, st.Cursor, n)
	case stepLast:
		if err := removeLast(tx, share); err != nil {
			return 0, false, err
		}
		return removalSteps[stepLast], true, nil
	default:
		return 0, false, fmt.Errorf("%w: share removal at pass %d", errMalformed, st.Step)
	}
	if err != nil {
		return 0, false, err
	}
	if finished {
		st.Step, st.Cursor = st.Step+1, nil
	}
	return removalSteps[st.Step], false, tx.Set(shareKey(share, keys.SSt), encodeState(st))
}

// removingState reads a share's state record with tracking, so two batches
// of one removal serialise on it: ok is false once the removal has deleted
// it, and a share not being removed is refused.
func removingState(tx kv.Txn, share entity.ShareID) (shareState, bool, error) {
	b, ok, err := get(tx, shareKey(share, keys.SSt))
	if err != nil || !ok {
		return shareState{}, false, err
	}
	st, err := decodeState(b)
	if err != nil {
		return shareState{}, false, err
	}
	if st.State != entity.ShareRemoving {
		return shareState{}, false, merrs.NewInvalidArgumentError("the share is not being removed")
	}
	return st, true, nil
}

// shardReached holds the walk over a share's shard list at the first shard
// whose rm row is below step, reading each shard's record and rm row with
// tracking.
func shardReached(tx kv.Txn, share entity.ShareID, step removalStep) func(kv.KeyValue) (bool, error) {
	return func(row kv.KeyValue) (bool, error) {
		shard, err := shardOfRow(share, row.Key)
		if err != nil {
			return false, err
		}
		if _, _, err := get(tx, shardKey(shard)); err != nil {
			return false, err
		}
		b, ok, err := get(tx, shardRemovalKey(shard))
		if err != nil || !ok {
			return true, err
		}
		got, err := decodeRemovalStep(b)
		return got < step, err
	}
}

// dropClientRows deletes, in one batch, the shard marks of every client
// that name one of the share's shards, walking every client's keys.
//
// ponytail: the walk reads every client record and mark, and the share's
// whole shard list each batch, since a mark's key puts the shard last; as
// ByShard does. Walk a shard-first client index instead once one exists.
func dropClientRows(tx kv.Txn, share entity.ShareID, after []byte, n int) ([]byte, bool, error) {
	shards := map[string]bool{}
	for row, err := range tx.Scan(shardsPrefix(share), nil) {
		if err != nil {
			return nil, false, err
		}
		shards[string(append([]byte{keys.CLSh}, row.Key[len(shardsPrefix(share)):]...))] = true
	}
	return walk(tx, []byte{keys.KindCL}, after, n, func(row kv.KeyValue) (bool, error) {
		if _, rest, err := keys.ReadUint64(row.Key[1:]); err != nil || !shards[string(rest)] {
			return false, err
		}
		return false, tx.Delete(row.Key)
	})
}

// dropShards deletes, in one batch, the records and rows of each of the
// share's shards but its root's, which the last pass deletes with the state
// record, so no crash leaves shard rows with no record of a removal due.
//
// ponytail: a shard's rows are deleted in the batch that reaches it, however
// many request records it holds; a share has one shard, which keeps none.
// Drop a shard's rows in batches of their own once shards hold request
// records.
func dropShards(tx kv.Txn, share entity.ShareID, after []byte, n int) ([]byte, bool, error) {
	info, err := shareInfoIn(tx, share)
	if err != nil {
		return nil, false, err
	}
	return walk(tx, shardsPrefix(share), after, n, func(row kv.KeyValue) (bool, error) {
		shard, err := shardOfRow(share, row.Key)
		if err != nil || shard == uint64(info.Shard) {
			return false, err
		}
		if err := deleteUnder(tx, shardKey(shard)); err != nil {
			return false, err
		}
		return false, tx.Delete(row.Key)
	})
}

// removeLast is the removal's last transaction: the root's shard record with
// its rows, the share's info, namespaces and state record, raising
// ShareList. The ShareID and the journal tag are never given again: their
// counters stay where they are.
//
// ponytail: step 7, the drain of the namespaces nsg names, is not run, and no
// drain mark is written: nothing reads one until GC exists. GC's first start
// finds a namespace this left undrained by one scan of the NS records
// against every share's nsg.
func removeLast(tx kv.Txn, share entity.ShareID) error {
	info, err := shareInfoIn(tx, share)
	if err != nil {
		return err
	}
	if err := deleteUnder(tx, shardKey(uint64(info.Shard))); err != nil {
		return err
	}
	if err := dropNamespaces(tx, share); err != nil {
		return err
	}
	if err := deleteKeys(tx, keys.Uint64(shardsPrefix(share), uint64(info.Shard)), shareKey(share, keys.SInfo),
		shareKey(share, keys.SSt)); err != nil {
		return err
	}
	_, err = raise(tx, shareListKey)
	return err
}

// dropNamespaces deletes the share's namespace list, nsg.
func dropNamespaces(tx kv.Txn, share entity.ShareID) error {
	return tx.Delete(namespacesKey(share))
}

// RecordShardRemoval records in shard's rm row how far a share's removal has
// gone on it, as the shard's primary reports: offloaded, once its live state
// has ended, or forgotten, once it has forgotten the share's tag. A shard
// forgets only after every shard is offloaded, so forgotten is refused until
// ContinueRemoval has found them all so. A step recorded already, or a share
// whose removal has finished, changes nothing.
func (n Nodes) RecordShardRemoval(ctx context.Context, share entity.ShareID, shard entity.ShardID, reached entity.TagState) error {
	step := map[entity.TagState]removalStep{entity.TagOffloaded: stepOffloaded, entity.TagForgotten: stepForgotten}[reached]
	if step == 0 {
		return merrs.NewInvalidArgumentError("a shard's removal records offloaded or forgotten")
	}
	return n.s.kv.Update(ctx, func(tx kv.Txn) error {
		st, ok, err := removingState(tx, share)
		if err != nil || !ok {
			return err
		}
		if st.Step < stepLive || step == stepForgotten && st.Step < stepForget {
			return errRemovalOrder
		}
		if _, ok, err := get(tx, keys.Uint64(shardsPrefix(share), uint64(shard))); err != nil {
			return err
		} else if !ok {
			return merrs.NewNotFoundError(fmt.Sprint(shard), "shard of the share")
		}
		key := shardRemovalKey(uint64(shard))
		var cur removalStep
		if b, ok, err := get(tx, key); err != nil {
			return err
		} else if ok {
			if cur, err = decodeRemovalStep(b); err != nil {
				return err
			}
		}
		switch {
		case cur >= step:
			return nil
		case step == stepForgotten && cur < stepOffloaded:
			return errRemovalOrder
		}
		return tx.Set(key, encodeShardStep(step, nil))
	})
}

// ContinueShardRemoval runs one bounded batch of step 4 on shard: it drops
// the share's files in key order from the cursor its rm row holds, a ref
// through the ref accounting every removal uses — its chunk's count falls,
// its reverse key goes, a block whose live count reaches zero is retired —
// and every other row of a file deleted as it is. The batch is a metadata
// transaction alone: the tag is forgotten, so it takes no journal version.
// done is the shard's files all dropped, its rm row done; a shard whose
// record or whose share's state record is gone answers done.
//
// No holder is checked and no fence written: the share serves nothing since
// step 3, so a link, an open or a pending release holds nothing. Every ref
// is dropped, as a removal at one above the share's version floor would drop
// them, since no write follows.
//
// ponytail: the share's files are the shard's, as a share has one shard. Drop
// only the files whose Shard names it once a share can have more.
//
// ponytail: a clone not done onto a file here keeps its hold on a source in
// another share; none can exist while every share has a namespace of its own,
// and a clone never crosses namespaces. Drop that hold with the clone's
// removal record here once shares share one.
func (n Nodes) ContinueShardRemoval(ctx context.Context, share entity.ShareID, shard entity.ShardID) (done bool, err error) {
	err = n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		done, err = n.s.shardDropBatch(tx, share, uint64(shard))
		return err
	})
	return done, err
}

func (s *Store) shardDropBatch(tx kv.Txn, share entity.ShareID, shard uint64) (bool, error) {
	if _, ok, err := removingState(tx, share); err != nil || !ok {
		return err == nil, err
	}
	if _, ok, err := get(tx, shardKey(shard)); err != nil || !ok {
		return err == nil, err
	}
	key := shardRemovalKey(shard)
	b, ok, err := get(tx, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errRemovalOrder
	}
	step, cursor, err := decodeShardStep(b)
	switch {
	case err != nil:
		return false, err
	case step == stepDone:
		return true, nil
	case step < stepForgotten:
		return false, errRemovalOrder
	}
	nss, err := shareNamespacesOf(tx, share)
	if err != nil {
		return false, err
	}
	deltas := map[uint32]counts{}
	files := keys.ID([]byte{keys.KindF}, share[:])
	cursor, finished, err := walk(tx, files, cursor, max(1, (s.k-1)/batchItemKeys), func(row kv.KeyValue) (bool, error) {
		rest := row.Key[len(files):]
		if len(rest) <= keys.IDLen || rest[keys.IDLen] != keys.FRef {
			return false, tx.Delete(row.Key)
		}
		file := entity.FileID(rest[:keys.IDLen])
		off, err := startOf(refsPrefix(share, file), row.Key)
		if err != nil {
			return false, err
		}
		r, err := decodeRef(file, off, row.Value)
		if err != nil {
			return false, err
		}
		ns, err := nss.namespace(r.NSGen)
		if err != nil {
			return false, err
		}
		if deltas[r.NSGen] == nil {
			deltas[r.NSGen] = counts{}
		}
		return false, dropRef(tx, ns, share, r, deltas[r.NSGen])
	})
	if err != nil {
		return false, err
	}
	if err := applyDeltas(tx, nss, deltas); err != nil {
		return false, err
	}
	if step = stepFour; finished {
		step, cursor = stepDone, nil
	}
	return finished, tx.Set(key, encodeShardStep(step, cursor))
}

// walk visits, in key order, at most n rows under prefix after the key
// after, and returns the key the next batch resumes after and whether no row
// is left. A row visit holds is not passed: the walk stops before it, so the
// next batch reads it again.
func walk(r kv.Reader, prefix, after []byte, n int, visit func(kv.KeyValue) (held bool, err error)) ([]byte, bool, error) {
	var rows []kv.KeyValue
	more := false
	for row, err := range r.Scan(prefix, after) {
		if err != nil {
			return nil, false, err
		}
		if len(rows) == n {
			more = true
			break
		}
		rows = append(rows, kv.KeyValue{Key: bytes.Clone(row.Key), Value: bytes.Clone(row.Value)})
	}
	for _, row := range rows {
		held, err := visit(row)
		if err != nil || held {
			return after, false, err
		}
		after = row.Key
	}
	return after, !more, nil
}

func sharePrefix(share entity.ShareID) []byte { return keys.ID([]byte{keys.KindS}, share[:]) }

func shareScope(share entity.ShareID) entity.SettingScope {
	return entity.SettingScope{Kind: entity.ScopeShare, ID: share}
}

// shardOfRow is the shard a row of the share's shard list names.
func shardOfRow(share entity.ShareID, key []byte) (uint64, error) {
	shard, rest, err := keys.ReadUint64(key[len(shardsPrefix(share)):])
	if err != nil || len(rest) != 0 {
		return 0, fmt.Errorf("%w: a share's shard row", errMalformed)
	}
	return shard, nil
}

func shareInfoIn(r kv.Reader, share entity.ShareID) (entity.Share, error) {
	b, err := r.Get(shareKey(share, keys.SInfo))
	if err != nil {
		return entity.Share{}, err
	}
	return decodeShareInfo(share, b)
}

func deleteKeys(tx kv.Txn, ks ...[]byte) error {
	for _, k := range ks {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
