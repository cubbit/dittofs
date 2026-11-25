package identity

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// defaultNFS4Domain is the owner strings' domain with no setting and no
// keytab.
const defaultNFS4Domain = "localdomain"

// nfs4Domain is the owner strings' domain: set when it is not empty, else the
// first realm of the keytab's current version with ASCII letters lowered, else
// localdomain.
func nfs4Domain(set string, keytab []secrets.Version) string {
	if set != "" {
		return set
	}
	if len(keytab) > 0 {
		if names, err := identity.KeytabNames(keytab[0].Value); err == nil && len(names) > 0 && names[0].Realm != "" {
			return foldASCII(names[0].Realm)
		}
	}
	return defaultNFS4Domain
}

// ownerForm is what parseOwner read an owner string as.
type ownerForm uint8

const (
	ownerNeither ownerForm = iota
	ownerName              // name@domain
	ownerNumber            // a canonical decimal uid or gid
)

// maxOwnerDigits is the length of 4294967295, the largest uid or gid.
const maxOwnerDigits = 10

// parseOwner reads an owner string as a name and the domain it was written
// under, as a number, or as neither. A string with an '@' splits at its last
// one, which no domain holds: an empty name or domain is neither, and the
// name is always a name, so 1500@corp.example names the user 1500. A string
// with none is a number only in canonical decimal, digits with no leading
// zero but 0 itself, at most 4294967295. So name+"@"+domain, or the number in
// decimal, is s again. It allocates nothing.
func parseOwner(s string) (name, domain string, num uint32, form ownerForm) {
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		if i == 0 || i == len(s)-1 {
			return "", "", 0, ownerNeither
		}
		return s[:i], s[i+1:], 0, ownerName
	}
	if s == "" || len(s) > maxOwnerDigits {
		return "", "", 0, ownerNeither
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return "", "", 0, ownerNeither
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	var b [maxOwnerDigits]byte
	if err != nil || string(strconv.AppendUint(b[:0], n, 10)) != s {
		return "", "", 0, ownerNeither
	}
	return "", "", uint32(n), ownerNumber
}

// ownerIDScheme is the store scheme of the number an owner scheme spells: the
// uid for owner, the gid for owner_group.
func ownerIDScheme(scheme identity.Scheme) identity.Scheme {
	if scheme == identity.SchemeOwnerGroup {
		return identity.SchemeGID
	}
	return identity.SchemeUID
}

// spellOwner spells a stored principal in an owner scheme: owner for a user,
// owner_group for a group, as name@domain when it has a name, else as its
// decimal uid or gid, else ErrUnknownPrincipal.
//
// decision: the owner schemes always spell the proven form. Spell acts for no
// caller and cannot see the call's proof, so the adapter picks the scheme by
// the proof it reports: an owner scheme on a call with proof Secret or above,
// the uid or gid scheme on a Claimed or None call, whose client already names
// users by number and whose idmapper may know none of the server's names.
// Overturned if a scheme ever depends on more than the proof kind.
func (s *Server) spellOwner(p entity.Principal, scheme identity.Scheme, name string, ids entity.ProtocolIDs) (identity.Spelling, error) {
	if (scheme == identity.SchemeOwnerGroup) != (p.Kind == entity.PrincipalGroup) {
		return identity.Spelling{}, vfserr.ErrUnknownPrincipal
	}
	if name != "" {
		st := s.state.Load()
		if st == nil {
			return identity.Spelling{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
		}
		return identity.Spelling{Scheme: scheme, Text: name + "@" + st.domain}, nil
	}
	for _, id := range ids {
		if id.Scheme == stored(ownerIDScheme(scheme)) {
			return identity.Spelling{Scheme: scheme, Text: id.Value}, nil
		}
	}
	return identity.Spelling{}, vfserr.ErrUnknownPrincipal
}

// findOwner finds the principal an owner string names: a user in owner, a
// group in owner_group. A name maps by name when its domain is the applied
// one, ASCII letters folded; a number through the uid or gid index; anything
// else is ErrUnknownPrincipal.
//
// ponytail: a name is found in the store only, so a chown to a directory user
// never seen before is ErrUnknownPrincipal. Ask the directory under
// FindOpts.Mint, through findDirectoryName as findQualified does, once an
// NFSv4 client is shown setting owners of unminted directory users.
func (s *Server) findOwner(ctx context.Context, sp identity.Spelling) (entity.Principal, error) {
	name, domain, num, form := parseOwner(sp.Text)
	switch form {
	case ownerNumber:
		return s.find(ctx, identity.Spelling{Scheme: ownerIDScheme(sp.Scheme), Num: num}, identity.FindOpts{})
	case ownerName:
		st := s.state.Load()
		if st == nil {
			return entity.Principal{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
		}
		if !asciiEqualFold(domain, st.domain) {
			return entity.Principal{}, vfserr.ErrUnknownPrincipal
		}
		pr := s.deps.Store.Principals()
		if sp.Scheme == identity.SchemeOwnerGroup {
			return found(pr.ByName(ctx, entity.PrincipalGroup, name))
		}
		p, err := found(pr.ByName(ctx, entity.PrincipalUser, name))
		return s.fileUser(ctx, p, err)
	}
	return entity.Principal{}, vfserr.ErrUnknownPrincipal
}
