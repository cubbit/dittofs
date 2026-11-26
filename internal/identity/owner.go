package identity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/auth/sid"
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

// maxSIDText is the length of the longest SID string: revision 1, a 48-bit
// authority and fifteen sub-authorities of ten digits each.
const maxSIDText = len("S-1-281474976710655") + 15*len("-4294967295")

// nullSID is the SID that names nobody, which a who spells for a principal no
// record holds.
const nullSID = "S-1-0-0"

// parseSID reads s as a SID in its canonical string form: S-1-, a 48-bit
// authority, then at most fifteen 32-bit sub-authorities, every number in
// canonical decimal. So sid.FormatSID of what it reads is s again. Any other
// string is not a SID, and is refused before anything is allocated.
func parseSID(s string) (*sid.SID, bool) {
	rest, ok := strings.CutPrefix(s, "S-1-")
	if !ok || len(s) > maxSIDText {
		return nil, false
	}
	for n := 0; ; n++ {
		field, tail, more := strings.Cut(rest, "-")
		limit := uint64(1<<32 - 1)
		if n == 0 {
			limit = 1<<48 - 1
		}
		if !canonicalDecimal(field, limit) || n > 15 {
			return nil, false
		}
		if !more {
			break
		}
		rest = tail
	}
	d, err := sid.ParseSIDString(s)
	return d, err == nil
}

// canonicalDecimal reports whether s is a decimal number at most limit, with
// no sign and no leading zero but 0 itself.
func canonicalDecimal(s string, limit uint64) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	var n uint64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' || n > (limit-uint64(c-'0'))/10 {
			return false
		}
		n = n*10 + uint64(c-'0')
	}
	return true
}

// whoSID reads a who as a well-formed SID and the domain it was written
// under, S-…@domain; any other who is not one.
func whoSID(s string) (*sid.SID, string, bool) {
	name, domain, _, form := parseOwner(s)
	if form != ownerName {
		return nil, "", false
	}
	d, ok := parseSID(name)
	return d, domain, ok
}

func isWho(scheme identity.Scheme) bool {
	return scheme == identity.SchemeWho || scheme == identity.SchemeWhoGroup
}

// whoOwner is the owner scheme a who scheme spells and finds names in.
func whoOwner(scheme identity.Scheme) identity.Scheme {
	if scheme == identity.SchemeWhoGroup {
		return identity.SchemeOwnerGroup
	}
	return identity.SchemeOwner
}

// whoKind reports whether a who scheme holds p: who a user or a machine,
// who_group a group, and either a well-known principal, which is neither.
func whoKind(p entity.Principal, scheme identity.Scheme) bool {
	if p.Kind == entity.PrincipalSpecial {
		return true
	}
	return (scheme == identity.SchemeWhoGroup) == (p.Kind == entity.PrincipalGroup)
}

// atDomain spells text at the applied State's NFSv4 domain.
func (s *Server) atDomain(scheme identity.Scheme, text string) (identity.Spelling, error) {
	st := s.state.Load()
	if st == nil {
		return identity.Spelling{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	return identity.Spelling{Scheme: scheme, Text: text + "@" + st.domain}, nil
}

// spellUnheld answers Spell for a principal no record holds: the null SID at
// the domain in a who scheme, err in any other or when err is not
// ErrUnknownPrincipal.
func (s *Server) spellUnheld(scheme identity.Scheme, err error) (identity.Spelling, error) {
	if isWho(scheme) && errors.Is(err, vfserr.ErrUnknownPrincipal) {
		return s.atDomain(scheme, nullSID)
	}
	return identity.Spelling{}, err
}

// spellWho spells a stored principal in a who scheme as the owner scheme
// does. A name that is itself a SID string is passed over for the number,
// since findWho would read it back as that SID. No stored principal is spelled
// by its SID: the store refuses a user or group holding a SID without a uid or
// gid, so each has an owner string; only a well-known principal is spelled by
// its SID.
func (s *Server) spellWho(p entity.Principal, scheme identity.Scheme, name string, ids entity.ProtocolIDs) (identity.Spelling, error) {
	if _, ok := parseSID(name); ok {
		name = ""
	}
	sp, err := s.spellOwner(p, whoOwner(scheme), name, ids)
	if err != nil {
		return identity.Spelling{}, err
	}
	sp.Scheme = scheme
	return sp, nil
}

// spellWhoWellKnown spells a well-known principal in a who scheme: root and
// anonymous by their reserved numbers, any other by its SID at the domain.
// OWNER@ and GROUP@ have neither and are ErrUnknownPrincipal.
func (s *Server) spellWhoWellKnown(p entity.Principal, scheme identity.Scheme) (identity.Spelling, error) {
	if sp, err := spellWellKnown(p, whoOwner(scheme)); err == nil {
		sp.Scheme = scheme
		return sp, nil
	}
	sp, err := spellWellKnown(p, identity.SchemeSID)
	if err != nil {
		return identity.Spelling{}, err
	}
	return s.atDomain(scheme, sid.FormatSID(&sp.SID))
}

// findWho finds the principal a who names: a well-formed S-…@domain by its
// SID when the domain is the applied one, ASCII letters folded; any other as
// the owner scheme finds it. A user is found only in who, a group only in
// who_group, and a well-known principal in either.
func (s *Server) findWho(ctx context.Context, sp identity.Spelling) (entity.Principal, error) {
	d, domain, ok := whoSID(sp.Text)
	if !ok {
		return s.findOwner(ctx, identity.Spelling{Scheme: whoOwner(sp.Scheme), Text: sp.Text})
	}
	st := s.state.Load()
	if st == nil {
		return entity.Principal{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	if !asciiEqualFold(domain, st.domain) {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	p, err := s.find(ctx, identity.Spelling{Scheme: identity.SchemeSID, SID: *d}, identity.FindOpts{})
	if err != nil {
		return entity.Principal{}, err
	}
	if !whoKind(p, sp.Scheme) {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	return p, nil
}
