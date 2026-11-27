package provider

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The fixture's inputs. testdata/keys-object-v1.bin was written from them by
// testdata/keys-object-v1.py, an independent implementation working from RFC
// 5 Appendix B.1's keys key and keys MAC and the object's layout alone, which
// also printed wantKeysKey and wantKeysMAC.
var (
	fixtureNS        = entity.NamespaceID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	fixtureExportKey = func() []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = byte(0x20 + i)
		}
		return k
	}()
	fixtureObject = keysObject{
		ns:          fixtureNS,
		seq:         7,
		exportKeyID: []byte("export-1"),
		rows: []keyRow{
			{Kind: "chunk-id-key", ID: "c1", State: "current", Record: bytes.Repeat([]byte{1}, 40)},
			{Kind: "data-key", ID: "d1", State: "retired", Record: bytes.Repeat([]byte{2}, 60)},
			{Kind: "data-key", ID: "d2", State: "current", Record: bytes.Repeat([]byte{3}, 60)},
			{Kind: "export-key", ID: "export-1", State: "current", Record: bytes.Repeat([]byte{4}, 60)},
		},
	}
)

// ponytail: the keys key and MAC are pinned here, from the fixture's
// generator. Read them from RFC 5's golden vector file through
// transformtest.LoadVectors once it carries the keys MAC, so one vector
// serves every implementation.
const (
	wantKeysKey = "205ac371d668e0693673b59c87ec087061fdbe7503bdf93464a006d9a7b7a3b6" // gitleaks:allow, a test vector, not a secret
	wantKeysMAC = "1325d06713c4ce38652ada2ee9195c0c673186b41df8a6ce0637ff3b3e6014b3" // gitleaks:allow, a test vector, not a secret
)

func readFixture(tb testing.TB) []byte {
	tb.Helper()

	b, err := os.ReadFile("testdata/keys-object-v1.bin")
	if err != nil {
		tb.Fatal(err)
	}

	return b
}

// The keys key and MAC of the vector's inputs equal the independent
// implementation's, so a joined field laid out apart from Appendix B.1 (a
// missing length prefix, a 4-byte sequence, a hash of other bytes) fails.
func TestKeysKeyAndMACMatchTheVector(t *testing.T) {
	prof.Watch(t, prof.Options{})

	key, err := keysKey(fixtureExportKey, fixtureNS)
	if err != nil {
		t.Fatal(err)
	}

	if got := hex.EncodeToString(key); got != wantKeysKey {
		t.Errorf("keys key %s, want %s", got, wantKeysKey)
	}

	records, err := encodeRecords(fixtureObject)
	if err != nil {
		t.Fatal(err)
	}

	mac, err := keysMAC(fixtureExportKey, fixtureNS, fixtureObject.seq, fixtureObject.exportKeyID, records)
	if err != nil {
		t.Fatal(err)
	}

	if got := hex.EncodeToString(mac); got != wantKeysMAC {
		t.Errorf("keys MAC %s, want %s", got, wantKeysMAC)
	}
}

// The fixture decodes to its known fields, and those fields, given in any
// order, encode to the fixture's bytes, so a layout change that would strand
// every object already written fails.
func TestKeysObjectFixtureBothWays(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fixture := readFixture(t)

	got, err := decodeKeysObject(fixture, fixtureNS, fixtureExportKey)
	if err != nil {
		t.Fatalf("decode of the fixture: %v", err)
	}

	if !reflect.DeepEqual(got, fixtureObject) {
		t.Errorf("the fixture decodes to %+v, want %+v", got, fixtureObject)
	}

	shuffled := fixtureObject
	shuffled.rows = []keyRow{fixtureObject.rows[3], fixtureObject.rows[2], fixtureObject.rows[0], fixtureObject.rows[1]}

	enc, err := encodeKeysObject(shuffled, fixtureExportKey)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(enc, fixture) {
		t.Errorf("the fixture's fields encode to\n%x\nwant\n%x", enc, fixture)
	}
}

// recordsAt is where the fixture's records field starts: after its version,
// namespace, sequence and export key ID.
var recordsAt = 1 + 4 + 16 + 8 + 4 + len(fixtureObject.exportKeyID)

// withRows is the fixture with its records field replaced by rows' bytes in
// the order given, and its MAC kept.
func withRows(fixture []byte, rows ...keyRow) []byte {
	b := binary.BigEndian.AppendUint64(bytes.Clone(fixture[:recordsAt]), uint64(len(rows)))
	for _, r := range rows {
		b = lenPrefixed(lenPrefixed(lenPrefixed(lenPrefixed(b, []byte(r.Kind)), []byte(r.ID)), []byte(r.State)), r.Record)
	}

	return append(b, fixture[len(fixture)-keysMACLen:]...)
}

