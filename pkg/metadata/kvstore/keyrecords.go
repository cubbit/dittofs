package kvstore

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// KeyRecords is the key provider's view of each namespace's key records,
// NS‖ns‖key‖kind‖keyID, its key lock, NS‖ns‖keyseq‖target, each master
// key's wrap count, MKW‖masterKeyID, and the store index NSR‖store‖ns
// (RFC 16 §4.2). A record's kind and state travel as RFC 5
// Appendix B.2 names them and are stored as bytes; the rest of the record is
// the provider's, stored as given. The state is a byte of its own so a block
// commit can read it without the provider's codec.
type KeyRecords struct{ s *Store }

func (s *Store) KeyRecords() KeyRecords { return KeyRecords{s} }

// keyRow is one key record as the view's callers hold it. It is an alias of
// an unnamed struct, so the view satisfies the provider's own declaration of
// it without either package importing the other.
type keyRow = struct {
	Kind, ID, State string
	Record          []byte
}

// A key record's state byte.
const (
	keyCurrent   byte = 0x01
	keyRetiring  byte = 0x02
	keyRetired   byte = 0x03
	keyDestroyed byte = 0x04
)

// keyRecordKinds and keyRecordStates map RFC 5's names to their stored bytes;
// a record of any other kind or state is refused both ways.
var (
	keyRecordKinds = map[string]byte{
		"chunk-id-key": keys.KeyChunkID,
		"chunking-key": keys.KeyChunking,
		"export-key":   keys.KeyExport,
		"data-key":     keys.KeyData,
	}
	keyRecordStates = map[string]byte{
		"current":   keyCurrent,
		"retiring":  keyRetiring,
		"retired":   keyRetired,
		"destroyed": keyDestroyed,
	}
)

func nameOf(names map[string]byte, b byte) (string, bool) {
	for name, v := range names {
		if v == b {
			return name, true
		}
	}
	return "", false
}

func keyRecordPrefix(ns entity.NamespaceID) []byte {
	return append(keys.ID([]byte{keys.KindNS}, ns[:]), keys.NSKey)
}

// keySeqKey is NS‖ns‖keyseq‖target with the empty target that names ns's
// own keys object.
func keySeqKey(ns entity.NamespaceID) []byte {
	return keys.String(append(keys.ID([]byte{keys.KindNS}, ns[:]), keys.NSKeyseq), nil)
}

func wrapCountKey(masterKeyID []byte) []byte {
	return keys.String([]byte{keys.KindMKW}, masterKeyID)
}

// encodeKeyRecord returns a row's key and value: the value is the version,
// the state's byte, then the record as given.
func encodeKeyRecord(ns entity.NamespaceID, row keyRow) (key, value []byte, err error) {
	kind, ok := keyRecordKinds[row.Kind]
	if !ok {
		return nil, nil, merrs.NewInvalidArgumentError(fmt.Sprintf("key record kind %q", row.Kind))
	}
	state, ok := keyRecordStates[row.State]
	if !ok {
		return nil, nil, merrs.NewInvalidArgumentError(fmt.Sprintf("key record state %q", row.State))
	}
	key = keys.String(append(keyRecordPrefix(ns), kind), []byte(row.ID))
	return key, codec.NewWriter(v1).Uint8(state).Fixed(row.Record).Encode(), nil
}

// decodeKeyRecord reads a row from a key under prefix and its value, copying
// what it keeps.
func decodeKeyRecord(prefix []byte, e kv.KeyValue) (keyRow, error) {
	var row keyRow
	rest := e.Key[len(prefix):]
	if len(rest) == 0 {
		return row, fmt.Errorf("%w: key record key %s", keys.ErrMalformed, keys.Describe(e.Key))
	}
	kind, ok := nameOf(keyRecordKinds, rest[0])
	if !ok {
		return row, fmt.Errorf("%w: key record kind %#x in %s", keys.ErrMalformed, rest[0], keys.Describe(e.Key))
	}
	id, rest, err := keys.ReadString(rest[1:])
	if err == nil && len(rest) > 0 {
		err = keys.ErrMalformed
	}
	if err != nil {
		return row, fmt.Errorf("%w: key record key %s", err, keys.Describe(e.Key))
	}
	r, err := open(e.Value)
	if err != nil {
		return row, fmt.Errorf("%w: key record at %s", err, keys.Describe(e.Key))
	}
	stateByte := r.Uint8()
	record := r.Fixed(len(e.Value) - 2)
	if err := r.Done(); err != nil {
		return row, fmt.Errorf("%w: key record at %s", err, keys.Describe(e.Key))
	}
	state, ok := nameOf(keyRecordStates, stateByte)
	if !ok {
		return row, fmt.Errorf("%w: key record state %#x at %s", errMalformed, stateByte, keys.Describe(e.Key))
	}
	row.Kind, row.ID, row.State = kind, string(id), state
	row.Record = append([]byte(nil), record...)
	return row, nil
}

