package kvstore

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Nodes is the node's own view: its record, its status, the counters it
// reconciles through and the records it reconciles. Only a node writes its
// status, and the management API's view has no write of one.
type Nodes struct{ s *Store }

func (s *Store) Nodes() Nodes { return Nodes{s} }

// Counters is each reconciled kind's counter: a record of the kind is
// applied on a node once the node has reconciled the kind through the
// record's position.
type Counters struct {
	Settings   uint64 // the settings generation
	ShareList  uint64 // the share list version
	Principals uint64 // the principals generation
	Claims     uint64 // the claim generation
}

// Counters reads every counter in one snapshot.
//
// ponytail: Claims is always zero, as no claim generation is kept until
// namespace claims are written. Read it here beside the others once one is.
func (n Nodes) Counters(ctx context.Context) (c Counters, err error) {
	err = n.s.kv.View(ctx, func(r kv.Reader) (err error) {
		if c.Settings, err = counterIn(r, cfgGenKey); err != nil {
			return err
		}
		if c.ShareList, err = counterIn(r, shareListKey); err != nil {
			return err
		}
		c.Principals, err = counterIn(r, pgenKey)
		return err
	})
	return c, err
}

// RaiseNodeEpoch raises node's epoch by one, from zero, in its node record
// and its own transaction, and returns the raised value: every start's is
// above every earlier start's, so what a start records under it is told from
// what an earlier process left. A record it creates holds only the epoch
// until the node registers. The zero node ID is refused.
func (s *Store) RaiseNodeEpoch(ctx context.Context, node entity.NodeID) (epoch uint64, err error) {
	if node == (entity.NodeID{}) {
		return 0, errZeroNode()
	}
	err = s.kv.Update(ctx, func(tx kv.Txn) error {
		n, _, err := nodeIn(tx, node)
		if err != nil {
			return err
		}
		n.Epoch++
		epoch = n.Epoch
		return tx.Set(nodeKey(node), encodeNode(n))
	})
	return epoch, err
}

// errZeroNode refuses a node write that names no node.
func errZeroNode() error { return merrs.NewInvalidArgumentError("a node record names its node") }

// RegisterNode writes what a node registers at a start and returns the
// record as stored. Its epoch, and whether a takeover marked it lapsed, are
// kept as stored, not taken from node: registering decides neither, so the
// epoch returned is the last one raised, which is this start's only once
// this start has raised it. The zero node ID, roles other than both, and a
// version range whose minimum is above its maximum, are refused.
func (n Nodes) RegisterNode(ctx context.Context, node entity.Node) (rec entity.Node, err error) {
	if node.ID == (entity.NodeID{}) {
		return rec, errZeroNode()
	}
	if node.Roles != entity.NodeRoleProtocol|entity.NodeRoleStorage {
		return rec, merrs.NewInvalidArgumentError("a node serves both the protocol and the storage role")
	}
	for _, r := range node.Formats {
		if r.Min > r.Max {
			return rec, merrs.NewInvalidArgumentError("a version range's minimum is above its maximum")
		}
	}
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		old, _, err := nodeIn(tx, node.ID)
		if err != nil {
			return err
		}
		rec = node
		rec.Epoch, rec.Lapsed = old.Epoch, old.Lapsed
		return tx.Set(nodeKey(node.ID), encodeNode(rec))
	})
	return rec, err
}

// Node reads one node's record; false when it has none.
func (n Nodes) Node(ctx context.Context, id entity.NodeID) (rec entity.Node, found bool, err error) {
	err = n.s.kv.View(ctx, func(r kv.Reader) error {
		rec, found, err = nodeIn(r, id)
		return err
	})
	return rec, found, err
}

// nodeIn reads id's record, the record with only its ID when there is none.
func nodeIn(r kv.Reader, id entity.NodeID) (entity.Node, bool, error) {
	b, ok, err := get(r, nodeKey(id))
	if err != nil || !ok {
		return entity.Node{ID: id}, false, err
	}
	n, err := decodeNode(id, b)
	return n, err == nil, err
}

// PutNodeStatus replaces the node's status with s; the zero node ID is
// refused.
func (n Nodes) PutNodeStatus(ctx context.Context, s entity.NodeStatus) error {
	if s.Node == (entity.NodeID{}) {
		return errZeroNode()
	}
	return n.s.kv.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(nodeStatusKey(s.Node), encodeNodeStatus(s))
	})
}