// One row's bytes changed, the sequence or the export key ID changed, under
// the fixture's MAC, and two rows swapped: the MAC fails, so a MAC over less than
// the records and the fields before them, or over the records as a set,
// fails.
func TestKeysMACCoversEveryRecordInOrder(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fixture := readFixture(t)
	r := fixtureObject.rows

	changed := r[1]
	changed.Record = append(bytes.Clone(changed.Record[:59]), 0xff)

	seq := bytes.Clone(fixture)
	seq[1+4+16+7]++

	exportKeyID := bytes.Clone(fixture)
	exportKeyID[recordsAt-1]++

	for name, b := range map[string][]byte{
		"one row's record changed":    withRows(fixture, r[0], changed, r[2], r[3]),
		"the sequence changed":        seq,
		"the export key's ID changed": exportKeyID,
	} {
		if _, err := decodeKeysObject(b, fixtureNS, fixtureExportKey); err == nil || !strings.Contains(err.Error(), "keys MAC does not verify") {
			t.Errorf("%s: decode error %v, want the keys MAC refused", name, err)
		}
	}

	// The decoder refuses swapped rows by their order before their MAC, so
	// the MAC over them is checked apart.
	swapped := withRows(fixture, r[0], r[2], r[1], r[3])

	mac, err := keysMAC(fixtureExportKey, fixtureNS, fixtureObject.seq, fixtureObject.exportKeyID, swapped[recordsAt:len(swapped)-keysMACLen])
	if err != nil {
		t.Fatal(err)
	}

	if hex.EncodeToString(mac) == wantKeysMAC {
		t.Errorf("two rows swapped: keys MAC %x, want one other than the fixture's", mac)
	}
}

// An object one byte over 256 KiB is refused by the encoder, naming the
// namespace and the record count, and one at the bound is not, so a bound
// left to the store fails.
func TestKeysObjectBound(t *testing.T) {
	prof.Watch(t, prof.Options{})

	o := keysObject{ns: fixtureNS, seq: 1, exportKeyID: []byte("export-1"), rows: []keyRow{{Kind: "data", ID: "d1", State: "active"}}}

	empty, err := encodeKeysObject(o, fixtureExportKey)
	if err != nil {
		t.Fatal(err)
	}

	o.rows[0].Record = make([]byte, maxKeysObject-len(empty))
	if b, err := encodeKeysObject(o, fixtureExportKey); err != nil || len(b) != maxKeysObject {
		t.Fatalf("an object of exactly %d bytes: %d bytes, error %v, want it encoded", maxKeysObject, len(b), err)
	}

	o.rows[0].Record = make([]byte, maxKeysObject-len(empty)+1)

	_, err = encodeKeysObject(o, fixtureExportKey)
	if !errors.Is(err, errKeysObjectTooLarge) {
		t.Fatalf("an object of %d bytes: error %v, want %v", maxKeysObject+1, err, errKeysObjectTooLarge)
	}

	for _, want := range []string{fixtureNS.String(), "1 records"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// Two records of one kind and ID are refused by the encoder, which would
// otherwise write one set of records as two byte strings.
func TestEncodeKeysObjectRefusesARepeatedRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	o := fixtureObject
	o.rows = append([]keyRow{{Kind: "data-key", ID: "d1", State: "current"}}, fixtureObject.rows...)

	if _, err := encodeKeysObject(o, fixtureExportKey); !errors.Is(err, errKeysObjectInvalid) || !strings.Contains(err.Error(), `kind "data-key" and ID "d1"`) {
		t.Errorf("error %v, want the repeated data-key d1 refused", err)
	}
}

// Every refusal of the decoder, each for its own reason.
func TestDecodeKeysObjectRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fixture := readFixture(t)
	r := fixtureObject.rows
	mutate := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(fixture)) }

	for _, tc := range []struct {
		name      string
		b         []byte
		ns        entity.NamespaceID
		exportKey []byte
		want      string
	}{
		{"an unknown version", mutate(func(b []byte) []byte { b[0] = 2; return b }), fixtureNS, fixtureExportKey, "unknown version 2"},
		{"another namespace", fixture, entity.NamespaceID{15}, fixtureExportKey, "written for another namespace"},
		{"a length past the end", fixture[:len(fixture)-keysMACLen-1], fixtureNS, fixtureExportKey, "runs past the end"},
		{"a length into the MAC", fixture[:len(fixture)-1], fixtureNS, fixtureExportKey, "runs past the end"},
		{"a record count past the end", mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint64(b[recordsAt:], 1<<63)
			return b
		}), fixtureNS, fixtureExportKey, "runs past the end"},
		{"trailing bytes", append(bytes.Clone(fixture[:len(fixture)-keysMACLen]), append([]byte{0}, fixture[len(fixture)-keysMACLen:]...)...), fixtureNS, fixtureExportKey, "1 trailing bytes"},
		{"shorter than a MAC", fixture[:keysMACLen], fixtureNS, fixtureExportKey, "shorter than its version and MAC"},
		{"past the bound", make([]byte, maxKeysObject+1), fixtureNS, fixtureExportKey, "more than"},
		{"a MAC that does not verify", mutate(func(b []byte) []byte { b[len(b)-1] ^= 1; return b }), fixtureNS, fixtureExportKey, "keys MAC does not verify"},
		{"another export key", fixture, fixtureNS, make([]byte, 32), "keys MAC does not verify"},
		{"rows out of order", withRows(fixture, r[0], r[2], r[1], r[3]), fixtureNS, fixtureExportKey, "row 2 out of order or repeated"},
		{"a row repeated", withRows(fixture, r[0], r[1], r[1], r[3]), fixtureNS, fixtureExportKey, "row 2 out of order or repeated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeKeysObject(tc.b, tc.ns, tc.exportKey)
			if !errors.Is(err, errKeysObjectInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want %v naming %q", err, errKeysObjectInvalid, tc.want)
			}
		})
	}
}

