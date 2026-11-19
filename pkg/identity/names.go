package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Identities maps principals to spellings and back. It acts on no share and
// is not admitted; vfs.Deps.Names holds it. A lookup error other than "not
// found" is never vfserr.ErrUnknownPrincipal, so a store outage is never read
// by a client as a principal no one holds.
type Identities interface {
	// Spell returns p's spelling in scheme, or vfserr.ErrUnknownPrincipal.
	Spell(ctx context.Context, p entity.Principal, scheme Scheme) (Spelling, error)
	// Find returns the principal holding s, or vfserr.ErrUnknownPrincipal.
	Find(ctx context.Context, s Spelling, o FindOpts) (entity.Principal, error)
}

// FindOpts: Mint lets a lookup the directory answers mint the directory
// principal. The filesystem sets it only for a call whose Identity is
// ProofSecret or above.
type FindOpts struct{ Mint bool }

// Scheme names how a protocol spells a principal.
type Scheme uint8

const (
	SchemeUID Scheme = iota + 1
	SchemeGID
	SchemeSID
	SchemeKrb
	SchemeDir
	SchemeUserName
	SchemeGroupName
	SchemeOIDC
	SchemeOwner // an NFSv4 owner string
	SchemeQualified
)

// Stored reports whether s has index rows: Find reads only these, and Spell
// derives the others.
//
// ponytail: oidc is stored, but it is left out here, and out of the store's
// schemes, until OpenID Connect sign-in lands and writes its rows.
func (s Scheme) Stored() bool {
	switch s {
	case SchemeUID, SchemeGID, SchemeSID, SchemeKrb, SchemeDir, SchemeUserName, SchemeGroupName:
		return true
	}
	return false
}

// Spelling is one protocol's name for a principal: Num, SID or Text is set,
// by Scheme.
type Spelling struct {
	Scheme Scheme
	Num    uint32
	SID    SID
	Text   string
}
