package settings

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// bound is a Bound record on the wire: the settings schema its rows carry
// and every key its table classes bound at namespace scope (RFC 13 §5.3,
// RFC 16 §2.3).
type bound struct {
	Schema   uint32                     `json:"schema"`
	Settings map[string]json.RawMessage `json:"settings"`
}

// Bound encodes rows, a namespace's settings at its creation, as its
// record's Bound holds them: every key the table classes bound at namespace
// scope with the value the rows give it, else its default, and no other
// key; a namespace section that declares no bound key encodes an empty set,
// never an absent one. Each value is encoded from what the decode gives,
// never from the row's bytes, so one set of values has one encoding
// whatever order or spelling the request used. A row no section reads is
// left out.
//
// decision: a create records every bound key, the ones its request sends
// and the others at their declared default, so a later release that changes
// a default cannot open a namespace, created before it, at the new value,
// which RFC 13 §5.3 refuses. RFC 13 states no encoding for the record, and
// the reader cannot import the API that writes it, so the codec is this
// package's, over the same decode that reads the rows. Overturned by an
// RFC 13 amendment that states one.
func (t Table) Bound(rows []entity.Setting) ([]byte, error) {
	var scope entity.SettingScope

	if len(rows) > 0 {
		scope = rows[0].Scope
	}

	x := t.index()

	of := make(map[*Section][]entity.Setting)
	schema := uint32(0)

	for _, row := range rows {
		schema = max(schema, row.Schema)

		sec, _, _, found := x.resolve(row.Key)
		if !found || sec.scope.form != formAt || sec.scope.wide != entity.ScopeNamespace {
			continue
		}

		of[sec] = append(of[sec], row)
	}

	set := make(map[string]json.RawMessage)

	for i := range t {
		sec := &t[i]
		if sec.scope.form != formAt || sec.scope.wide != entity.ScopeNamespace {
			continue
		}

		value, err := sec.decodeAny(sec, sec.def, scope, of[sec])
		if err != nil {
			return nil, err
		}

		for _, f := range sec.fields {
			if f.class != Bound {
				continue
			}

			b, err := json.Marshal(valueOf(value, f.index))
			if err != nil {
				return nil, err
			}

			set[f.key] = b
		}
	}

	return json.Marshal(bound{Schema: schema, Settings: set})
}

// DecodeBound decodes b, a Bound record as Bound writes it, into each bound
// key's encoded value. It refuses a schema this node does not read, a key it
// does not class bound at namespace scope and a value the decode refuses; it
// does not ask for every bound key, so a record a later release widened
// still decodes. Only Bound writes the record, marshalled from a map, so no
// key repeats in what this reads: a hand-made record's last value for one
// wins, and a writer of one has to guard that itself.
func (t Table) DecodeBound(b []byte) (map[string]json.RawMessage, error) {
	var rec bound
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("settings: a bound record is not one: %w", ErrInvalid)
	}

	if rec.Schema > maxSchema() {
		return nil, fmt.Errorf(
			"settings: bound record schema %d is newer than this node reads, up to %d: %w",
			rec.Schema, maxSchema(), ErrNewerSchema)
	}

	if rec.Settings == nil {
		return nil, fmt.Errorf("settings: a bound record holds no settings set: %w", ErrInvalid)
	}

	x := t.index()

	out := make(map[string]json.RawMessage, len(rec.Settings))

	for k, v := range rec.Settings {
		sec, _, _, found := x.resolve(k)
		if !found {
			return nil, fmt.Errorf(
				"settings: %s is not a key bound at a namespace's scope: %w",
				strconv.Quote(k), ErrInvalid)
		}

		f, declared := sec.fieldOf(k)
		if !declared ||
			sec.scope.form != formAt || sec.scope.wide != entity.ScopeNamespace ||
			f.class != Bound {
			return nil, fmt.Errorf(
				"settings: %s is not a key bound at a namespace's scope: %w",
				strconv.Quote(k), ErrInvalid)
		}

		row := entity.Setting{
			Scope: entity.SettingScope{Kind: entity.ScopeNamespace},
			Key:   k,
			Value: v,
		}

		value, err := sec.decodeAny(sec, sec.def, row.Scope, []entity.Setting{row})
		if err != nil {
			return nil, err
		}

		b, err := json.Marshal(valueOf(value, f.index))
		if err != nil {
			return nil, err
		}

		out[k] = b
	}

	return out, nil
}
