package kvstore

import (
	"bytes"
	"context"
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
// until the node registers.
func (s *Store) RaiseNodeEpoch(ctx context.Context, node entity.NodeID) (epoch uint64, err error) {
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

// RegisterNode writes what a node registers at a start and returns the
// record written. The epoch its start raised, and whether a takeover marked
// it lapsed, are kept as stored: registering decides neither. Roles other
// than both, and a version range whose minimum is above its maximum, are
// refused.
func (n Nodes) RegisterNode(ctx context.Context, node entity.Node) (rec entity.Node, err error) {
	if node.Roles != entity.RoleProtocol|entity.RoleStorage {
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

// PutNodeStatus replaces the node's status with s.
func (n Nodes) PutNodeStatus(ctx context.Context, s entity.NodeStatus) error {
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

// Record is one record List yields: its Setting or its Share, as the kind
// listed says, and After, what List takes to resume after it.
type Record struct {
	Setting entity.Setting
	Share   entity.Share
	After   []byte
}

// List lists every record of a reconciled kind, settings at every scope in
// key order or shares in folded-name order, resuming after the After of the
// last record read; nil starts from the first. Any other kind is refused.
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
		return pages(ctx, n.s.kv, []byte{keys.KindNX, keys.NXShare}, after, func(r kv.Reader, kvp kv.KeyValue) (Record, error) {
			id, err := decodeID(kvp.Value)
			if err != nil {
				return Record{}, err
			}
			s, err := shareIn(r, id)
			return Record{Share: s, After: bytes.Clone(kvp.Key)}, err
		})
	}
	return func(yield func(Record, error) bool) {
		yield(Record{}, merrs.NewInvalidArgumentError("only settings and shares are listed by kind"))
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
	n := entity.Node{ID: id, Address: r.String(), Roles: entity.Roles(r.Uint8()), Binary: r.String(), Domain: r.String(),
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
	if n.Roles&^(entity.RoleProtocol|entity.RoleStorage) != 0 {
		return n, errMalformed
	}
	return n, nil
}

func putResource(w *codec.Writer, ref entity.ResourceRef) *codec.Writer {
	return w.Uint8(uint8(ref.Kind)).Bytes(ref.ID)
}

// readResource reads a resource reference, setting *known false for one of a
// kind this build does not know.
func readResource(r *codec.Reader, known *bool) entity.ResourceRef {
	ref := entity.ResourceRef{Kind: entity.ResourceKind(r.Uint8()), ID: cloneOrNil(r.Bytes())}
	*known = *known && ref.Kind.String() != ""
	return ref
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
