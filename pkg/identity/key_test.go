package identity_test

import (
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

var (
	timeType = reflect.TypeFor[time.Time]()
	addrType = reflect.TypeFor[netip.Addr]()
)

// leafValues returns non-zero values of the leaf type t, each differing from
// the one before, or false for a type the walk does not know how to vary. A
// time has two, so its seconds and its nanoseconds are each varied.
func leafValues(t reflect.Type) ([]reflect.Value, bool) {
	v := reflect.New(t).Elem()
	switch {
	case t == timeType:
		return []reflect.Value{reflect.ValueOf(time.Unix(1, 0)), reflect.ValueOf(time.Unix(1, 1))}, true
	case t == addrType:
		v.Set(reflect.ValueOf(netip.MustParseAddr("192.0.2.1")))
	case t.Kind() == reflect.Bool:
		v.SetBool(true)
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		v.SetInt(1)
	case t.Kind() >= reflect.Uint && t.Kind() <= reflect.Uintptr:
		v.SetUint(1)
	case t.Kind() == reflect.String:
		v.SetString("x")
	default:
		return nil, false
	}
	return []reflect.Value{v}, true
}

// keyOf is the key of the Identity root points to.
func keyOf(root reflect.Value) identity.ProofKey { return root.Interface().(identity.Identity).Key() }

// checkMoves walks every field reachable from t, which at locates inside a
// zero Identity, recursing into structs, slices and arrays and taking a
// time.Time and a netip.Addr as leaves. For each leaf it builds an Identity
// with the containers on the way to it allocated, sets the leaf to each of its
// values in turn and fails unless every step moves Key; for each slice it
// fails unless growing it from empty to one zero element moves Key.
func checkMoves(t *testing.T, ty reflect.Type, name string, at func(root reflect.Value) reflect.Value) {
	t.Helper()
	moves := func(what string, changes ...func(loc reflect.Value)) {
		root := reflect.New(reflect.TypeFor[identity.Identity]()).Elem()
		loc := at(root)
		before := keyOf(root)
		for _, change := range changes {
			change(loc)
			if keyOf(root) == before {
				t.Errorf("Key() unchanged when %s %s, to %v, want every field but Client in the key", name, what, loc)
			}
			before = keyOf(root)
		}
	}
	if ty == timeType || ty == addrType || (ty.Kind() != reflect.Struct && ty.Kind() != reflect.Slice && ty.Kind() != reflect.Array) {
		vals, ok := leafValues(ty)
		if !ok {
			t.Errorf("%s is a %s the test cannot vary, want it and Key() taught about it", name, ty)
			return
		}
		var changes []func(reflect.Value)
		for _, v := range vals {
			changes = append(changes, func(loc reflect.Value) { loc.Set(v) })
		}
		moves("is set", changes...)
		return
	}
	switch ty.Kind() {
	case reflect.Struct:
		for i := range ty.NumField() {
			f := ty.Field(i)
			if !f.IsExported() {
				t.Errorf("%s.%s is unexported, so the test cannot set it; want Key() and this test to cover it", name, f.Name)
				continue
			}
			checkMoves(t, f.Type, name+"."+f.Name, func(r reflect.Value) reflect.Value { return at(r).Field(i) })
		}
	case reflect.Slice:
		moves("grows from empty", func(loc reflect.Value) { loc.Set(reflect.MakeSlice(ty, 1, 1)) })
		checkMoves(t, ty.Elem(), name+"[0]", func(r reflect.Value) reflect.Value {
			s := at(r)
			if s.Len() == 0 {
				s.Set(reflect.MakeSlice(ty, 1, 1))
			}
			return s.Index(0)
		})
	case reflect.Array:
		for i := range ty.Len() {
			checkMoves(t, ty.Elem(), name+"["+strconv.Itoa(i)+"]", func(r reflect.Value) reflect.Value { return at(r).Index(i) })
		}
	}
}

// Changing any field of an Identity but Client, at any depth, moves Key();
// changing Client does not. A field added to Identity or Evidence fails here
// until Key() writes it.
func TestKeyCoversEveryFieldButClient(t *testing.T) {
	ty := reflect.TypeFor[identity.Identity]()
	for i := range ty.NumField() {
		f := ty.Field(i)
		if f.Name == "Client" {
			continue
		}
		checkMoves(t, f.Type, "Identity."+f.Name, func(r reflect.Value) reflect.Value { return r.Field(i) })
	}

	id := identity.Identity{Proof: identity.ProofSecret, Evidence: identity.Evidence{Name: "alice"}}
	other := id
	other.Client = netip.MustParseAddr("192.0.2.1")
	if id.Key() != other.Key() {
		t.Error("Key() moved with Client, want the client's address left out of the proof key")
	}
}

var (
	alice = entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{1}}
	sidA  = identity.SID{Revision: 1, SubAuthorityCount: 2, IdentifierAuthority: [6]byte{5: 5}, SubAuthorities: []uint32{21, 1001}}
	sidB  = identity.SID{Revision: 1, SubAuthorityCount: 2, IdentifierAuthority: [6]byte{5: 5}, SubAuthorities: []uint32{21, 513}}
)

