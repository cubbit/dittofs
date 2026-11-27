package settings

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// nsFields is a namespace section's fields for the Bound codec's tests: a
// next-write key, which the record never holds, and bound keys of the kinds
// the decode reads.
type nsFields struct {
	Codec  string            `json:"codec"  class:"next write"`
	Level  int               `json:"level"  class:"bound"`
	Prefix string            `json:"prefix" class:"bound"`
	Target bytesize.ByteSize `json:"target" class:"bound"`
}

func (nsFields) Validate() error {
	return nil
}

// boundTable is the codec's table over nsFields alone.
func boundTable() Table {
	return Table{
		Of("ns", At(entity.ScopeNamespace), nsFields{Codec: "none", Level: 3, Target: 262144}),
	}
}

// table.Bound encodes every bound key of a namespace section, the ones the
// rows send with the value sent and the others at their declared default,
// re-encoded from the decoded value so two spellings of one value encode
// once, never a next-write key, and its schema is the rows' own.
// DecodeBound returns what Bound encoded.
func TestBoundCodec(t *testing.T) {
	ns := entity.SettingScope{Kind: entity.ScopeNamespace, ID: entity.NamespaceID{'n', 's'}}
	rows := []entity.Setting{
		{Scope: ns, Key: "ns.codec", Value: []byte(`"zstd"`), Schema: 1},
		{Scope: ns, Key: "ns.prefix", Value: []byte(`"p/"`), Schema: 1},
		{Scope: ns, Key: "ns.target", Value: []byte(`"256KiB"`), Schema: 1},
	}
	want := `{"schema":1,"settings":{"ns.level":3,"ns.prefix":"p/","ns.target":262144}}`

	b, err := boundTable().Bound(rows)
	if err != nil {
		t.Fatalf("Bound(%v): %v", rows, err)
	}

	if string(b) != want {
		t.Fatalf("Bound(%v) = %s, want %s", rows, b, want)
	}

	m, err := boundTable().DecodeBound(b)
	if err != nil {
		t.Fatalf("DecodeBound(%s): %v", b, err)
	}

	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != `{"ns.level":3,"ns.prefix":"p/","ns.target":262144}` {
		t.Fatalf("DecodeBound(%s) = %s, want the three bound keys as Bound encodes them", b, got)
	}
}

// DecodeBound refuses a schema the node does not read, a key it does not
// class bound at namespace scope and a value the decode refuses, and reads
// a size written in its spelled form as the bytes it names.
func TestDecodeBoundRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want error
	}{
		{
			"a schema the node does not read",
			`{"schema":2,"settings":{"ns.prefix":"p/"}}`,
			ErrNewerSchema,
		},
		{"a key not classed bound", `{"schema":1,"settings":{"ns.codec":"zstd"}}`, ErrInvalid},
		{"a key no section declares", `{"schema":1,"settings":{"nosuch":"p/"}}`, ErrInvalid},
		{"a value the decode refuses", `{"schema":1,"settings":{"ns.target":"x"}}`, ErrInvalid},
		{"a record with no set", `{"schema":1}`, ErrInvalid},
		{"a record with a null set", `{"schema":1,"settings":null}`, ErrInvalid},
	} {
		if _, err := boundTable().DecodeBound([]byte(tc.in)); !errors.Is(err, tc.want) {
			t.Errorf("%s: DecodeBound(%s) refused with %v, want %v", tc.name, tc.in, err, tc.want)
		}
	}

	m, err := boundTable().DecodeBound([]byte(`{"schema":1,"settings":{"ns.target":"256KiB"}}`))
	if err != nil {
		t.Fatalf(`DecodeBound of a spelled size: %v`, err)
	}

	if string(m["ns.target"]) != "262144" {
		t.Fatalf(`DecodeBound of "256KiB" = %s, want 262144`, m["ns.target"])
	}
}
