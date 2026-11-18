package identity_test

import (
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// keyFields names every field reachable from t, through structs, pointers,
// slices, arrays and maps, that could hold a key: one of type Keys, of a
// byte-slice or byte-array type, or an interface, which can hold either. A
// SID's and a principal's bytes are identifiers, not keys, so neither is
// walked into, and nor is an EvidenceDigest.
func keyFields(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	keys := reflect.TypeFor[identity.Keys]()
	switch {
	case t == keys:
		return []string{path + " is a Keys"}
	case t == reflect.TypeFor[identity.SID](), t == reflect.TypeFor[entity.Principal]():
		return nil
	// decision: EvidenceDigest is the one byte array let through, by its type
	// and not by its shape, because it is a hash of what sign-in established
	// rather than key material: Evidence carries it so that the proof key can be
	// computed once. Any other [32]byte still fails. Withdraw the exemption if
	// the digest is ever keyed by a secret, such as an HMAC under a session key.
	case t == reflect.TypeFor[identity.EvidenceDigest]():
		return nil
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return []string{path + " is a byte slice"}
	case t.Kind() == reflect.Array && t.Elem().Kind() == reflect.Uint8:
		return []string{path + " is a byte array"}
	case t.Kind() == reflect.Interface:
		return []string{path + " is an interface"}
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return keyFields(t.Elem(), path+"[]", seen)
	case reflect.Map:
		return append(keyFields(t.Key(), path+"{key}", seen), keyFields(t.Elem(), path+"{}", seen)...)
	case reflect.Struct:
		if seen[t] {
			return nil
		}
		seen[t] = true
		var found []string
		for i := range t.NumField() {
			f := t.Field(i)
			found = append(found, keyFields(f.Type, path+"."+f.Name, seen)...)
		}
		return found
	}
	return nil
}

// An Identity is copied into every request and cached on its proof, so it
// carries no key: no field of it, at any depth, is a Keys, a byte slice, a
// byte array or an interface.
func TestIdentityHoldsNoKey(t *testing.T) {
	for _, f := range keyFields(reflect.TypeFor[identity.Identity](), "Identity", map[reflect.Type]bool{}) {
		t.Errorf("%s, want no key in an Identity", f)
	}
}

// The walk reports a key at any depth, so the test above can fail, and lets
// an EvidenceDigest through while a [32]byte of the same shape still fails.
func TestKeyFieldsFindsAKey(t *testing.T) {
	type nested struct{ Session []byte }
	type withKey struct {
		Evidence       struct{ K *identity.Keys }
		Deep           []nested
		SessionBaseKey [16]byte
		Extra          any
		Digest         identity.EvidenceDigest
		NotADigest     [32]byte
	}
	got := keyFields(reflect.TypeFor[withKey](), "T", map[reflect.Type]bool{})
	want := []string{"T.Evidence.K[] is a Keys", "T.Deep[].Session is a byte slice", "T.SessionBaseKey is a byte array", "T.Extra is an interface", "T.NotADigest is a byte array"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keyFields = %q, want %q", got, want)
	}
}
