package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// remappedSpec counts the directory principals minted with an allocated uid
// or gid in place of the entry's: 0 or 65534, which root and anonymous hold,
// or one another principal already holds.
var remappedSpec = metrics.Spec{
	Name:   "identity_mints_remapped_total",
	Help:   "Directory principals minted with an allocated uid or gid in place of the entry's reserved or held one, by kind.",
	Labels: []string{"kind"},
}

// skippedSpec counts the groups of a directory user's closure left unminted
// because their SID is not an account of the directory's domain, by reason:
// builtin for S-1-5-32, other_domain for any other.
var skippedSpec = metrics.Spec{
	Name:   "identity_directory_groups_skipped_total",
	Help:   "Groups of a directory user's closure not minted, their SID not of the directory's domain, by reason.",
	Labels: []string{"reason"},
}

// directoryBound is how long one lookup's identity work may take, its
// directory queries and its mints together (RFC 18 §5.1). Each lookup opens
// it once through directory.Bounded on the injected clock, so every query,
// mint and wait below it inherits one deadline, and each fails
// ErrUnavailable once it ends.
//
// decision: the bound is one deadline per lookup, not a budget split between
// its steps: a slow directory may spend the whole 20 seconds on whichever
// step it slows, each query still under its own 5-second timeout. Overturned
// if a later step, such as the mint, must be kept time an earlier slow one
// would use.
const directoryBound = 20 * time.Second

// errDirectoryBound ends a lookup that ran past directoryBound.
var errDirectoryBound = errors.New("identity: the lookup ran past its 20-second bound")

// inDomain reports whether the binary SID value is an account of domain's:
// domain's SID with one RID more.
func inDomain(domain, value []byte) bool {
	v, _, err := sid.DecodeSID(value)
	return err == nil && bytes.Equal(directory.WithRID(domain, v.RID()), value)
}

