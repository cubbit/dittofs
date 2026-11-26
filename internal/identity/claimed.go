package identity

import (
	"context"
	"strconv"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// claimedUID is the map step of a claimed uid: the principal holding it
// through the uid index, root for 0 and the well-known anonymous for 65534,
// with its user record, zero for a well-known one. A uid no principal holds
// maps to the well-known anonymous, counted unmapped under uid; one whose
// user is disabled is refused ErrAccess, counted disabled. A store error is
// ErrUnavailable, never anonymous.
//
// decision: an unmapped claimed uid acts as anonymous rather than as a
// principal minted for that number. Any client may send any of 2^32 uids, so
// minting on sight would let one host fill the store with principals, each
// owning files no operator can name. The cost is that a client user with no
// record creates files owned by anonymous. Add a share option that mints
// numeric principals, with a cap, when a deployment shows NFS users the store
// cannot hold records for.
func (s *Server) claimedUID(ctx context.Context, pr kvstore.Principals, uid uint32) (entity.Principal, entity.User, error) {
	if p, ok := reservedIDs[uid]; ok {
		return p, entity.User{}, nil
	}
	p, ok, err := byNumber(ctx, pr, entity.SchemeUID, uid)
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
// and of each supplementary gid, each once. A gid no principal holds is no
// group at all, left out and counted unmapped under gid, and a primary gid no
// principal holds leaves group zero, so a create falls back to the user's
// primary group. A store error is ErrUnavailable.
func (s *Server) claimedGroups(ctx context.Context, pr kvstore.Principals, c identity.Claims) (group entity.Principal, groups []entity.Principal, err error) {
	seen := map[uint32]bool{}
	for i, gid := range append([]uint32{c.GID}, c.GIDs...) {
		if seen[gid] {
			continue
		}
		seen[gid] = true
		g, ok := reservedIDs[gid]
		if !ok {
			if g, ok, err = byNumber(ctx, pr, entity.SchemeGID, gid); err != nil {
				return entity.Principal{}, nil, err
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
	return group, groups, nil
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