// NodeStatuses lists every node's status in node ID order, resuming after
// the node after; the zero ID starts from the first.
func (n Nodes) NodeStatuses(ctx context.Context, after entity.NodeID) iter.Seq2[entity.NodeStatus, error] {
	var cursor []byte
	if after != (entity.NodeID{}) {
		cursor = nodeStatusKey(after)
	}
	type row struct {
		s  entity.NodeStatus
		ok bool
	}
	// The node records and their lease and clock rows share the prefix.
	rows := pages(ctx, n.s.kv, []byte{keys.KindN}, cursor, func(_ kv.Reader, kvp kv.KeyValue) (row, error) {
		if len(kvp.Key) != 2+keys.IDLen || kvp.Key[1+keys.IDLen] != keys.NSt {
			return row{}, nil
		}
		s, err := decodeNodeStatus(entity.NodeID(kvp.Key[1:1+keys.IDLen]), kvp.Value)
		return row{s, err == nil}, err
	})
	return func(yield func(entity.NodeStatus, error) bool) {
		for r, err := range rows {
			if err != nil {
				yield(entity.NodeStatus{}, err)
				return
			}
			if r.ok && !yield(r.s, nil) {
				return
			}
		}
	}
}

// RegisteredStatuses reads every node that has a node record, in node ID
// order, with its status, all in one view: a node that has reported no
// status reads as the zero status naming it, and a status whose node has no
// record, as one removed from the installation leaves, is left out.
//
// ponytail: every call reads every node's rows in one view, O(nodes). Keep
// a count of registered nodes beside the records when a listing's page
// shows this read in a profile.
func (n Nodes) RegisteredStatuses(ctx context.Context) (sts []entity.NodeStatus, err error) {
	err = n.s.kv.View(ctx, func(r kv.Reader) error {
		sts = sts[:0]
		for kvp, err := range r.Scan([]byte{keys.KindN}, nil) {
			if err != nil {
				return err
			}
			k := kvp.Key
			switch {
			case len(k) == 1+keys.IDLen:
				sts = append(sts, entity.NodeStatus{Node: entity.NodeID(k[1:])})
			case len(k) == 2+keys.IDLen && k[1+keys.IDLen] == keys.NSt &&
				len(sts) > 0 && sts[len(sts)-1].Node == entity.NodeID(k[1:1+keys.IDLen]):
				s, err := decodeNodeStatus(sts[len(sts)-1].Node, kvp.Value)
				if err != nil {
					return err
				}
				sts[len(sts)-1] = s
			}
		}
		return nil
	})
	return sts, err
}

// NodeRecords lists every node's record in node ID order, resuming after
// the node after; the zero ID starts from the first.
func (n Nodes) NodeRecords(ctx context.Context, after entity.NodeID) iter.Seq2[entity.Node, error] {
	var cursor []byte
	if after != (entity.NodeID{}) {
		cursor = nodeKey(after)
	}
	type row struct {
		n  entity.Node
		ok bool
	}
	// A node's status, lease and clock rows share its record's prefix.
	rows := pages(ctx, n.s.kv, []byte{keys.KindN}, cursor, func(_ kv.Reader, kvp kv.KeyValue) (row, error) {
		if len(kvp.Key) != 1+keys.IDLen {
			return row{}, nil
		}
		rec, err := decodeNode(entity.NodeID(kvp.Key[1:]), kvp.Value)
		return row{rec, err == nil}, err
	})
	return func(yield func(entity.Node, error) bool) {
		for r, err := range rows {
			if err != nil {
				yield(entity.Node{}, err)
				return
			}
			if r.ok && !yield(r.n, nil) {
				return
			}
		}
	}
}

var _ entity.SettingsReader = Nodes{}

