package settings

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// errNoHolds refuses a change of a bound setting no one can judge: the caller
// handed Check no holds, so nothing can tell whether the scope holds content,
// and a bound setting never fails open.
var errNoHolds = errors.New("settings: bound setting changed with no content check")

// gates are the refusals that protect stored data, run after every other
// gate, in order: the bound gate, the store location gate, then the block
// format version gate. Each runs only on the key it marks. The store is read
// through the write's transaction, one point read of the written key at each
// scope of its chain, so a concurrent write of a row a gate compared makes the
// write retry (RFC 13 §6).
func (sec *Section) gates(
	tx entity.ControlPlaneTx,
	w Write,
	declared, thing string,
	holds HoldsContent,
	effective any,
) error {
	f, found := sec.fieldOf(declared)
	if !found || (f.class != Bound && !f.store && sec.formatVersion != declared) {
		return nil
	}

	if f.class == Bound || f.store {
		before, had, err := sec.beforeOf(tx, w, f, thing)
		if err != nil {
			return err
		}

		after := valueOf(effective, f.index)

		if !reflect.DeepEqual(before, after) {
			// A store location's first value, the write that creates the
			// store, is no change: no row of the key at any scope of the
			// chain and no default, a field having no default exactly when
			// its section declares it required (RFC 13 §4.2).
			first := w.put != nil && !had && slices.Contains(sec.required, declared)

			if f.class == Bound {
				if holds == nil {
					return errNoHolds
				}

				has, err := holds(w.Scope)
				if err != nil {
					return err
				}

				if has {
					return refuse(w.Scope, w.Key, sec.gateRule(w, declared,
						"bound setting changed while its scope holds content",
						"RFC 13 §5.1"), ErrBound)
				}
			}

			// decision: a remote store's location changes only by a proof:
			// the store's capability check finds the namespace's claim at
			// the new location (RFC 13 §5.2). Neither the check nor the
			// claim is built, and a check without the claim would pass any
			// reachable bucket, so every change but a first value is refused
			// here. Withdraw it once both exist.
			if f.store && !first {
				return refuse(w.Scope, w.Key, sec.gateRule(w, declared,
					"store location change not provable yet",
					"RFC 13 §5.2"), ErrUnproven)
			}
		}
	}

	if w.put != nil && sec.formatVersion == declared {
		max := Versions()[entity.FormatBlock].Max

		if n := pastRange(reflect.ValueOf(effective).FieldByIndex(f.index), max); n != "" {
			return refuse(w.Scope, w.Key, fmt.Sprintf(
				"block format version %s is past this node's range, up to %d (RFC 13 §5.4)",
				n, max), ErrFormatVersion)
		}
	}

	return nil
}

// pastRange is the format version v holds, printed as a refusal prints it,
// when the version sits above the range max; "" when it does not.
func pastRange(v reflect.Value, max uint32) string {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() > int64(max) {
			return strconv.FormatInt(v.Int(), 10)
		}
	default:
		if v.Uint() > uint64(max) {
			return strconv.FormatUint(v.Uint(), 10)
		}
	}

	return ""
}

// beforeOf reads the field as it stands before w, through tx: the section's
// default, then each scope of the chain overlaid with the row stored there,
// widest first, as after builds the value once w is applied, so the two are
// compared like for like; had reports whether any scope of the chain stores a
// row of the key. Each row is read as the decode reads a stored one, so a
// value is compared decoded, never as bytes, and a row of a schema this
// binary does not read refuses the write whole.
func (sec *Section) beforeOf(
	tx entity.ControlPlaneTx,
	w Write,
	f field,
	thing string,
) (any, bool, error) {
	value := sec.def
	had := false

	for _, scope := range sec.chain(w.Scope) {
		stored, found, err := tx.Setting(scope, w.Key)
		if err != nil {
			return nil, false, fmt.Errorf(
				"settings: read %s at %s: %w", w.Key, scope.Kind, err)
		}

		if !found {
			continue
		}

		had = true

		if stored.Schema > maxSchema() {
			return nil, had, newer(scope, w.Key, stored.Schema)
		}

		stored.Key = f.key

		value, err = sec.decodeAny(sec, value, scope, []entity.Setting{stored})
		if err != nil {
			return nil, had, sec.inThing(err, thing)
		}
	}

	return valueOf(value, f.index), had, nil
}

// fieldOf is the section's field declared as key, or false when the section
// declares none such.
func (sec *Section) fieldOf(key string) (field, bool) {
	for _, f := range sec.fields {
		if f.key == key {
			return f, true
		}
	}

	return field{}, false
}

// valueOf is the Go field at index of a decoded section value, the value a
// gate compares.
func valueOf(value any, index []int) any {
	return reflect.ValueOf(value).FieldByIndex(index).Interface()
}

// gateRule words a gate's refusal: the rule, the value a put writes beside
// it when one is printed, never the value of a reference field since its put
// is refused before it could be printed, then the RFC section the rule comes
// from.
func (sec *Section) gateRule(w Write, declared, rule, rfc string) string {
	if w.put != nil {
		if _, ref := sec.secretRefs[declared]; !ref {
			rule += ", got " + putPrinted(w.put.Value)
		}
	}

	return rule + " (" + rfc + ")"
}

// putPrinted prints a put's value as a decode refusal prints the value it got:
// a string quoted, any other value compact, so the value never spans lines.
func putPrinted(raw []byte) string {
	p := jsonParser{
		b: raw,
	}

	j, f := p.value("")
	if f != nil {
		return printed(string(raw))
	}

	return j.got()
}
