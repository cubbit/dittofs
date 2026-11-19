package identity

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// remappedSpec counts the directory principals minted with an allocated uid
// or gid in place of the entry's 0 or 65534, which root and anonymous hold.
var remappedSpec = metrics.Spec{
	Name:   "identity_mints_remapped_total",
	Help:   "Directory principals minted with an allocated uid or gid in place of the entry's reserved one, by kind.",
	Labels: []string{"kind"},
}

// directoryBound is how long one lookup's identity work may take, its
// directory queries and its mints together (RFC 18 §5.1).
const directoryBound = 20 * time.Second

// errDirectoryBound ends a lookup that ran past directoryBound.
var errDirectoryBound = errors.New("identity: the lookup ran past its 20-second bound")

// directoryBounded is ctx ended at directoryBound on the injected clock, so
// every query, mint and wait below it inherits one deadline, and each fails
// ErrUnavailable once it ends.
//
// decision: the bound is one deadline per lookup, not a budget split between
// its steps: a slow directory may spend the whole 20 seconds on whichever
// step it slows, each query still under its own 5-second timeout. Overturned
// if a later step, such as the mint, must be kept time an earlier slow one
// would use.
func (s *Server) directoryBounded(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	fired := s.deps.Clock.After(directoryBound)
	go func() {
		select {
		case <-fired:
			cancel(errDirectoryBound)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

// findDirectorySID is Find of a SID no principal holds: with mint, a SID of
// an ad directory's domain is asked of the directory and its holder minted;
// otherwise ErrUnknownPrincipal, as for an rfc2307 directory, which routes no
// SID.
func (s *Server) findDirectorySID(ctx context.Context, v sid.SID, mint bool) (entity.Principal, error) {
	// decision: a lookup without FindOpts.Mint never mints, and so asks the
	// directory nothing: lsarpc runs on IPC$, which an anonymous session
	// reaches, so minting there would let a caller that proved nothing write
	// one record per directory account (RFC 18 §4.4), and an answer it could
	// not mint would be ErrUnknownPrincipal anyway. Overturned if an
	// anonymous client is shown to need a stable principal for a SID it only
	// reads.
	st := s.state.Load()
	if !mint || st == nil || st.Directory == nil || !st.Directory.IsAD() {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	if st.dir == nil {
		return entity.Principal{}, fmt.Errorf("%w: identity: no directory", vfserr.ErrUnavailable)
	}
	ctx, cancel := s.directoryBounded(ctx)
	defer cancel()
	domain, err := st.dir.client.DomainSID(ctx)
	if err != nil {
		return entity.Principal{}, err
	}
	var b bytes.Buffer
	sid.EncodeSID(&b, &v)
	value := b.Bytes()
	if len(value) != len(domain)+4 || !bytes.Equal(directory.WithRID(domain, binary.LittleEndian.Uint32(value[len(domain):])), value) {
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
	ctx, cancel := s.directoryBounded(ctx)
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
// with a service component matches nothing.
func (s *Server) byKerberosName(ctx context.Context, user, realm string, mint bool) (entity.Principal, error) {
	st := s.state.Load()
	if st == nil || st.Directory == nil || realm != st.Directory.Realm || user == "" || strings.Contains(user, "/") {
		return entity.Principal{}, vfserr.ErrUnknownPrincipal
	}
	return s.findDirectoryName(ctx, st, user, mint, directory.ByUserName)
}

// KerberosLinkSID is the SID a krb link for name, user@REALM, is written
// for: the SID of the user entry the directory holds under the user part,
// asked fresh only, never served stale (RFC 18 §4.2). It fails
// ErrUnavailable when no fresh answer can be had and ErrUnknownPrincipal
// when the directory holds no such entry; a name in another realm, the realm
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
	if strings.Contains(user, "/") {
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
// ErrUnknownPrincipal.
func (s *Server) dirPrincipal(ctx context.Context, st *State, a directory.Answer, mint bool) (entity.Principal, error) {
	if len(a.EntryID) == 0 {
		return entity.Principal{}, fmt.Errorf("%w: identity: the directory answered an entry with no ID", vfserr.ErrUnavailable)
	}
	p, err := s.byDir(ctx, a.EntryID)
	if !mint || !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		return p, err
	}
	entries := []directory.Answer{a}
	if !a.Group {
		// The closure is asked of the client: the cache holds only "who
		// holds this" answers, and a mint asks it once per user it mints.
		groups, err := st.dir.client.Groups(ctx, a)
		if err != nil {
			return entity.Principal{}, err
		}
		for _, g := range groups {
			// decision: a group tokenGroups names but no entry holds is not
			// minted: with no entry ID it has no dir spelling, so no later
			// answer could find or rename it. Its SID still reaches the
			// caller's groups through resolution. Overturned if such a SID
			// must own files.
			if len(g.EntryID) > 0 {
				entries = append(entries, g)
			}
		}
	}
	var remapped []entity.Principal
	err = s.mint(ctx, func(ctx context.Context) (entity.MintBatch, int, error) {
		remapped = remapped[:0]
		var b entity.MintBatch
		bound := 0
		for _, e := range entries {
			_, err := s.byDir(ctx, e.EntryID)
			switch {
			case err == nil:
				bound++
				continue
			case !errors.Is(err, vfserr.ErrUnknownPrincipal):
				return entity.MintBatch{}, 0, err
			}
			ids, alloc, remap, err := dirSpellings(e, st.Settings.IDRange)
			if err != nil {
				return entity.MintBatch{}, 0, err
			}
			var p entity.Principal
			if e.Group {
				p = entity.NewGroup()
				b.Groups = append(b.Groups, entity.MintGroup{Group: entity.Group{Principal: p, Name: e.Name, IDs: ids}, Alloc: alloc})
			} else {
				p = entity.NewUser()
				b.Users = append(b.Users, entity.MintUser{User: entity.User{Principal: p, Name: e.Name, IDs: ids, Email: e.Mail}, Alloc: alloc})
			}
			if remap {
				remapped = append(remapped, p)
			}
		}
		return b, bound, nil
	})
	if err != nil {
		return entity.Principal{}, err
	}
	for _, p := range remapped {
		s.remapped.WithLabelValues(p.Kind.String()).Inc()
		s.logger.WarnContext(ctx, "identity: directory principal minted with an allocated ID in place of the entry's reserved one",
			"kind", p.Kind.String(), "principal", p.ID.String())
	}
	return s.byDir(ctx, a.EntryID)
}

// dirSpellings is the spellings a's record is minted with and what the mint
// allocates: the entry's ID; its SID, or a RID for none; its uidNumber, or a
// group's gidNumber, or an allocated one for none or for 0 or 65534, which
// root and anonymous hold (RFC 18 §2.3, §4.3), remapped true then.
func dirSpellings(a directory.Answer, rng entity.IDRange) (entity.ProtocolIDs, entity.Alloc, bool, error) {
	ids := entity.ProtocolIDs{{Scheme: entity.SchemeDir, Value: string(a.EntryID)}}
	var alloc entity.Alloc
	if a.SID == nil {
		alloc.RID = true
	} else {
		if _, err := decodeSID(a.SID); err != nil {
			return nil, alloc, false, err
		}
		ids = append(ids, entity.ProtocolID{Scheme: entity.SchemeSID, Value: string(a.SID)})
	}
	num, scheme := a.UID, entity.SchemeUID
	if a.Group {
		num, scheme = a.GID, entity.SchemeGID
	}
	if num != nil {
		if _, reserved := reservedIDs[*num]; !reserved {
			return append(ids, entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(*num), 10)}), alloc, false, nil
		}
	}
	alloc.Range = rng
	alloc.UID, alloc.GID = !a.Group, a.Group
	return ids, alloc, num != nil, nil
}