// AllSettings lists every scope's settings in key order, resuming after the
// setting after names, one read transaction per page, so a write between two
// pages shows only in a page read after it. The zero key starts from the
// first: its scope kind is zero, below every scope a setting is stored at.
//
// decision: a row that does not decode is yielded with an error wrapping
// entity.ErrBadRow and the cause, its Setting carrying the scope and key when
// the row's key parses, and the listing goes on, because a record that does
// not validate stops only what it configures (RFC 13 §6). Any other error is
// the scan failing: it is yielded as it is and ends the listing.
func (n Nodes) AllSettings(
	ctx context.Context,
	after entity.SettingKey,
) iter.Seq2[entity.Setting, error] {
	type row struct {
		s   entity.Setting
		err error
	}

	cursor := settingKey(after.Scope, after.Key)

	// load never fails, so pages ends the listing only on a scan error.
	rows := pages(
		ctx,
		n.s.kv,
		[]byte{keys.KindCFG},
		cursor,
		func(_ kv.Reader, kvp kv.KeyValue) (row, error) {
			scope, key, err := settingAt(kvp.Key)
			if err != nil {
				return row{err: badRow(err)}, nil
			}

			s, err := decodeSetting(scope, key, kvp.Value)
			if err != nil {
				return row{s: entity.Setting{Scope: scope, Key: key}, err: badRow(err)}, nil
			}

			return row{s: s}, nil
		},
	)

	return func(yield func(entity.Setting, error) bool) {
		for r, err := range rows {
			if err != nil {
				yield(entity.Setting{}, err)

				return
			}

			if !yield(r.s, r.err) {
				return
			}
		}
	}
}

// badRow wraps err, why a setting row does not decode, in entity.ErrBadRow.
func badRow(err error) error {
	return fmt.Errorf("%w: %w", entity.ErrBadRow, err)
}

// Record is one record List yields: its Setting or its Share, as the kind
// listed says, and After, what List takes to resume after it.
type Record struct {
	Setting entity.Setting
	Share   entity.Share
	After   []byte
}

// List lists every record of a reconciled kind, settings at every scope in
// key order or shares not being removed in share ID order, resuming after
// the After of the last record read; nil starts from the first. Any other
// kind is refused.
func (n Nodes) List(ctx context.Context, kind entity.ResourceKind, after []byte) iter.Seq2[Record, error] {
	switch kind {
	case entity.ResourceSetting:
		return pages(ctx, n.s.kv, []byte{keys.KindCFG}, after, func(_ kv.Reader, kvp kv.KeyValue) (Record, error) {
			scope, key, err := settingAt(kvp.Key)
			if err != nil {
				return Record{}, err
			}
			s, err := decodeSetting(scope, key, kvp.Value)
			return Record{Setting: s, After: bytes.Clone(kvp.Key)}, err
		})
	case entity.ResourceShare:
		return sharesByID(ctx, n.s.kv, after)
	}
	return func(yield func(Record, error) bool) {
		yield(Record{}, merrs.NewInvalidArgumentError("only settings and shares are listed by kind"))
	}
}

// sharesByID lists every share not being removed, in share ID order,
// resuming after the After of the last one read. It walks the shares' own
// rows, not the name index, so a share renamed between two pages is listed
// once. A share's info row sorts first of its rows, so each share costs one
// seek, and the next seek starts past every row of the share.
//
// ponytail: one scan per share, rows of a share without its info row
// skipped one seek at a time. Keep an ID-keyed share index instead when a
// profile shows the seeks.
func sharesByID(ctx context.Context, db kv.KV, after []byte) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		for {
			var page []Record
			cursor := after
			err := db.View(ctx, func(r kv.Reader) error {
				page, cursor = page[:0], after
				for len(page) < pageSize {
					var key []byte
					for kvp, err := range r.Scan([]byte{keys.KindS}, cursor) {
						if err != nil {
							return err
						}
						key = bytes.Clone(kvp.Key)
						break
					}
					if key == nil {
						return nil
					}
					if len(key) < 2+keys.IDLen {
						return errMalformed
					}
					id := entity.ShareID(key[1 : 1+keys.IDLen])
					if cursor = shareKey(id, 0xFF); key[1+keys.IDLen] != keys.SInfo {
						continue
					}
					s, err := shareIn(r, id)
					if err != nil {
						return err
					}
					if s.State != entity.ShareRemoving && s.State != entity.ShareRetired {
						page = append(page, Record{Share: s, After: cursor})
					}
				}
				return nil
			})
			if err != nil {
				yield(Record{}, err)
				return
			}
			for _, rec := range page {
				if !yield(rec, nil) {
					return
				}
			}
			if len(page) < pageSize {
				return
			}
			after = cursor
		}
	}
}

