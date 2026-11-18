package kvstore

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"strconv"

	sids "github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
)

// The IDS kinds of the principals' spelling counters. Each holds the last ID
// it gave, only rises, and is read and raised in the transaction that
// creates the record it numbers, never reserved in ranges.
const (
	idUID = 4
	idGID = 5
	idRID = 6
)

// firstRID is the lowest RID a principal is given, above the RIDs Windows
// reserves for its own accounts and groups.
const firstRID = 1000

// allocate appends to ids the spellings a draws for a create of a record of
// kind, refusing an allocation on a write that is not a create, one the
// kind cannot hold, one of a scheme ids already holds, a uid or gid with no
// SID held or allocated beside it, and a uid or gid with no range to draw
// from.
func (c controlTx) allocate(kind entity.PrincipalKind, ids entity.ProtocolIDs, existed bool, a entity.Alloc) (entity.ProtocolIDs, error) {
	if !a.UID && !a.GID && !a.RID {
		return ids, nil
	}
	if existed {
		return nil, merrs.NewInvalidArgumentError("only a create allocates IDs")
	}
	if a.UID && kind != entity.PrincipalUser || a.GID && kind != entity.PrincipalGroup || a.RID && kind == entity.PrincipalMachine {
		return nil, merrs.NewInvalidArgumentError("an allocation of an ID " + kind.String() + " records do not hold")
	}
	// Checked before any counter is read: fileSpellings would refuse the
	// result, blaming the record's IDs rather than the caller's flags.
	if (a.UID || a.GID) && !a.RID && !slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeSID }) {
		return nil, merrs.NewInvalidArgumentError("allocating a uid or gid needs a RID allocated too, or a SID the record holds")
	}
	if (a.UID || a.GID) && a.Range == (entity.IDRange{}) {
		return nil, merrs.NewInvalidArgumentError("a uid or gid allocation needs a range")
	}
	decimal := func(scheme entity.ProtocolScheme) func(uint32) (entity.ProtocolID, error) {
		return func(n uint32) (entity.ProtocolID, error) {
			return entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(n), 10)}, nil
		}
	}
	rid := func(n uint32) (entity.ProtocolID, error) {
		in, err := c.Installation()
		return domainSID(in.ID, n), err
	}
	for _, d := range []struct {
		on      bool
		scheme  entity.ProtocolScheme
		counter byte
		lo, hi  uint32
		spell   func(uint32) (entity.ProtocolID, error)
	}{
		{a.UID, entity.SchemeUID, idUID, a.Range.Lo, a.Range.Hi, decimal(entity.SchemeUID)},
		{a.GID, entity.SchemeGID, idGID, a.Range.Lo, a.Range.Hi, decimal(entity.SchemeGID)},
		{a.RID, entity.SchemeSID, idRID, firstRID, math.MaxUint32, rid},
	} {
		if !d.on {
			continue
		}
		if slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == d.scheme }) {
			return nil, merrs.NewInvalidArgumentError("an allocation of a " + d.scheme.String() + " the record already holds")
		}
		id, err := c.draw(d.counter, d.lo, d.hi, d.scheme != entity.SchemeSID, d.spell)
		if err != nil {
			return nil, err
		}
		ids = append(slices.Clip(ids), id)
	}
	return ids, nil
}

// draw gives the next ID of counter: the counter's value plus one, or lo if
// that is higher, skipping one whose spelling a principal holds, and
// 65534 where reserved. The counter is raised to it, so it is never given
// again, even once its principal is deleted. Every read is tracked, so a
// create racing for the same ID, or a write claiming it, conflicts.
func (c controlTx) draw(counter byte, lo, hi uint32, reserved bool, spell func(uint32) (entity.ProtocolID, error)) (entity.ProtocolID, error) {
	key := idKey(counter)
	mark, err := counterIn(c.tx, key)
	if err != nil {
		return entity.ProtocolID{}, err
	}
	for n := max(mark+1, uint64(lo)); n <= uint64(hi); n++ {
		if reserved && n == 65534 {
			continue
		}
		id, err := spell(uint32(n))
		if err != nil {
			return entity.ProtocolID{}, err
		}
		_, held, err := get(c.tx, protocolKey(id))
		if err != nil {
			return entity.ProtocolID{}, err
		}
		if !held {
			return id, c.tx.Set(key, encodeCounter(n))
		}
	}
	return entity.ProtocolID{}, &merrs.StoreError{Code: merrs.ErrNoSpace, Message: "no ID left to allocate in the range"}
}

// domainSID is rid under the installation's domain SID, S-1-5-21-a-b-c, a, b
// and c the installation ID's first twelve bytes as three little-endian
// 32-bit numbers: derived from the ID each time, never stored.
func domainSID(install [16]byte, rid uint32) entity.ProtocolID {
	var b bytes.Buffer
	sids.EncodeSID(&b, &sids.SID{
		Revision:            1,
		SubAuthorityCount:   5,
		IdentifierAuthority: [6]byte{5: 5},
		SubAuthorities: []uint32{21,
			binary.LittleEndian.Uint32(install[0:4]),
			binary.LittleEndian.Uint32(install[4:8]),
			binary.LittleEndian.Uint32(install[8:12]),
			rid},
	})
	return entity.ProtocolID{Scheme: entity.SchemeSID, Value: b.String()}
}

// fileSpellings refuses a user holding one of a uid and a SID without the
// other, and a group without both its gid and its SID.
//
// decision: a principal with a file spelling holds every one its kind has,
// and one with none holds none, as an API-only account and a machine do; so
// no write stores a user that NFS can name but SMB cannot, or the reverse.
// Withdraw it only if a protocol comes to name principals by one of these
// spellings alone.
func fileSpellings(kind entity.PrincipalKind, ids entity.ProtocolIDs) error {
	has := func(s entity.ProtocolScheme) bool {
		return slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == s })
	}
	switch {
	case kind == entity.PrincipalUser && has(entity.SchemeUID) != has(entity.SchemeSID):
		return merrs.NewInvalidArgumentError("a user holds a uid and a SID, or neither")
	case kind == entity.PrincipalGroup && !(has(entity.SchemeGID) && has(entity.SchemeSID)):
		return merrs.NewInvalidArgumentError("a group holds a gid and a SID")
	}
	return nil
}

// keepSID refuses a write that changes or drops the SID the record holds:
// SMB clients cache it and store it in their own ACLs.
func keepSID(old, ids entity.ProtocolIDs) error {
	for _, id := range old {
		if id.Scheme == entity.SchemeSID && !slices.Contains(ids, id) {
			return merrs.NewInvalidArgumentError("a SID never changes once the principal has one")
		}
	}
	return nil
}