// An export key that is missing or not 32 bytes is refused by both the
// decoder and the encoder, so no object is authenticated under a key anyone
// can compute.
func TestKeysObjectRefusesAnExportKeyOfTheWrongLength(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fixture := readFixture(t)

	for _, key := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33)} {
		if _, err := decodeKeysObject(fixture, fixtureNS, key); err == nil || !strings.Contains(err.Error(), "want 32") {
			t.Errorf("decode under a %d-byte export key: error %v, want the key refused", len(key), err)
		}

		if _, err := encodeKeysObject(fixtureObject, key); err == nil || !strings.Contains(err.Error(), "want 32") {
			t.Errorf("encode under a %d-byte export key: error %v, want the key refused", len(key), err)
		}
	}
}

// The decoder never panics on any bytes and never reads past its buffer: the
// body's capacity ends at the MAC, so a length read past it panics here. What
// it parses verifies exactly when the bytes end with the keys MAC of the
// parsed fields, re-encodes to the same bytes up to the MAC, and those decode,
// under the MAC the encoder gave them, to the same value.
func FuzzKeysObject(f *testing.F) {
	fixture := readFixture(f)
	f.Add(fixture)

	empty, err := encodeKeysObject(keysObject{ns: fixtureNS, exportKeyID: []byte("export-1")}, fixtureExportKey)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(empty)
	f.Add(fixture[:len(fixture)-keysMACLen-1])
	f.Add(fixture[:len(fixture)-1])

	past := bytes.Clone(fixture)
	binary.BigEndian.PutUint32(past[1:], 1<<31)
	f.Add(past)

	f.Fuzz(func(t *testing.T, b []byte) {
		o, records, err := parseKeysObject(b, fixtureNS)
		if err != nil {
			if _, derr := decodeKeysObject(b, fixtureNS, fixtureExportKey); derr == nil {
				t.Fatalf("decoded bytes that do not parse: %v", err)
			}

			return
		}

		mac, err := keysMAC(fixtureExportKey, fixtureNS, o.seq, o.exportKeyID, records)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := decodeKeysObject(b, fixtureNS, fixtureExportKey); (err == nil) != bytes.Equal(b[len(b)-keysMACLen:], mac) {
			t.Fatalf("decode error %v, with the bytes' MAC equal to the keys MAC %v", err, bytes.Equal(b[len(b)-keysMACLen:], mac))
		}

		enc, err := encodeKeysObject(o, fixtureExportKey)
		if err != nil {
			t.Fatalf("a parsed object does not encode: %v", err)
		}

		if !bytes.Equal(enc[:len(enc)-keysMACLen], b[:len(b)-keysMACLen]) {
			t.Fatalf("re-encoded as\n%x\nfrom\n%x", enc, b)
		}

		again, err := decodeKeysObject(enc, fixtureNS, fixtureExportKey)
		if err != nil {
			t.Fatalf("the re-encoded object does not decode: %v", err)
		}

		if !reflect.DeepEqual(again, o) {
			t.Fatalf("the re-encoded object decodes to %+v, want %+v", again, o)
		}
	})
}
