package provider

import (
	"bytes"
	"cmp"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// keyRow is one key record as the metadata store's key-record view holds it:
// its kind, its ID, its state and the stored record, wrapped. It is the
// view's row type under another name, to be replaced by it when the view
// lands.
type keyRow = struct {
	Kind, ID, State string
	Record          []byte
}

// keysObject is one version of a namespace's keys object (RFC 5 Appendix
// B.2): every key record of the namespace, at a sequence, under the export key
// whose ID it names.
type keysObject struct {
	ns          entity.NamespaceID
	seq         uint64
	exportKeyID []byte
	rows        []keyRow
}

const (
	keysObjectVersion = 1
	keysObjectLabel   = "dittofs keys object v1"
	keysMACLen        = sha256.Size
	exportKeyLen      = 32

	// decision: a keys object is at most 256 KiB, RFC 4 §4.13's bound on a
	// control object, checked here before any put. At about 150 bytes a
	// record that is about 1,700 records, and since no record is ever
	// deleted, a namespace rotating its data key daily reaches it in about
	// four years. Raise it, or split the object, before a namespace can.
	maxKeysObject = 256 << 10
)

var (
	errKeysObjectTooLarge = errors.New("provider: keys object too large")
	errKeysObjectInvalid  = errors.New("provider: keys object invalid")
)

// keysKey derives the key authenticating a namespace's keys object from the
// export key (RFC 5 Appendix B.1).
func keysKey(exportKey []byte, ns entity.NamespaceID) ([]byte, error) {
	if len(exportKey) != exportKeyLen {
		return nil, fmt.Errorf("provider: an export key of %d bytes, want %d", len(exportKey), exportKeyLen)
	}

	return hkdf.Key(sha256.New, exportKey, ns[:], keysObjectLabel, sha256.Size)
}

// keysMAC is the keys MAC over one version (RFC 5 Appendix B.1): the namespace
// ID, the sequence, the export key's ID and the SHA-256 of records, the
// records field's bytes, each field that is not an integer length-prefixed.
func keysMAC(exportKey []byte, ns entity.NamespaceID, seq uint64, exportKeyID, records []byte) ([]byte, error) {
	key, err := keysKey(exportKey, ns)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(records)
	m := hmac.New(sha256.New, key)
	m.Write(lenPrefixed(nil, ns[:]))
	m.Write(binary.BigEndian.AppendUint64(nil, seq))
	m.Write(lenPrefixed(nil, exportKeyID))
	m.Write(lenPrefixed(nil, sum[:]))

	return m.Sum(nil), nil
}

func lenPrefixed(b, field []byte) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(field))), field...)
}

// decision: rows are ordered by the byte-wise order of the kind's name, then
// ID, not by the kind byte the metadata store keys them under, so the
// provider never needs the store's encoding of a kind. The order is part of
// version 1's bytes; changing it is a version 2.
func compareRows(a, b keyRow) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
}

// encodeRecords is the records field: the row count, then each row's kind, ID,
// state and record, in the order of kind and then ID, so one set of records is
// one byte string. Two rows of one kind and ID are refused.
func encodeRecords(o keysObject) ([]byte, error) {
	rows := slices.SortedFunc(slices.Values(o.rows), compareRows)
	b := binary.BigEndian.AppendUint64(nil, uint64(len(rows)))

	for i, r := range rows {
		if i > 0 && compareRows(rows[i-1], r) == 0 {
			return nil, fmt.Errorf("%w: namespace %s holds two records of kind %q and ID %q", errKeysObjectInvalid, o.ns, r.Kind, r.ID)
		}

		b = lenPrefixed(b, []byte(r.Kind))
		b = lenPrefixed(b, []byte(r.ID))
		b = lenPrefixed(b, []byte(r.State))
		b = lenPrefixed(b, r.Record)
	}

	return b, nil
}