// settingAt is the scope and key a setting's key names.
func settingAt(k []byte) (entity.SettingScope, string, error) {
	if len(k) < 2 {
		return entity.SettingScope{}, "", errMalformed
	}
	scope, rest := entity.SettingScope{Kind: entity.ScopeKind(k[1])}, k[2:]
	if scope.Kind != entity.ScopeInstallation {
		id, r, err := keys.ReadID(rest, keys.IDLen)
		if err != nil {
			return scope, "", err
		}
		scope.ID, rest = [16]byte(id), r
	}
	key, rest, err := keys.ReadString(rest)
	if err == nil && len(rest) != 0 {
		err = errMalformed
	}
	return scope, string(key), err
}

func nodeKey(id entity.NodeID) []byte { return keys.ID([]byte{keys.KindN}, id[:]) }

func nodeStatusKey(id entity.NodeID) []byte { return append(nodeKey(id), keys.NSt) }

func encodeNode(n entity.Node) []byte {
	w := codec.NewWriter(v1).String(n.Address).Uint8(uint8(n.Roles)).String(n.Binary).String(n.Domain).
		Uint64(n.Epoch).Len(len(n.Formats))
	for _, f := range slices.Sorted(maps.Keys(n.Formats)) {
		w.Uint8(uint8(f)).Uint32(n.Formats[f].Min).Uint32(n.Formats[f].Max)
	}
	return w.Bool(n.Lapsed).Encode()
}

func decodeNode(id entity.NodeID, b []byte) (entity.Node, error) {
	r, err := open(b)
	if err != nil {
		return entity.Node{}, err
	}
	n := entity.Node{ID: id, Address: r.String(), Roles: entity.NodeRoles(r.Uint8()), Binary: r.String(), Domain: r.String(),
		Epoch: r.Uint64()}
	if k := r.Len(); k > 0 {
		n.Formats = make(map[entity.Format]entity.VersionRange, k)
		for range k {
			n.Formats[entity.Format(r.Uint8())] = entity.VersionRange{Min: r.Uint32(), Max: r.Uint32()}
		}
	}
	n.Lapsed = r.Bool()
	if err := r.Done(); err != nil {
		return n, err
	}
	if n.Roles&^(entity.NodeRoleProtocol|entity.NodeRoleStorage) != 0 {
		return n, errMalformed
	}
	return n, nil
}

func encodeNodeStatus(s entity.NodeStatus) []byte {
	w := codec.NewWriter(v1).Uint64(s.Settings).Uint64(s.ShareList).Uint64(s.Principals).Uint64(s.Claims)
	w.Len(len(s.Refused))
	for _, rr := range s.Refused {
		putResource(w, rr.Record).Uint64(rr.Generation).Bytes(rr.Error)
	}
	w.Len(len(s.RestartPending))
	for _, ref := range s.RestartPending {
		putResource(w, ref)
	}
	w.Len(len(s.Secrets))
	for _, sv := range s.Secrets {
		w.Fixed(sv.Secret[:]).Uint64(sv.Version)
	}
	return w.Bytes(s.Conditions).Encode()
}

func decodeNodeStatus(node entity.NodeID, b []byte) (entity.NodeStatus, error) {
	r, err := open(b)
	if err != nil {
		return entity.NodeStatus{}, err
	}
	s := entity.NodeStatus{Node: node, Settings: r.Uint64(), ShareList: r.Uint64(), Principals: r.Uint64(), Claims: r.Uint64()}
	known := true
	if k := r.Len(); k > 0 {
		s.Refused = make([]entity.RefusedRecord, k)
		for i := range s.Refused {
			s.Refused[i] = entity.RefusedRecord{Record: readResource(r, &known), Generation: r.Uint64(), Error: cloneOrNil(r.Bytes())}
		}
	}
	if k := r.Len(); k > 0 {
		s.RestartPending = make([]entity.ResourceRef, k)
		for i := range s.RestartPending {
			s.RestartPending[i] = readResource(r, &known)
		}
	}
	if k := r.Len(); k > 0 {
		s.Secrets = make([]entity.SecretVersion, k)
		for i := range s.Secrets {
			copy(s.Secrets[i].Secret[:], r.Fixed(16))
			s.Secrets[i].Version = r.Uint64()
		}
	}
	s.Conditions = cloneOrNil(r.Bytes())
	if err := r.Done(); err != nil {
		return s, err
	}
	if !known {
		return s, errMalformed
	}
	return s, nil
}
