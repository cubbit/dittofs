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
// one with snapshots when the call neither deletes nor detaches them, when it
// deletes them one any use record still holds, and one whose file is the
// source of a clone not done onto another share's file. Snapshot and use
// records and durable opens are found under a range guard, and the re-home
// record by a tracked read, so a cut, a re-home or a use record that starts
// concurrently conflicts with the mark in either commit order. A clone's
// phase 1 reads the source share's state with tracking, so a clone
// committing after the mark conflicts with it or answers stale.
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
	if err := noForeignCloneHold(c.tx, share); err != nil {
		return err
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

// noForeignCloneHold refuses a share one of whose files is the source of a
// clone not done onto another share's file, as its durable opens show.
//
// decision: the mark is refused rather than the removal keeping the source's
// refs until the clone is done, since step 4 would otherwise retire blocks
// the clone has yet to adopt. A clone ends on its own, so the caller retries.
// Keep the refs instead if a long clone blocks removals in practice.
//
// ponytail: the mark reads every durable open of the share in its one
// transaction. Keep a share's clone holds in an index of their own once its
// durable opens approach the key budget.
func noForeignCloneHold(tx kv.Txn, share entity.ShareID) error {
	prefix := shareKey(share, keys.SDo)
	if err := tx.GuardRange(prefix); err != nil {
		return err
	}
	for row, err := range tx.Scan(prefix, nil) {
		if err != nil {
			return err
		}
		_, rest, err := keys.ReadUint64(row.Key[len(prefix):])
		if err != nil {
			return err
		}
		file, rest, err := keys.ReadID(rest, keys.IDLen)
		if err != nil {
			return err
		}
		id, _, err := keys.ReadUint64(rest)
		if err != nil {
			return err
		}
		b, ok, err := get(tx, openKey(share, entity.FileID(file), entity.OpenID(id)))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		op, err := decodeOpen(entity.FileID(file), entity.OpenID(id), b)
		if err != nil || op.Kind != entity.OpenCloneSource {
			if err != nil {
				return err
			}
			continue
		}
		if _, here, err := get(tx, fileKey(share, op.CloneDst)); err != nil {
			return err
		} else if !here {
			return merrs.NewConflictError("delete share", "a clone onto another share reads from it; retry once the clone is done")
		}
	}
	return nil
}

// ErrRemovalOrder is a shard's removal step asked for before it is due: its
// offload or forget recorded before ContinueRemoval has answered 3 or 4, or
// its files' drop before it has forgotten the tag. The driver retries later.
var ErrRemovalOrder = errors.New("kvstore: a share removal's step is not due yet")

// ContinueRemoval runs one bounded batch of a share's removal from where its
// state record stands, and reports the step of RFC 16 §2.3.2 it stands at
// after. A driver calls it first and acts on its answer: at 3 it ends each
// shard's live state and records that with RecordShardRemoval, and at 4 it
// forgets the tag on each shard, records that, and drops the shard's files
// with ContinueShardRemoval. Neither step is passed until every shard's rm row
// has reached it. done is the removal finished; asked again, it is done.
func (n Nodes) ContinueRemoval(ctx context.Context, share entity.ShareID) (step uint8, done bool, err error) {
	if err := n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		step, done, err = n.s.removalBatch(tx, share)
		return err
	}); err != nil {
		return 0, false, err
	}
	return step, done, nil
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
		// Delete or detach the share's snapshots here when they do, and make
		// ContinueShardRemoval keep the refs a detached snapshot reads rather
		// than drop them all.
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
// of one removal serialise on it: ok is false once the removal has ended,
// and a share not being removed is refused.
func removingState(tx kv.Txn, share entity.ShareID) (shareState, bool, error) {
	b, ok, err := get(tx, shareKey(share, keys.SSt))
	if err != nil || !ok {
		return shareState{}, false, err
	}
	st, err := decodeState(b)
	if err != nil {
		return shareState{}, false, err
	}
	if st.State == entity.ShareRetired {
		return shareState{}, false, nil
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
		got, err := shardStepIn(tx, shard)
		return got < step, err
	}
}

// shardStepIn reads a shard's rm row with tracking; zero for none.
func shardStepIn(r kv.Reader, shard uint64) (removalStep, error) {
	b, ok, err := get(r, shardRemovalKey(shard))
	if err != nil || !ok {
		return 0, err
	}
	return decodeRemovalStep(b)
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
	info, err := shareIn(tx, share)
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
// its rows, the share's info and its namespaces with its keys in their share
// indexes, retiring its state record and raising ShareList. The journal tag
// is never given again: its counter stays where it is.
//
// decision: the state record stays, retired, so a PutShare naming the
// removed ShareID is refused: ShareIDs come from the caller, and nothing else
// remembers which were given. It costs a key per removed share, which every
// walk over the shares reads. Delete it here once the store mints ShareIDs.
//
// ponytail: step 7, the drain of the namespaces nsg names, is not run, and no
// drain mark is written: nothing reads one until GC exists. GC's first start
// finds a namespace this left undrained by one scan of the NS records
// against every share's nsg.
func removeLast(tx kv.Txn, share entity.ShareID) error {
	info, err := shareIn(tx, share)
	if err != nil {
		return err
	}
	if err := deleteUnder(tx, shardKey(uint64(info.Shard))); err != nil {
		return err
	}
	if err := putShareNamespaces(tx, share, shareNamespaces{Namespaces: info.Namespaces, Write: info.Write}, shareNamespaces{}); err != nil {
		return err
	}
	if err := deleteKeys(tx, keys.Uint64(shardsPrefix(share), uint64(info.Shard)), shareKey(share, keys.SInfo),
		namespacesKey(share)); err != nil {
		return err
	}
	if err := tx.Set(shareKey(share, keys.SSt), encodeState(shareState{State: entity.ShareRetired})); err != nil {
		return err
	}
	_, err = raise(tx, shareListKey)
	return err
}

// RecordShardRemoval records in shard's rm row how far a share's removal has
// gone on it, as the shard's primary reports: offloaded, once its live state
// has ended, or forgotten, once it has forgotten the share's tag. Offloaded is
// refused with ErrRemovalOrder until ContinueRemoval has answered 3, and
// forgotten until it has answered 4, which it does only once every shard is
// offloaded. A step recorded already, or a share whose removal has finished,
// changes nothing.
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
			return ErrRemovalOrder
		}
		if err := shardOfShare(tx, share, uint64(shard)); err != nil {
			return err
		}
		if cur, err := shardStepIn(tx, uint64(shard)); err != nil || cur >= step {
			return err
		}
		return tx.Set(shardRemovalKey(uint64(shard)), encodeRemovalStep(step))
	})
}

