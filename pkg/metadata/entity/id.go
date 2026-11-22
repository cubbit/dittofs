// Package entity holds the store's entities: plain values with pure methods
// only. Nothing here does I/O or holds a store; a rule that needs the store
// belongs on an interface over it.
package entity

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// FileID, ShareID and PrincipalID are minted at random and never reused.
type (
	FileID      [16]byte
	ShareID     [16]byte
	PrincipalID [16]byte
)

// NodeID names a node; minted at random, never reused.
type NodeID [16]byte

// OperationID names a management operation; minted at random, never reused.
type OperationID [16]byte

// JournalDir is a journal device: the directory the node's bootstrap names
// for it, as written there. The store keys a device by these bytes and never
// normalises them, so the bootstrap, which builds the paths, names each
// device one way.
type JournalDir string

// ErrSyntax is text that does not parse as the type asked for.
var ErrSyntax = errors.New("entity: invalid syntax")

func newID() [16]byte { return uuid.New() }

func parseID(s string) ([16]byte, error) {
	id, err := uuid.Parse(s)
	if err != nil || len(s) != 36 {
		return [16]byte{}, ErrSyntax
	}
	return id, nil
}

func NewFileID() FileID           { return newID() }
func NewShareID() ShareID         { return newID() }
func NewPrincipalID() PrincipalID { return newID() }

func ParseFileID(s string) (FileID, error) {
	id, err := parseID(s)
	return id, err
}

func ParseShareID(s string) (ShareID, error) {
	id, err := parseID(s)
	return id, err
}

func ParsePrincipalID(s string) (PrincipalID, error) {
	id, err := parseID(s)
	return id, err
}

// String is the canonical UUID form.
func (id FileID) String() string      { return uuid.UUID(id).String() }
func (id ShareID) String() string     { return uuid.UUID(id).String() }
func (id PrincipalID) String() string { return uuid.UUID(id).String() }

func (id FileID) IsZero() bool      { return id == FileID{} }
func (id ShareID) IsZero() bool     { return id == ShareID{} }
func (id PrincipalID) IsZero() bool { return id == PrincipalID{} }

func (id FileID) MarshalText() ([]byte, error)      { return []byte(id.String()), nil }
func (id ShareID) MarshalText() ([]byte, error)     { return []byte(id.String()), nil }
func (id PrincipalID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *FileID) UnmarshalText(b []byte) (err error) {
	*id, err = ParseFileID(string(b))
	return err
}

func (id *ShareID) UnmarshalText(b []byte) (err error) {
	*id, err = ParseShareID(string(b))
	return err
}

func (id *PrincipalID) UnmarshalText(b []byte) (err error) {
	*id, err = ParsePrincipalID(string(b))
	return err
}

// Principal is a protocol-neutral identity: a user, a group, a machine, or
// one of the fixed well-known specials. Its ID is minted when the principal is created
// and never reissued, so an account reusing a UID or SID cannot inherit an
// old principal's files or grants. Protocol spellings live only on the User
// or Group record and in the protocol-ID index; nothing here interprets one.
type Principal struct {
	Kind PrincipalKind
	ID   PrincipalID
}

type PrincipalKind uint8

const (
	PrincipalUser PrincipalKind = iota + 1
	PrincipalGroup
	PrincipalSpecial
	// PrincipalMachine is a host or service signing in for itself, held as a
	// User record with no name and only its Kerberos spelling.
	PrincipalMachine
)

var principalKinds = []string{PrincipalUser: "user", PrincipalGroup: "group", PrincipalSpecial: "special", PrincipalMachine: "machine"}

func (k PrincipalKind) String() string                { return enumString(principalKinds, k) }
func (k PrincipalKind) MarshalText() ([]byte, error)  { return enumMarshal(principalKinds, k) }
func (k *PrincipalKind) UnmarshalText(b []byte) error { return enumUnmarshal(principalKinds, k, b) }

