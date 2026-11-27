package settings

import (
	"fmt"
	"reflect"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// checkSecretRefs judges the references the write makes effective: for every
// field a SecretRef option declares that a row of the section's chain sets,
// the Secret its reference names is point-read through tx, tracked, and the
// write is refused when the reference names no secret, one of another kind
// than the field declares, or one owned by another record or setting, the
// owner the sealing routes derive from the record they write (RFC 16 §2.3).
// It runs before Validate, so a plaintext typed into a reference never
// reaches a Validate that prints it, and every refusal names the field
// judged, never the reference or the value.
func (sec *Section) checkSecretRefs(
	tx entity.ControlPlaneTx,
	effective any,
	thing string,
	holders map[string]entity.Setting,
) error {
	for _, f := range sec.fields {
		kind, named := sec.secretRefs[f.key]
		if !named {
			continue
		}

		row, held := holders[f.key]
		if !held {
			continue
		}

		key := sec.written(f.key, thing)

		// decision: a field whose effective value is not a ref, as when a
		// section declared a string field as its reference, names no secret:
		// the write is refused closed, never passed on a value that might
		// have been a reference under the section the field once had. The
		// value itself never reaches the refusal.
		ref, ok := refOf(effective, f.index)
		if !ok || ref == (entity.SecretRef{}) {
			return refuse(row.Scope, key, "names no secret of kind "+kind.String(), ErrSecretRef)
		}

		s, found, err := tx.Secret(ref, 0)
		if err != nil {
			return fmt.Errorf("settings: read the secret %s names: %w", key, err)
		}

		if !found {
			return refuse(row.Scope, key, "names no secret of kind "+kind.String(), ErrSecretRef)
		}

		if s.Kind != kind {
			return refuse(row.Scope, key,
				"names a secret of another kind: "+s.Kind.String()+", not "+kind.String(),
				ErrSecretRef)
		}

		// The owner is the row that holds the reference, scope and key both,
		// so a stored reference stays judged where it lives even when a
		// delete leaves a wider row effective. The other owner is never
		// named: a record's ID is no operator's business.
		want := entity.SecretOwner{Scope: row.Scope, Key: row.Key}
		if !s.Owner.Equal(want) {
			return refuse(row.Scope, key, "names a secret of another owner", ErrSecretRef)
		}
	}

	return nil
}

// refOf reads effective's field at index, a ref only when it holds one.
func refOf(effective any, index []int) (entity.SecretRef, bool) {
	v := reflect.ValueOf(effective)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}

	ref, ok := v.FieldByIndex(index).Interface().(entity.SecretRef)

	return ref, ok
}