func loadKeyRecords(r kv.Reader, ns entity.NamespaceID) ([]keyRow, error) {
	prefix := keyRecordPrefix(ns)
	var rows []keyRow
	for e, err := range r.Scan(prefix, nil) {
		if err != nil {
			return nil, err
		}
		row, err := decodeKeyRecord(prefix, e)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Load returns every key record of ns, in no order a caller may rely on.
func (k KeyRecords) Load(ctx context.Context, ns entity.NamespaceID) ([]keyRow, error) {
	var rows []keyRow
	err := k.s.kv.View(ctx, func(r kv.Reader) error {
		var err error
		rows, err = loadKeyRecords(r, ns)
		return err
	})
	return rows, err
}

// Update runs fn on ns's key records, in no order a caller may rely on, and
// on the last sequence recorded for ns's own keys object, NS‖ns‖keyseq‖ with
// an empty target (0 if none was), in one transaction that conflicts with any
// other write of either. It writes the rows fn returns, each replacing the
// record of its kind and ID, and the sequence fn returns when it differs from
// the one fn saw. It deletes nothing. fn may run more than once and must have
// no other effect. A row whose kind or state RFC 5 does not name fails the
// call, and nothing is written.
//
// decision: the conflict over the records comes from a guard over all of
// ns's key records, taken before they are scanned, since a scan is never
// tracked: without it two creators of one namespace's keys each see none,
// write different IDs, and both commit. A guard orders only the writers that
// take it, so every write of NS‖ns‖key‖… goes through Update; a writer that
// bypasses it withdraws the guarantee. The sequence is the key lock: read
// with a tracked Get, so two writers that raise it from one value conflict.
func (k KeyRecords) Update(ctx context.Context, ns entity.NamespaceID, fn func(seq uint64, rows []keyRow) (uint64, []keyRow, error)) error {
	return k.s.kv.Update(ctx, func(tx kv.Txn) error {
		if err := tx.GuardRange(keyRecordPrefix(ns)); err != nil {
			return err
		}
		seqKey := keySeqKey(ns)
		seq, err := counterIn(tx, seqKey)
		if err != nil {
			return err
		}
		rows, err := loadKeyRecords(tx, ns)
		if err != nil {
			return err
		}
		next, out, err := fn(seq, rows)
		if err != nil {
			return err
		}
		for _, row := range out {
			key, value, err := encodeKeyRecord(ns, row)
			if err != nil {
				return err
			}
			if err := tx.Set(key, value); err != nil {
				return err
			}
		}
		if next == seq {
			return nil
		}
		return tx.Set(seqKey, encodeCounter(next))
	})
}

// Namespaces returns every namespace whose namespace record names the remote
// store, from the store index NSR‖store‖ns (RFC 16 §4.2).
//
// decision: key records under no namespace record are not listed; the
// provider's KeyRecords.Namespaces says why that loses nothing.
func (k KeyRecords) Namespaces(ctx context.Context, store [16]byte) ([]entity.NamespaceID, error) {
	prefix := keys.ID([]byte{keys.KindNSR}, store[:])
	var out []entity.NamespaceID
	err := k.s.kv.View(ctx, func(r kv.Reader) error {
		out = out[:0]
		for e, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			if len(e.Key) != len(prefix)+keys.IDLen {
				return fmt.Errorf("%w: store index key %s", keys.ErrMalformed, keys.Describe(e.Key))
			}
			out = append(out, entity.NamespaceID(e.Key[len(prefix):]))
		}
		return nil
	})
	return out, err
}

// RaiseWrapCount adds n to masterKeyID's wrap count if the sum stays at most
// limit, and reports whether it did. The count is read tracked, so two raises
// that race conflict and the loser runs again on the winner's count.
func (k KeyRecords) RaiseWrapCount(ctx context.Context, masterKeyID []byte, n, limit uint64) (bool, error) {
	key := wrapCountKey(masterKeyID)
	var raised bool
	err := k.s.kv.Update(ctx, func(tx kv.Txn) error {
		raised = false
		count, err := counterIn(tx, key)
		if err != nil {
			return err
		}
		if n > limit || count > limit-n {
			return nil
		}
		raised = true
		if n == 0 {
			return nil
		}
		return tx.Set(key, encodeCounter(count+n))
	})
	return raised && err == nil, err
}