// findDirectorySID is Find of the binary SID value no principal holds: with
// mint, a SID of an ad directory's domain is asked of the directory and its
// holder minted; otherwise ErrUnknownPrincipal, as for an rfc2307 directory,
// which routes no SID.
func (s *Server) findDirectorySID(ctx context.Context, st *State, value []byte, mint bool) (entity.Principal, error) {
	// decision: a lookup without FindOpts.Mint never mints, and so asks the
	// directory nothing: lsarpc runs on IPC$, which an anonymous session
	// reaches, so minting there would let a caller that proved nothing write
	// one record per directory account (RFC 18 §4.4), and an answer it could
	// not mint would be ErrUnknownPrincipal anyway. Overturned if an
	// anonymous client is shown to need a stable principal for a SID it only
	// reads.
	if !mint || st == nil || st.Directory == nil || !st.Directory.IsAD() {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	if st.dir == nil {
		return entity.Principal{}, fmt.Errorf("%w: identity: no directory", vfserr.ErrUnavailable)
	}
	ctx, cancel := directory.Bounded(ctx, s.deps.Clock, directoryBound, errDirectoryBound)
	defer cancel()
	domain, err := st.dir.domainSID(ctx)
	if err != nil {
		return entity.Principal{}, err
	}
	if !inDomain(domain, value) {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal // another domain's
	}
	a, ok, err := st.dirLookup(ctx, directory.BySID, value, false)
	if err != nil {
		return entity.Principal{}, err
	}
	if !ok {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	return s.dirPrincipal(ctx, st, a, true)
}

// findDirectoryName is the principal holding the dir spelling of the entry
// the directory holds under name, asked under each of keys in turn until one
// answers, minted with mint.
func (s *Server) findDirectoryName(ctx context.Context, st *State, name string, mint bool, keys ...directory.Key) (entity.Principal, error) {
	ctx, cancel := directory.Bounded(ctx, s.deps.Clock, directoryBound, errDirectoryBound)
	defer cancel()
	for _, k := range keys {
		a, ok, err := st.dirLookup(ctx, k, []byte(name), false)
		if err != nil {
			return entity.Principal{}, err
		}
		if ok {
			return s.dirPrincipal(ctx, st, a, mint)
		}
	}
	return entity.Principal{}, vfserr.ErrUnknownPrincipal
}

// byKerberosName is RFC 18 §4.2's step 3: a Kerberos name in the directory's
// realm, the realm compared as the KDC spells it, maps to the entry the
// directory holds under its user part, and then to the principal holding
// that entry's dir spelling, never to one found by name alone. A user part
// with a service component matches nothing, and neither, without asking the
// directory, does a machine account's name, which ends in $ (RFC 18 §2.4).
func (s *Server) byKerberosName(ctx context.Context, user, realm string, mint bool) (entity.Principal, error) {
	st := s.state.Load()
	if st == nil || st.Directory == nil || realm != st.Directory.Realm || user == "" || strings.Contains(user, "/") || strings.HasSuffix(user, "$") {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	return s.findDirectoryName(ctx, st, user, mint, directory.ByUserName)
}

// KerberosLinkSID is the SID a krb link for name, user@REALM, is written
// for: the SID of the user entry the directory holds under the user part,
// asked fresh only, never served stale (RFC 18 §4.2). It fails
// ErrUnavailable when no fresh answer can be had and ErrUnknownPrincipal
// when the directory holds no such entry, or the user part names a service
// or ends in $, a machine account's (RFC 18 §2.4); a name in another realm, the realm
// compared case and all, and an entry with no SID give the zero SID, and
// nothing is minted.
func (s *Server) KerberosLinkSID(ctx context.Context, name string) (sid.SID, error) {
	i := strings.LastIndexByte(name, '@')
	if i <= 0 {
		return sid.SID{}, fmt.Errorf("%w: a Kerberos name is user@REALM", vfserr.ErrInvalid)
	}
	user, realm := name[:i], name[i+1:]
	st := s.state.Load()
	if st == nil {
		return sid.SID{}, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	if st.Directory == nil || realm != st.Directory.Realm {
		return sid.SID{}, nil
	}
	if strings.Contains(user, "/") || strings.HasSuffix(user, "$") {
		return sid.SID{}, vfserr.ErrUnknownPrincipal
	}
	a, ok, err := st.dirLookup(ctx, directory.ByUserName, []byte(user), true)
	if err != nil {
		return sid.SID{}, err
	}
	if !ok {
		return sid.SID{}, vfserr.ErrUnknownPrincipal
	}
	if a.SID == nil {
		return sid.SID{}, nil
	}
	v, err := decodeSID(a.SID)
	if err != nil {
		return sid.SID{}, err
	}
	return *v, nil
}

// decodeSID decodes a directory's binary SID, refusing one with trailing
// bytes, which Find would re-encode without them and miss.
func decodeSID(b []byte) (*sid.SID, error) {
	v, n, err := sid.DecodeSID(b)
	if err == nil && n != len(b) {
		err = fmt.Errorf("%d trailing bytes", len(b)-n)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: identity: the directory answered an undecodable SID: %w", vfserr.ErrUnavailable, err)
	}
	return v, nil
}

// byDir is the principal holding the dir spelling id, ErrUnknownPrincipal
// for none, and ErrUnavailable for a store failure.
func (s *Server) byDir(ctx context.Context, id []byte) (entity.Principal, error) {
	p, err := found(s.deps.Store.Principals().ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeDir, Value: string(id)}))
	if err != nil && !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		err = fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
	}
	return p, err
}

// dirPrincipal is the principal holding a's dir spelling. With none and mint
// set, a is minted through the mint path, a user with every group of its
// closure that is not yet minted, in one batch; without mint it is
// ErrUnknownPrincipal. A machine's account is never minted (RFC 18 §2.4), nor
// an ad entry whose SID is not an account of the directory's domain, such as
// a builtin group, whose SID spells a well-known principal (§4.4).
func (s *Server) dirPrincipal(ctx context.Context, st *State, a directory.Answer, mint bool) (entity.Principal, error) {
	if len(a.EntryID) == 0 {
		return entity.Principal{}, fmt.Errorf("%w: identity: the directory answered an entry with no ID", vfserr.ErrUnavailable)
	}
	p, err := s.byDir(ctx, a.EntryID)
	if !mint || !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		return p, err
	}
	if a.Computer {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	var domain []byte
	if st.Directory.IsAD() {
		if domain, err = st.dir.domainSID(ctx); err != nil {
			return entity.Principal{}, err
		}
		if a.SID != nil && !inDomain(domain, a.SID) {
			return entity.Principal{}, vfserr.ErrUnknownPrincipal
		}
	}
	entries := []directory.Answer{a}
	if !a.Group {
		groups, err := st.dir.groups(ctx, a)
		if err != nil {
			return entity.Principal{}, err
		}
		for _, g := range groups {
			// decision: a group tokenGroups names but no entry holds is not
			// minted: with no entry ID it has no dir spelling, so no later
			// answer could find or rename it. Its SID still reaches the
			// caller's groups through resolution. Overturned if such a SID
			// must own files.
			if len(g.EntryID) == 0 {
				continue
			}
			// decision: an rfc2307 directory's groups are not held to a
			// domain, since its record names none and it routes no SID;
			// overturned if one is found holding a well-known SID.
			if domain != nil && g.SID != nil && !inDomain(domain, g.SID) {
				reason := "other_domain"
				if v, _, err := sid.DecodeSID(g.SID); err == nil && strings.HasPrefix(sid.FormatSID(v), "S-1-5-32-") {
					reason = "builtin"
				}
				s.skipped.WithLabelValues(reason).Inc()
				continue
			}
			entries = append(entries, g)
		}
	}
	type remap struct {
		p  entity.Principal
		id uint32
	}
	var remapped []remap
	err = s.mint(ctx, func(ctx context.Context) (entity.MintBatch, int, error) {
		remapped, p = remapped[:0], entity.Principal{}
		var b entity.MintBatch
		bound := 0
		claimed := map[entity.ProtocolID]bool{} // the uids and gids this batch takes
		held := func(id entity.ProtocolID) (bool, error) {
			if claimed[id] {
				return true, nil
			}
			_, err := s.deps.Store.Principals().ByProtocolID(ctx, id)
			switch {
			case err == nil:
				return true, nil
			case merrs.IsNotFoundError(err):
				claimed[id] = true
				return false, nil
			}
			return false, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		}
		for i, e := range entries {
			q, err := s.byDir(ctx, e.EntryID)
			switch {
			case err == nil:
				if i == 0 {
					p = q
				}
				bound++
				continue
			case !errors.Is(err, vfserr.ErrUnknownPrincipal):
				return entity.MintBatch{}, 0, err
			}
			ids, alloc, from, err := dirSpellings(e, st.Settings.IDRange, held)
			if err != nil {
				return entity.MintBatch{}, 0, err
			}
			var np entity.Principal
			if e.Group {
				np = entity.NewGroup()
				b.Groups = append(b.Groups, entity.MintGroup{Group: entity.Group{Principal: np, Name: e.Name, IDs: ids}, Alloc: alloc})
			} else {
				np = entity.NewUser()
				b.Users = append(b.Users, entity.MintUser{User: entity.User{Principal: np, Name: e.Name, IDs: ids, Email: e.Mail}, Alloc: alloc})
			}
			if i == 0 {
				p = np
			}
			if from != nil {
				remapped = append(remapped, remap{np, *from})
			}
		}
		return b, bound, nil
	})
	if err != nil {
		return entity.Principal{}, err
	}
	for _, r := range remapped {
		s.remapped.WithLabelValues(r.p.Kind.String()).Inc()
		s.logger.WarnContext(ctx, "identity: directory principal minted with an allocated ID in place of the entry's reserved or held one",
			"kind", r.p.Kind.String(), "principal", r.p.ID.String(), "id", r.id)
	}
	return p, nil
}

// dirSpellings is the spellings a's record is minted with and what the mint
// allocates: the entry's ID; its SID, or a RID for none; its uidNumber, or a
// group's gidNumber, or an allocated one for none, for 0 or 65534, which
// root and anonymous hold (RFC 18 §2.3, §4.3), or for one held reports
// another principal holds. from is the entry's number an allocated one
// replaced, nil when none was.
//
// decision: a uidNumber or gidNumber another principal already holds is
// replaced by an allocated one, as 0 and 65534 are, rather than failing the
// mint: the user and its groups go in one batch, so one group clashing with
// a local gid would otherwise refuse every member's mint on every sign-in,
// reading as an outage. Overturned if a deployment needs the directory's
// numbers kept exactly, which would need the clash refused for that entry
// alone.
func dirSpellings(a directory.Answer, rng entity.IDRange, held func(entity.ProtocolID) (bool, error)) (entity.ProtocolIDs, entity.Alloc, *uint32, error) {
	ids := entity.ProtocolIDs{{Scheme: entity.SchemeDir, Value: string(a.EntryID)}}
	var alloc entity.Alloc
	if a.SID == nil {
		alloc.RID = true
	} else {
		if _, err := decodeSID(a.SID); err != nil {
			return nil, alloc, nil, err
		}
		ids = append(ids, entity.ProtocolID{Scheme: entity.SchemeSID, Value: string(a.SID)})
	}
	num, scheme := a.UID, entity.SchemeUID
	if a.Group {
		num, scheme = a.GID, entity.SchemeGID
	}
	if num != nil {
		if _, reserved := reservedIDs[*num]; !reserved {
			id := entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(*num), 10)}
			taken, err := held(id)
			if err != nil {
				return nil, alloc, nil, err
			}
			if !taken {
				return append(ids, id), alloc, nil, nil
			}
		}
	}
	alloc.Range = rng
	alloc.UID, alloc.GID = !a.Group, a.Group
	return ids, alloc, num, nil
}
