package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// unmappedSpec counts the uids, gids and SIDs Find answered no principal for.
var unmappedSpec = metrics.Spec{
	Name:   "identity_unmapped_total",
	Help:   "Spellings no principal holds, by scheme.",
	Labels: []string{"scheme"},
}

// unmappedLabel is the label a scheme's misses are counted under; a scheme
// with none is not counted.
var unmappedLabel = map[identity.Scheme]string{identity.SchemeUID: "uid", identity.SchemeGID: "gid", identity.SchemeSID: "sid"}

// wellKnownSIDs spells the well-known principals that have a SID of their
// own. OWNER@ and GROUP@ have none: an adapter spells them as the file's
// owner or group.
var wellKnownSIDs = []struct {
	p   entity.Principal
	sid *sid.SID
}{
	{entity.Everyone, sid.WellKnownEveryone},
	{entity.OwnerRights, sid.WellKnownOwnerRights},
	{entity.CreatorOwner, sid.WellKnownCreatorOwner},
	{entity.CreatorGroup, sid.WellKnownCreatorGroup},
	{entity.AuthenticatedUsers, sid.WellKnownAuthenticatedUsers},
	{entity.BuiltinUsers, sid.WellKnownBuiltinUsers},
	{entity.Administrators, sid.WellKnownAdministrators},
	{entity.Network, sid.WellKnownNetwork},
	{entity.System, sid.WellKnownSystem},
	{entity.AnonymousPrincipal, sid.WellKnownAnonymous},
	{entity.RootPrincipal, sid.WellKnownUnixRoot},
}

// reservedIDs is the uid and gid each of root and anonymous holds by rule.
var reservedIDs = map[uint32]entity.Principal{0: entity.RootPrincipal, 65534: entity.AnonymousPrincipal}

// deferred refuses a scheme Spell and Find do not serve yet.
//
// decision: owner and oidc answer ErrInvalid, not ErrUnknownPrincipal: the
// owner scheme needs identity.nfs4_domain and the oidc one its rows, neither
// of which exists yet, and "no one holds it" would be read by a client as
// data. Withdraw each when its setting or rows land.
func deferred(scheme identity.Scheme) error {
	switch {
	case scheme == identity.SchemeOwner, scheme == identity.SchemeOIDC:
		return fmt.Errorf("%w: the %d scheme is not served yet", vfserr.ErrInvalid, scheme)
	case scheme.Stored(), scheme == identity.SchemeQualified:
		return nil
	}
	return fmt.Errorf("%w: unknown scheme %d", vfserr.ErrInvalid, scheme)
}