// The zero Identity and a None identity, whose evidence holds the zero
// principal, each have a key, and neither is a Ticket identity's.
func TestKeyOfZeroAndNone(t *testing.T) {
	ticket := identity.Identity{Proof: identity.ProofTicket, Evidence: identity.Evidence{Name: "alice@CORP.EXAMPLE", Principal: alice}}.Key()
	cases := map[string]identity.Identity{
		"zero": {},
		"None": {Proof: identity.ProofNone, Protection: identity.ProtectionIntegrity, Client: netip.MustParseAddr("192.0.2.1")},
	}
	for name, id := range cases {
		k := id.Key()
		if k == (identity.ProofKey{}) {
			t.Errorf("%s identity's Key() = the zero key, want a digest", name)
		}
		if k == ticket {
			t.Errorf("%s identity's Key() = a Ticket identity's, want them apart", name)
		}
	}
}

// The deny-only bit and list boundaries are keyed: moving a deny-only SID into
// SIDs moves Key(), as does moving one between the two lists' ends.
func TestKeyKeepsDenyOnly(t *testing.T) {
	base := identity.Identity{Proof: identity.ProofTicket, Evidence: identity.Evidence{Principal: alice}}
	denied := base
	denied.Evidence.SIDs = []identity.SID{sidA}
	denied.Evidence.DenyOnly = []identity.SID{sidB}
	allowed := base
	allowed.Evidence.SIDs = []identity.SID{sidA, sidB}
	if denied.Key() == allowed.Key() {
		t.Error("Key() of SIDs [A] with deny-only [B] = Key() of SIDs [A B], want the deny-only bit keyed")
	}
}

// Two identities of one principal, one proved by a secret and one by a
// ticket, have different keys.
func TestKeySeparatesProofKinds(t *testing.T) {
	ev := identity.Evidence{Name: "alice", Principal: alice, SIDs: []identity.SID{sidA}}
	secret := identity.Identity{Proof: identity.ProofSecret, Evidence: ev}
	ticket := identity.Identity{Proof: identity.ProofTicket, Evidence: ev}
	if secret.Key() == ticket.Key() {
		t.Error("Key() of a Secret identity = a Ticket identity's of one principal, want them apart")
	}
}

// Each list's length is keyed, so values cannot slide across a field boundary:
// gids [0 9] and the name "x" encode, without the list's length, as the same
// bytes as no gids and a nine-byte name starting with the first name's length.
func TestKeyKeepsListBoundaries(t *testing.T) {
	a := identity.Identity{Claims: identity.Claims{GIDs: []uint32{0, 9}}, Evidence: identity.Evidence{Name: "x"}}
	b := identity.Identity{Evidence: identity.Evidence{Name: "\x00\x00\x00\x00\x00\x00\x00\x01x"}}
	if a.Key() == b.Key() {
		t.Error("Key() of gids [0 9] and name \"x\" = Key() of no gids and a name holding their bytes, want each list's length keyed")
	}
}
