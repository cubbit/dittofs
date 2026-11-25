//go:build engine_conformance

package integration

import (
	"context"
	"fmt"
	"sync"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// txnKeys is what the final attempt of one committed transaction wrote, by
// class: an overwrite record (existence), a file record (size), or any key
// that is not an overwrite record (other, a file record included).
type txnKeys struct {
	existence, size, other bool
}

// txnRecord passes every call through to the store's kv.KV and records, for
// each Update that committed, whether its final attempt wrote an existence key
// (an overwrite record, F‖share‖file‖ov), a size key (the file record,
// F‖share‖file), only other keys, or several of them. Earlier attempts the
// backend retried count for nothing. It answers nothing in the store's place:
// it is the pass-through record a test may assert on, beside kv.Counting.
//
// decision: a transaction is classified by its keys' prefixes, from the key
// layout of pkg/metadata/kv/keys; a change of that layout must change
// existenceKey and sizeKey, or every transaction reads as the storm's. Only
// the overwrite record marks an existence commit: a chmod writes the share's
// version floor (S‖share‖vf) too, so that key would count every chmod as the
// engine's. Only the file record marks a size commit: the sync marks of a
// journal (J‖journal‖sm) and the open generation it reads are not sizes.
type txnRecord struct {
	kv.KV

	mu       sync.Mutex
	recorded []txnKeys
}

// recordTxn notes the keys one attempt of a transaction writes.
type recordTxn struct {
	kv.Txn

	seen *txnKeys
}

func (t recordTxn) Set(key, value []byte) error {
	t.note(key)

	return t.Txn.Set(key, value)
}

func (t recordTxn) Delete(key []byte) error {
	t.note(key)

	return t.Txn.Delete(key)
}

func (t recordTxn) note(key []byte) {
	switch {
	case existenceKey(key):
		t.seen.existence = true
	case sizeKey(key):
		t.seen.size, t.seen.other = true, true
	default:
		t.seen.other = true
	}
}

func (r *txnRecord) Update(ctx context.Context, fn func(kv.Txn) error) error {
	var seen txnKeys

	err := r.KV.Update(ctx, func(t kv.Txn) error {
		seen = txnKeys{}

		return fn(recordTxn{Txn: t, seen: &seen})
	})
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.recorded = append(r.recorded, seen)

	return nil
}

// takeTxns returns the transactions committed since the last take or
// takeTxns, in commit order, and resets the record.
func (r *txnRecord) takeTxns() []txnKeys {
	r.mu.Lock()
	defer r.mu.Unlock()

	recorded := r.recorded
	r.recorded = nil

	return recorded
}

// take returns the committed transactions since the last take, those that
// wrote an existence key (mixed ones included) and those among them that also
// wrote other keys, and resets the record.
func (r *txnRecord) take() (engine, mixed int) {
	for _, t := range r.takeTxns() {
		if t.existence {
			engine++
		}

		if t.existence && t.other {
			mixed++
		}
	}

	return engine, mixed
}

// fileRecordLen is the length of F‖share‖file, the file record's key; a key
// under it has a sub-kind byte after.
const fileRecordLen = 1 + 2*keys.IDLen

// existenceKey reports whether key is an overwrite record, which only an
// existence commit of an overwrite writes.
func existenceKey(key []byte) bool {
	return len(key) > fileRecordLen && key[0] == keys.KindF && key[fileRecordLen] == keys.FOv
}

// sizeKey reports whether key is a file record, the key that holds the file's
// committed size, which an existence commit of an append writes.
func sizeKey(key []byte) bool {
	return len(key) == fileRecordLen && key[0] == keys.KindF
}

// appendBurstError checks the transactions committed while the 64 fsyncs of an
// appending burst ran, in a composition whose existence timer cannot come due
// in the window. An fsync of appended content waits for no overwrite, so the
// group commit's mark transaction is at most one, and no transaction may write
// a file record (a size), the existence commit RFC 8 §15.4's B5 forbids on an
// fsync's path.
func appendBurstError(txns []txnKeys) error {
	var sizes, marks int

	for _, t := range txns {
		if t.size {
			sizes++
		} else {
			marks++
		}
	}

	switch {
	case sizes > 0:
		return fmt.Errorf(
			"%d transactions wrote a file record, want none: no existence commit on an fsync path",
			sizes,
		)
	case marks > 1:
		return fmt.Errorf("%d transactions wrote no file record, want at most 1 per journal", marks)
	}

	return nil
}
