package identity

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// claimedUID is the map step of id's claimed uid under st: the principal
// holding it through the uid index, root for 0 and the well-known anonymous
// for 65534, with its user record, zero for a well-known one. A uid no
// principal holds is asked of st's directory, when it has one (see
// claimedDir), and the entry that holds it minted. A uid neither holds maps
// to the well-known anonymous, counted unmapped under uid; one whose user is
// disabled is refused ErrAccess, counted disabled. A store or directory
// error, or a cap reached, is ErrUnavailable, never anonymous.
//
// decision: a claimed uid neither the store nor the directory holds acts as
// anonymous rather than as a principal minted for that number. Any client
// may send any of 2^32 uids, so
// minting on sight would let one host fill the store with principals, each
// owning files no operator can name. The cost is that a client user with no
// record creates files owned by anonymous. Add a share option that mints
// numeric principals, with a cap, when a deployment shows NFS users the store
// cannot hold records for.
func (s *Server) claimedUID(ctx context.Context, st *State, pr kvstore.Principals, id identity.Identity) (entity.Principal, entity.User, error) {
	uid := id.Claims.UID
	if p, ok := reservedIDs[uid]; ok {
		return p, entity.User{}, nil
	}
	p, ok, err := byNumber(ctx, pr, entity.SchemeUID, uid)
	if err == nil && !ok {
		p, ok, err = s.claimedDir(ctx, st, id.Client, directory.ByUID, uid)
	}
	if err != nil {
		return p, entity.User{}, err
	}
	if !ok {
		s.unmapped.WithLabelValues("uid").Inc()
		return entity.AnonymousPrincipal, entity.User{}, nil
	}
	u, err := live(ctx, pr, p, reasonDisabled)
	return p, u, err
}

// claimedGroups is an unsquashed claimed caller's primary group, the
// principal holding its claimed gid, and its groups: the holders of that gid
// and of each supplementary gid, each once, a gid no principal holds asked
// of st's directory as claimedUID asks a uid. A gid no principal holds after
// that is no group at all, left out and counted unmapped under gid, and a
// primary gid no principal holds leaves group zero, so a create falls back
// to the user's primary group. short reports a supplementary gid left out
// unasked past the client's lookup cap. A store or directory error, or a
// cap reached for the primary gid, is ErrUnavailable.
func (s *Server) claimedGroups(ctx context.Context, st *State, pr kvstore.Principals, id identity.Identity) (group entity.Principal, groups []entity.Principal, short bool, err error) {
	c := id.Claims
	seen := map[uint32]bool{}
	for i, gid := range append([]uint32{c.GID}, c.GIDs...) {
		if seen[gid] {
			continue
		}
		seen[gid] = true
		g, ok := reservedIDs[gid]
		if !ok {
			if g, ok, err = byNumber(ctx, pr, entity.SchemeGID, gid); err == nil && !ok {
				g, ok, err = s.claimedDir(ctx, st, id.Client, directory.ByGID, gid)
				// decision: past the client's lookup cap, a supplementary
				// gid no principal holds is left out of the caller's groups,
				// unasked and uncharged, rather than refusing the call as RFC
				// 18 §7.3 has it; only the uid and the primary gid refuse it.
				// A gid no principal holds names nothing an ACL or a grant
				// can hold, so leaving it out loses no grant and escapes no
				// deny entry, and a host whose users carry local gids the
				// directory lacks does not spend its window on them and then
				// refuse every user. The caller is built with a zero Fresh,
				// so it is not cached and its next call asks again.
				// Overturned if an ACL can ever name a group the node has not
				// minted.
				if i > 0 && errors.Is(err, errLookupCap) {
					ok, err, short = false, nil, true
				}
			}
			if err != nil {
				return entity.Principal{}, nil, false, err
			}
		}
		if !ok {
			s.unmapped.WithLabelValues("gid").Inc()
			continue
		}
		if i == 0 {
			group = g
		}
		groups = append(groups, g)
	}
	return group, groups, short, nil
}

// claimedDir is the principal of the entry st's directory holds under n by
// by, ByUID or ByGID, minted if no record holds it, for a Claimed caller
// from client: false with no directory, when no entry holds n, or when the
// entry is one no record is minted for, a computer's account or an entry
// of another domain among them. Every query the lookup sends, the uid or gid
// question and a user's closure query, is charged to client's lookup cap,
// and every record it mints to the node's mint cap; a cap reached refuses
// the call ErrUnavailable, counted lookup_cap. A directory error, or an
// answer neither fresh nor within the stale grace, is ErrUnavailable,
// counted directory_unavailable, and writes nothing.
//
// decision: a claimed number the directory holds mints the entry's
// principal, although an unauthenticated Find never mints. Owners, ACL
// entries, quotas and audit all key on one principal, so an AUTH_SYS user
// who also signs in over Kerberos or SMB must be one record, not a number
// beside it, and without the mint no directory user is minted where nothing
// signs in with a proof. The guards keep the claim from buying more than the
// directory already says: no record for a number it does not know, a mint
// rate no client can raise, and a record bound to the entry's ID that
// follows the entry. Overturned if identity_mints_total{kind="user"} is seen
// rising at the node cap while no user signs in; mints then need a proof.
//
// decision: a number two directory entries hold is refused ErrAccess, never
// minted and never anonymous, since a mint is made only on one entry's word;
// the directory cache counts the lookup ambiguous. A directory with
// duplicate numbers locks those users out until it is fixed. Overturned if
// an operator needs one of the two entries preferred.
func (s *Server) claimedDir(ctx context.Context, st *State, client netip.Addr, by directory.Key, n uint32) (entity.Principal, bool, error) {
	if st == nil || st.Directory == nil {
		return entity.Principal{}, false, nil
	}
	if st.dir == nil {
		return entity.Principal{}, false, dirRefusal(errors.New("identity: a directory with no client"))
	}
	ctx, cancel := directory.Bounded(ctx, s.deps.Clock, directoryBound, errDirectoryBound)
	defer cancel()
	ch := &charges{s: s, client: client}
	e, queried, err := st.dir.lookupCharged(ctx, by, []byte(strconv.FormatUint(uint64(n), 10)), st.Settings.DirectoryStaleGrace, false, ch.lookup)
	switch {
	case errors.Is(err, errLookupCap):
		return entity.Principal{}, false, err
	case errors.Is(err, directory.ErrAmbiguous):
		return entity.Principal{}, false, fmt.Errorf("%w: identity: two directory entries hold one claimed number", vfserr.ErrAccess)
	case err != nil:
		return entity.Principal{}, false, dirRefusal(err)
	case !e.found:
		return entity.Principal{}, false, nil
	}
	p, err := s.dirPrincipal(ctx, st, e.ans, queried, true, ch)
	var r *refusal
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return entity.Principal{}, false, nil
	case errors.As(err, &r):
		return entity.Principal{}, false, err
	case errors.Is(err, vfserr.ErrUnavailable):
		return entity.Principal{}, false, dirRefusal(err)
	case err != nil:
		return entity.Principal{}, false, err
	}
	return p, true, nil
}

// byNumber is the principal holding n in scheme's index; false when none
// does.
func byNumber(ctx context.Context, pr kvstore.Principals, scheme entity.ProtocolScheme, n uint32) (entity.Principal, bool, error) {
	p, err := pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(n), 10)})
	if merrs.IsNotFoundError(err) {
		return entity.Principal{}, false, nil
	}
	if err != nil {
		return entity.Principal{}, false, unavailable(err)
	}
	return p, true, nil
}