// shardOfShare refuses a shard not in the share's shard list.
func shardOfShare(r kv.Reader, share entity.ShareID, shard uint64) error {
	if _, ok, err := get(r, keys.Uint64(shardsPrefix(share), shard)); err != nil || ok {
		return err
	}
	return merrs.NewNotFoundError(fmt.Sprint(shard), "shard of the share")
}

// ContinueShardRemoval runs one bounded batch of step 4 on shard, one of the
// share's: it drops the share's files in key order, each ref through the ref
// accounting every removal uses, and every other row of a file as it goes.
// done is every file dropped and the shard's rm row done; a shard whose
// record, or whose share's removal, is gone answers done. It is refused with
// ErrRemovalOrder until the shard has forgotten the tag.
//
// decision: no holder is checked, no fence written and no journal version
// taken, and every ref is dropped as at one above the share's version floor:
// the share has served nothing since step 3, so no link, open or pending
// release holds a file, and no write follows. Withdraw it if a file of a share
// past step 3 can be reached.
//
// ponytail: the share's files are the shard's, as a share has one shard. Drop
// only the files whose Shard names it once a share can have more.
//
// A clone not done onto a file here gives up its hold on its source as the
// drop reaches the clone's removal record, so the source's share can be
// removed in turn.
//
// ponytail: each batch scans from the share's first file key, since every
// row before it is gone; on a store whose scan steps over deleted keys until
// compaction, a share's drop reads O(n²) of them. Keep the last key in the rm
// row if a large share's drop shows in a profile.
func (n Nodes) ContinueShardRemoval(ctx context.Context, share entity.ShareID, shard entity.ShardID) (done bool, err error) {
	if err := n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		done, err = n.s.shardDropBatch(tx, share, uint64(shard))
		return err
	}); err != nil {
		return false, err
	}
	return done, nil
}

func (s *Store) shardDropBatch(tx kv.Txn, share entity.ShareID, shard uint64) (bool, error) {
	if _, ok, err := removingState(tx, share); err != nil || !ok {
		return err == nil, err
	}
	if _, ok, err := get(tx, shardKey(shard)); err != nil || !ok {
		return err == nil, err
	}
	if err := shardOfShare(tx, share, shard); err != nil {
		return false, err
	}
	switch step, err := shardStepIn(tx, shard); {
	case err != nil:
		return false, err
	case step == stepDone:
		return true, nil
	case step < stepForgotten:
		return false, ErrRemovalOrder
	}
	nss, err := shareNamespacesOf(tx, share)
	if err != nil {
		return false, err
	}
	deltas := map[uint32]counts{}
	files := keys.ID([]byte{keys.KindF}, share[:])
	_, finished, err := walk(tx, files, nil, max(1, (s.k-1)/batchItemKeys), func(row kv.KeyValue) (bool, error) {
		rest := row.Key[len(files):]
		if len(rest) > keys.IDLen && rest[keys.IDLen] == keys.FRm {
			if err := dropCloneHoldOf(tx, entity.FileID(rest[:keys.IDLen]), rest[keys.IDLen+1:], row.Value); err != nil {
				return false, err
			}
		}
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
	if err := applyDeltas(tx, nss, deltas); err != nil || !finished {
		return false, err
	}
	return true, tx.Set(shardRemovalKey(shard), encodeRemovalStep(stepDone))
}

// dropCloneHoldOf drops the hold on its source of the clone not done whose
// removal record of dst is at the key suffix v, if it is one. A source
// already gone holds nothing.
func dropCloneHoldOf(tx kv.Txn, dst entity.FileID, v, b []byte) error {
	version, _, err := keys.ReadVersion(v)
	if err != nil {
		return fmt.Errorf("%w: removal key: %v", errMalformed, err)
	}
	m, err := decodeRemoval(version, b)
	if err != nil || m.Kind != entity.RemoveClone || m.Done {
		return err
	}
	if err := dropCloneHold(tx, dst, m); err != nil && !errors.Is(err, entity.ErrStale) {
		return err
	}
	return nil
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

func deleteKeys(tx kv.Txn, ks ...[]byte) error {
	for _, k := range ks {
		if err := tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