// The well-known principals an ACL entry can name, each with a fixed ID:
// fifteen zero bytes, then its number. No minted ID takes that form, since a
// minted ID is a random UUID with its version bits set. Which caller each
// matches is the access check's rule.
var (
	Owner              = Principal{PrincipalSpecial, PrincipalID{15: 0x01}} // OWNER@: the file's owner
	OwnerGroup         = Principal{PrincipalSpecial, PrincipalID{15: 0x02}} // GROUP@: members of the file's group
	Everyone           = Principal{PrincipalSpecial, PrincipalID{15: 0x03}} // EVERYONE@, Everyone
	OwnerRights        = Principal{PrincipalSpecial, PrincipalID{15: 0x04}} // the owner, limiting what ownership grants
	CreatorOwner       = Principal{PrincipalSpecial, PrincipalID{15: 0x05}} // an inheritable placeholder, replaced at create
	CreatorGroup       = Principal{PrincipalSpecial, PrincipalID{15: 0x06}}
	AuthenticatedUsers = Principal{PrincipalSpecial, PrincipalID{15: 0x07}}
	Administrators     = Principal{PrincipalSpecial, PrincipalID{15: 0x08}}
	System             = Principal{PrincipalSpecial, PrincipalID{15: 0x09}}
	Network            = Principal{PrincipalSpecial, PrincipalID{15: 0x0A}}
	BuiltinUsers       = Principal{PrincipalSpecial, PrincipalID{15: 0x0B}}
	// AnonymousPrincipal is what a caller proving nothing, or one a share
	// squashes, acts as where the share's policy names no other: matched by
	// a caller whose principal it is, never authenticated, admin or
	// privileged.
	AnonymousPrincipal = Principal{PrincipalSpecial, PrincipalID{15: 0x0C}}
	// RootPrincipal is what a claimed uid 0 maps to: privileged where a
	// share leaves it unsquashed, and the owner of what it creates there.
	// Resolution treats a proven caller bound to it as root, but no sign-in
	// binds one yet, and a claimed uid 0 is not mapped to it: the ponytail:
	// in identity.Resolve says what a claimed uid 0 gets meanwhile. No ACL
	// check matches it, so ACL.Valid refuses a deny naming it.
	//
	// decision: uid 0 is a well-known principal, root, not a user an operator
	// creates. Without it a claimed uid 0 maps to no principal and acts as
	// anonymous, so a share that squashes nothing still squashes root:
	// chown, tar xpf and restores fail, and root's files are owned by 65534.
	// The cost is that root is one principal across every client host.
	// Overturned if a deployment needs root on one host told apart from root
	// on another; that is a per-host link, not a second rule.
	RootPrincipal = Principal{PrincipalSpecial, PrincipalID{15: 0x0D}}
)

// WellKnown reports whether p is one of the well-known principals above.
func (p Principal) WellKnown() bool {
	var prefix [15]byte
	return p.Kind == PrincipalSpecial && [15]byte(p.ID[:15]) == prefix && p.ID[15] >= 0x01 && p.ID[15] <= 0x0D
}

// IsCreator reports CREATOR OWNER or CREATOR GROUP, which act only through
// inheritance.
func (p Principal) IsCreator() bool { return p == CreatorOwner || p == CreatorGroup }

func NewUser() Principal  { return Principal{PrincipalUser, NewPrincipalID()} }
func NewGroup() Principal { return Principal{PrincipalGroup, NewPrincipalID()} }

func (p Principal) IsZero() bool { return p == Principal{} }

// String is "kind:id", such as "user:7c1e…": never a UID or SID.
func (p Principal) String() string { return p.Kind.String() + ":" + p.ID.String() }

func ParsePrincipal(s string) (Principal, error) {
	kind, id, ok := strings.Cut(s, ":")
	if !ok {
		return Principal{}, ErrSyntax
	}
	var p Principal
	if err := p.Kind.UnmarshalText([]byte(kind)); err != nil {
		return Principal{}, err
	}
	var err error
	if p.ID, err = ParsePrincipalID(id); err != nil {
		return Principal{}, err
	}
	return p, nil
}

func (p Principal) MarshalText() ([]byte, error) {
	if p.Kind.String() == "" {
		return nil, ErrSyntax
	}
	return []byte(p.String()), nil
}

func (p *Principal) UnmarshalText(b []byte) (err error) {
	*p, err = ParsePrincipal(string(b))
	return err
}

// enumString names v by its index in names; an unnamed value prints empty
// rather than as a number, so a dump never shows an unknown code as a word.
func enumString[E ~uint8](names []string, v E) string {
	if int(v) < len(names) {
		return names[v]
	}
	return ""
}

func enumMarshal[E ~uint8](names []string, v E) ([]byte, error) {
	if s := enumString(names, v); s != "" {
		return []byte(s), nil
	}
	return nil, ErrSyntax
}

func enumUnmarshal[E ~uint8](names []string, v *E, b []byte) error {
	for i, n := range names {
		if n != "" && n == string(b) {
			*v = E(i)
			return nil
		}
	}
	return ErrSyntax
}
