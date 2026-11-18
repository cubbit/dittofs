package entity

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// An owner is equal to another only when every part names the same holder,
// the record ID compared by its bytes, so a reference cannot be moved to a
// holder that differs in one part alone.
func TestSecretOwnerEqual(t *testing.T) {
	prof.Watch(t, prof.Options{})

	user := SecretOwner{
		Record: ResourceRef{Kind: ResourceUser, ID: []byte{1, 2, 3, 4}},
		Key:    "password",
	}
	setting := SecretOwner{
		Scope: SettingScope{Kind: ScopeShare, ID: [16]byte{9}},
		Key:   "identity.keytab",
	}

	otherKind := SettingScope{Kind: ScopeNode, ID: [16]byte{9}}
	otherID := SettingScope{Kind: ScopeShare, ID: [16]byte{8}}

	for _, c := range []struct {
		name string
		a, b SecretOwner
		want bool
	}{
		{"the same record owner, its ID copied", user, withID(user, []byte{1, 2, 3, 4}), true},
		{"the same setting owner", setting, setting, true},
		{"a nil and an empty record ID", withID(user, nil), withID(user, []byte{}), true},
		{"another record kind", user, withKind(user, ResourceGroup), false},
		{"a record ID differing in one byte", user, withID(user, []byte{1, 2, 3, 5}), false},
		{"a longer record ID", user, withID(user, []byte{1, 2, 3, 4, 0}), false},
		{"another scope kind", setting, withScope(setting, otherKind), false},
		{"another scope ID", setting, withScope(setting, otherID), false},
		{"another key", user, withKey(user, "nt_hash"), false},
	} {
		if got := c.a.Equal(c.b); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", c.name, got, c.want)
		}

		if got := c.b.Equal(c.a); got != c.want {
			t.Errorf("%s, reversed: Equal = %v, want %v", c.name, got, c.want)
		}
	}
}

func withID(o SecretOwner, id []byte) SecretOwner {
	o.Record.ID = id

	return o
}

func withKind(o SecretOwner, k ResourceKind) SecretOwner {
	o.Record.Kind = k

	return o
}

func withScope(o SecretOwner, s SettingScope) SecretOwner {
	o.Scope = s

	return o
}

func withKey(o SecretOwner, k string) SecretOwner {
	o.Key = k

	return o
}
