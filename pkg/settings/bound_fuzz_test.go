package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// FuzzBound reads Bound records no one at DittoFS wrote: no decode panics,
// every refusal is one of the codec's own errors, and a record that decodes,
// re-encoded under the schema this node reads, decodes to the same values.
func FuzzBound(f *testing.F) {
	f.Add([]byte(`{"schema":1,"settings":{"ns.level":3,"ns.prefix":"p/","ns.target":262144}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"schema":9,"settings":{"ns.prefix":"p/"}}`))
	f.Add([]byte(`{"schema":1,"settings":{"ns.codec":"zstd"}}`))
	f.Add([]byte(`{"schema":1,"settings":{"ns.prefix":null}}`))

	table := boundTable()

	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := table.DecodeBound(b)
		if err != nil {
			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNewerSchema) {
				t.Fatalf("DecodeBound(%s) refused with %v, want the codec's own error", b, err)
			}

			return
		}

		again, err := json.Marshal(bound{Schema: 1, Settings: m})
		if err != nil {
			t.Fatalf("re-encode the decoded record: %v", err)
		}

		re, err := table.DecodeBound(again)
		if err != nil {
			t.Fatalf("DecodeBound(%s), the record re-encoded: %v", again, err)
		}

		for k, v := range m {
			if !bytes.Equal(v, re[k]) {
				t.Fatalf("DecodeBound twice: %s decoded to %s and then %s", k, v, re[k])
			}
		}
	})
}