// encodeKeysObject returns o's bytes, version 1, under a keys MAC of
// exportKey. An object past maxKeysObject is refused, naming the namespace and
// the record count.
func encodeKeysObject(o keysObject, exportKey []byte) ([]byte, error) {
	records, err := encodeRecords(o)
	if err != nil {
		return nil, err
	}

	mac, err := keysMAC(exportKey, o.ns, o.seq, o.exportKeyID, records)
	if err != nil {
		return nil, err
	}

	b := []byte{keysObjectVersion}
	b = lenPrefixed(b, o.ns[:])
	b = binary.BigEndian.AppendUint64(b, o.seq)
	b = lenPrefixed(b, o.exportKeyID)
	b = append(b, records...)
	b = append(b, mac...)

	if len(b) > maxKeysObject {
		return nil, fmt.Errorf("%w: namespace %s with %d records needs %d bytes, more than %d", errKeysObjectTooLarge, o.ns, len(o.rows), len(b), maxKeysObject)
	}

	return b, nil
}

// decodeKeysObject reads one version of ns's keys object and verifies its keys
// MAC under exportKey, so no sequence or row it returns is unauthenticated.
// It refuses what parseKeysObject refuses and a keys MAC that does not verify.
func decodeKeysObject(b []byte, ns entity.NamespaceID, exportKey []byte) (keysObject, error) {
	o, records, err := parseKeysObject(b, ns)
	if err != nil {
		return keysObject{}, err
	}

	want, err := keysMAC(exportKey, ns, o.seq, o.exportKeyID, records)
	if err != nil {
		return keysObject{}, err
	}

	if !hmac.Equal(b[len(b)-keysMACLen:], want) {
		return keysObject{}, fmt.Errorf("%w: namespace %s: keys MAC does not verify", errKeysObjectInvalid, ns)
	}

	return o, nil
}

// parseKeysObject reads one version of ns's keys object without its keys MAC,
// and returns its records field's bytes. It refuses an unknown version,
// another namespace, a length past the end, trailing bytes, and rows out of
// order or repeated.
func parseKeysObject(b []byte, ns entity.NamespaceID) (keysObject, []byte, error) {
	bad := func(what string) (keysObject, []byte, error) {
		return keysObject{}, nil, fmt.Errorf("%w: namespace %s: %s", errKeysObjectInvalid, ns, what)
	}

	if len(b) > maxKeysObject {
		return bad(fmt.Sprintf("%d bytes, more than %d", len(b), maxKeysObject))
	}

	if len(b) < 1+keysMACLen {
		return bad("shorter than its version and MAC")
	}

	if b[0] != keysObjectVersion {
		return bad(fmt.Sprintf("unknown version %d", b[0]))
	}

	// The body's capacity ends where the MAC starts, so no read reaches it.
	body := b[1 : len(b)-keysMACLen : len(b)-keysMACLen]
	r := reader{b: body}

	if id := r.field(); r.err == nil && !bytes.Equal(id, ns[:]) {
		return bad("written for another namespace")
	}

	o := keysObject{ns: ns, seq: r.uint64()}
	o.exportKeyID = r.field()
	recordsAt := r.off

	// The count bounds nothing up front: each row consumes at least its four
	// length prefixes, so a count past the bytes left ends at the first
	// length past the end.
	for n := r.uint64(); n > 0 && r.err == nil; n-- {
		o.rows = append(o.rows, keyRow{Kind: string(r.field()), ID: string(r.field()), State: string(r.field()), Record: bytes.Clone(r.field())})
	}

	if r.err != nil {
		return bad(r.err.Error())
	}

	if r.off != len(body) {
		return bad(fmt.Sprintf("%d trailing bytes", len(body)-r.off))
	}

	for i := 1; i < len(o.rows); i++ {
		if compareRows(o.rows[i-1], o.rows[i]) >= 0 {
			return bad(fmt.Sprintf("row %d out of order or repeated", i))
		}
	}

	return o, body[recordsAt:], nil
}

// reader reads big-endian integers and length-prefixed fields off b, and
// keeps its first failure.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) next(n uint64) []byte {
	if r.err != nil {
		return nil
	}

	if n > uint64(len(r.b)-r.off) {
		r.err = fmt.Errorf("a length of %d at byte %d runs past the end", n, r.off)
		return nil
	}

	out := r.b[r.off : r.off+int(n) : r.off+int(n)]
	r.off += int(n)

	return out
}

func (r *reader) uint64() uint64 {
	if b := r.next(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}

	return 0
}

func (r *reader) field() []byte {
	b := r.next(4)
	if b == nil {
		return nil
	}

	return r.next(uint64(binary.BigEndian.Uint32(b)))
}