// Spell returns p's spelling in scheme, or ErrUnknownPrincipal. A stored
// spelling is read off p's record; qualified is derived from its name and the
// NetBIOS name of the State applied when the call reads it.
//
// decision: a deleted principal is ErrUnknownPrincipal, which an SMB adapter
// shows as the null SID, not as the SID it had, because deleting a principal
// drops its index rows and its SID with them. The cost is that two deleted
// owners look alike in the security tab. Keep deleted principals' SIDs as
// tombstone rows when an operator needs to tell them apart.
func (s *Server) Spell(ctx context.Context, p entity.Principal, scheme identity.Scheme) (identity.Spelling, error) {
	if err := deferred(scheme); err != nil {
		return identity.Spelling{}, err
	}
	if p.WellKnown() {
		return spellWellKnown(p, scheme)
	}
	var name string
	var ids entity.ProtocolIDs
	switch p.Kind {
	case entity.PrincipalGroup:
		g, err := s.deps.Store.Records().Group(ctx, p)
		if err != nil {
			return identity.Spelling{}, unknown(err)
		}
		name, ids = g.Name, g.IDs
	case entity.PrincipalUser, entity.PrincipalMachine:
		u, err := s.deps.Store.Principals().User(ctx, p)
		if err != nil {
			return identity.Spelling{}, unknown(err)
		}
		name, ids = u.Name, u.IDs
	default:
		return identity.Spelling{}, vfserr.ErrUnknownPrincipal
	}
	sp := identity.Spelling{Scheme: scheme}
	switch scheme {
	case identity.SchemeUserName, identity.SchemeGroupName:
		if (scheme == identity.SchemeUserName) != (p.Kind == entity.PrincipalUser) || name == "" {
			return identity.Spelling{}, vfserr.ErrUnknownPrincipal
		}
		sp.Text = name
		return sp, nil
	case identity.SchemeQualified:
		if name == "" {
			return identity.Spelling{}, vfserr.ErrUnknownPrincipal
		}
		st := s.state.Load()
		if st == nil {
			return identity.Spelling{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
		}
		domain := st.Settings.NetBIOSName
		if has(ids, entity.SchemeDir) {
			// A directory principal is spelled under the directory's domain.
			if st.Directory == nil {
				return identity.Spelling{}, vfserr.ErrUnknownPrincipal
			}
			domain = st.Directory.NetBIOSDomain
		}
		sp.Text = domain + `\` + name
		return sp, nil
	}
	i := slices.IndexFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == stored(scheme) })
	if i < 0 {
		return identity.Spelling{}, vfserr.ErrUnknownPrincipal
	}
	v := ids[i].Value
	switch scheme {
	case identity.SchemeUID, identity.SchemeGID:
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return identity.Spelling{}, fmt.Errorf("identity: %v holds %s %q: %w", p, ids[i].Scheme, v, err)
		}
		sp.Num = uint32(n)
	case identity.SchemeSID:
		d, n, err := sid.DecodeSID([]byte(v))
		if err != nil {
			return identity.Spelling{}, fmt.Errorf("identity: %v holds an undecodable SID: %w", p, err)
		}
		// Find re-encodes the SID without the trailing bytes and would miss the row.
		if n != len(v) {
			return identity.Spelling{}, fmt.Errorf("identity: %v holds a SID with %d trailing bytes", p, len(v)-n)
		}
		sp.SID = *d
	default:
		sp.Text = v
	}
	return sp, nil
}

// spellWellKnown spells a well-known principal: root and anonymous by their
// reserved uid and gid, and each with a SID by it.
func spellWellKnown(p entity.Principal, scheme identity.Scheme) (identity.Spelling, error) {
	switch scheme {
	case identity.SchemeUID, identity.SchemeGID:
		for n, holder := range reservedIDs {
			if holder == p {
				return identity.Spelling{Scheme: scheme, Num: n}, nil
			}
		}
	case identity.SchemeSID:
		for _, w := range wellKnownSIDs {
			if w.p == p {
				return identity.Spelling{Scheme: scheme, SID: *w.sid}, nil
			}
		}
	}
	return identity.Spelling{}, vfserr.ErrUnknownPrincipal
}

// Find returns the principal holding sp, or ErrUnknownPrincipal: through the
// index rows for a stored scheme, by name for qualified. A SID of the
// directory's domain no principal holds is asked of the directory, and its
// holder minted, only with o.Mint; a qualified name of the directory's domain
// is asked of the directory, and its holder minted with o.Mint. It never
// returns an API-only account, and counts a uid, gid or SID no principal
// holds.
func (s *Server) Find(ctx context.Context, sp identity.Spelling, o identity.FindOpts) (entity.Principal, error) {
	p, err := s.find(ctx, sp, o)
	if label, ok := unmappedLabel[sp.Scheme]; ok && errors.Is(err, vfserr.ErrUnknownPrincipal) {
		s.unmapped.WithLabelValues(label).Inc()
	}
	return p, err
}

func (s *Server) find(ctx context.Context, sp identity.Spelling, o identity.FindOpts) (entity.Principal, error) {
	if err := deferred(sp.Scheme); err != nil {
		return entity.Principal{}, err
	}
	pr := s.deps.Store.Principals()
	switch sp.Scheme {
	case identity.SchemeUID, identity.SchemeGID:
		if p, ok := reservedIDs[sp.Num]; ok {
			return p, nil
		}
		return found(pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: stored(sp.Scheme), Value: strconv.FormatUint(uint64(sp.Num), 10)}))
	case identity.SchemeSID:
		for _, w := range wellKnownSIDs {
			if w.sid.Equal(&sp.SID) {
				return w.p, nil
			}
		}
		var b bytes.Buffer
		sid.EncodeSID(&b, &sp.SID)
		p, err := found(pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeSID, Value: b.String()}))
		if errors.Is(err, vfserr.ErrUnknownPrincipal) {
			return s.findDirectorySID(ctx, s.state.Load(), b.Bytes(), o.Mint)
		}
		return p, err
	case identity.SchemeGroupName:
		return found(pr.ByName(ctx, entity.PrincipalGroup, sp.Text))
	case identity.SchemeUserName:
		p, err := found(pr.ByName(ctx, entity.PrincipalUser, sp.Text))
		return s.fileUser(ctx, p, err)
	case identity.SchemeKrb, identity.SchemeDir:
		p, err := found(pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: stored(sp.Scheme), Value: sp.Text}))
		return s.fileUser(ctx, p, err)
	}
	return s.findQualified(ctx, sp.Text, o.Mint)
}

// findQualified maps DOMAIN\name by name when DOMAIN is the installation's
// NetBIOS name, and through the directory, minting with mint, when it is the
// directory's NetBIOS domain, each compared with ASCII letters folded.
//
// decision: a name a user and a group share maps to the user, as a POSIX
// user's private group shares its name; the group is still found by SID and
// by its group name. Overturned if lsarpc must tell the two apart by
// qualified name alone.
func (s *Server) findQualified(ctx context.Context, text string, mint bool) (entity.Principal, error) {
	domain, name, ok := strings.Cut(text, `\`)
	if !ok || domain == "" || name == "" {
		return entity.Principal{}, fmt.Errorf("%w: a qualified name is DOMAIN\\name", vfserr.ErrInvalid)
	}
	st := s.state.Load()
	if st == nil {
		return entity.Principal{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	if !asciiEqualFold(domain, st.Settings.NetBIOSName) {
		if st.Directory == nil || !asciiEqualFold(domain, st.Directory.NetBIOSDomain) {
			return entity.Principal{}, vfserr.ErrUnknownPrincipal
		}
		return s.findDirectoryName(ctx, st, name, mint, directory.ByUserName, directory.ByGroupName)
	}
	pr := s.deps.Store.Principals()
	p, err := found(pr.ByName(ctx, entity.PrincipalUser, name))
	if p, err = s.fileUser(ctx, p, err); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		return p, err
	}
	return found(pr.ByName(ctx, entity.PrincipalGroup, name))
}

// fileUser passes p through unless it is an API-only account, a user with no
// uid, which no file protocol may present.
func (s *Server) fileUser(ctx context.Context, p entity.Principal, err error) (entity.Principal, error) {
	if err != nil || p.Kind != entity.PrincipalUser {
		return p, err
	}
	u, err := s.deps.Store.Principals().User(ctx, p)
	if err != nil {
		return entity.Principal{}, unknown(err)
	}
	if !has(u.IDs, entity.SchemeUID) {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	return p, nil
}

// storedAs is the store's scheme of each identity scheme with protocol-ID
// index rows; the name schemes have name rows instead.
var storedAs = map[identity.Scheme]entity.ProtocolScheme{
	identity.SchemeUID: entity.SchemeUID, identity.SchemeGID: entity.SchemeGID, identity.SchemeSID: entity.SchemeSID,
	identity.SchemeKrb: entity.SchemeKerberos, identity.SchemeDir: entity.SchemeDir,
}

func stored(scheme identity.Scheme) entity.ProtocolScheme { return storedAs[scheme] }

func has(ids entity.ProtocolIDs, scheme entity.ProtocolScheme) bool {
	return slices.ContainsFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == scheme })
}

// unknown is ErrUnknownPrincipal for a store's "not found", and any other
// error unchanged, so an outage never reads as a principal no one holds.
func unknown(err error) error {
	if merrs.IsNotFoundError(err) {
		return vfserr.ErrUnknownPrincipal
	}
	return err
}

func found(p entity.Principal, err error) (entity.Principal, error) {
	if err != nil {
		return entity.Principal{}, unknown(err)
	}
	return p, nil
}

// asciiEqualFold compares two ASCII names with ASCII letters folded only, so
// the Kelvin sign never matches k as strings.EqualFold would have it.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'a' <= x && x <= 'z' {
			x -= 'a' - 'A'
		}
		if 'a' <= y && y <= 'z' {
			y -= 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
