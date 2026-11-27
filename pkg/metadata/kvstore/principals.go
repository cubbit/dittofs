package kvstore

import (
	"context"
	"errors"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Principals is what authentication and the authorisation chokepoint read.
type Principals struct{ s *Store }

func (s *Store) Principals() Principals { return Principals{s} }

// ByName finds the user or group of that name. Names are unique per kind
// only: a user and its same-named private group are the POSIX norm.
func (p Principals) ByName(ctx context.Context, kind entity.PrincipalKind, name string) (entity.Principal, error) {
	if kind != entity.PrincipalUser && kind != entity.PrincipalGroup {
		return entity.Principal{}, merrs.NewInvalidArgumentError("only users and groups have names")
	}
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, nameKey(nameKind(kind), name))
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewNotFoundError(name, kind.String())
		}
		id, err := decodeNameRow(b)
		out = entity.Principal{Kind: kind, ID: id}
		return err
	})
	return out, err
}

// ByProtocolID finds the principal holding id. A principal marked deleted
// holds none: its dir spelling's row, the one row its mark keeps, reads as
// not found.
func (p Principals) ByProtocolID(ctx context.Context, id entity.ProtocolID) (entity.Principal, error) {
	var out entity.Principal
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		holder, marked, ok, err := protocolRow(r, id)
		switch {
		case err != nil:
			return err
		case !ok || marked:
			return merrs.NewNotFoundError(id.Scheme.String()+":"+id.Value, "principal")
		}
		out = holder
		return nil
	})
	return out, err
}

// Marked reports whether a principal marked deleted, its deletion not yet
// ended, holds id: true only for a dir spelling, the one row a mark keeps.
func (p Principals) Marked(ctx context.Context, id entity.ProtocolID) (bool, error) {
	var marked bool
	err := p.s.kv.View(ctx, func(r kv.Reader) (err error) {
		_, marked, _, err = protocolRow(r, id)
		return err
	})
	return marked, err
}

// protocolRow reads id's row: its principal, whether that principal is
// marked deleted, and whether the row exists.
//
// decision: a mark keeps the marked record's dir spelling row, which its
// deletion's last batch deletes with the record, so the directory entry it
// names is not minted anew while the deletion runs: a sign-in naming it
// fails as an unknown name's does. Every other protocol-ID row goes at the
// mark, free for a new principal at once; a dir spelling is freed when the
// deletion ends, after which a mint for the entry makes a new principal, as
// a SID reused after a deletion gets one. Only a dir row is checked against
// its holder's record, the one read that costs; overturned if another
// scheme's row must outlive a mark.
func protocolRow(r kv.Reader, id entity.ProtocolID) (entity.Principal, bool, bool, error) {
	b, err := r.Get(protocolKey(id))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.Principal{}, false, false, nil
	}
	if err != nil {
		return entity.Principal{}, false, false, err
	}
	holder, _, err := decodeProtocolRow(id.Scheme, b) // the row's linked SID unread: see encodeProtocolRow
	if err != nil || id.Scheme != entity.SchemeDir {
		return holder, false, true, err
	}
	ok, err := live(r, holder)
	return holder, !ok, true, err
}

// Groups yields every group p is a member of, directly or through nested
// groups, each once, nearest first. A cycle of nested groups ends the walk
// rather than loop. A group marked deleted is not one, nor are the groups
// reached only through it; the same holds for each group in stop.
func (p Principals) Groups(ctx context.Context, of entity.Principal, stop ...entity.Principal) iter.Seq2[entity.Principal, error] {
	return func(yield func(entity.Principal, error) bool) {
		var groups []entity.Principal
		err := p.s.kv.View(ctx, func(r kv.Reader) (err error) {
			groups, err = groupsOf(r, of, stop)
			return err
		})
		if err != nil {
			yield(entity.Principal{}, err)
			return
		}
		for _, g := range groups {
			if !yield(g, nil) {
				return
			}
		}
	}
}

// ponytail: collects the whole transitive closure before yielding, so memory
// grows with the number of groups one principal is in, not with group sizes.
// Stream it if a principal's groups ever number in the thousands.
func groupsOf(r kv.Reader, of entity.Principal, stop []entity.Principal) ([]entity.Principal, error) {
	seen := map[entity.PrincipalID]bool{of.ID: true}
	for _, s := range stop {
		seen[s.ID] = true // never yielded, never walked through
	}
	var out []entity.Principal
	queue := []entity.PrincipalID{of.ID}
	for len(queue) > 0 {
		member := queue[0]
		queue = queue[1:]
		prefix := memberOfPrefix(member)
		for kvp, err := range r.Scan(prefix, nil) {
			if err != nil {
				return nil, err
			}
			gid, _, err := keys.ReadID(kvp.Key[len(prefix):], keys.IDLen)
			if err != nil {
				return nil, err
			}
			id := entity.PrincipalID(gid)
			if seen[id] {
				continue
			}
			seen[id] = true
			g := entity.Principal{Kind: entity.PrincipalGroup, ID: id}
			if ok, err := live(r, g); err != nil || !ok {
				if err != nil {
					return nil, err
				}
				continue
			}
			out = append(out, g)
			queue = append(queue, id)
		}
	}
	return out, nil
}

// Grant is the grant naming of itself on share, and true; false when none
// does. A stored grant of none is (none, true), distinct from no grant: it
// refuses of where its groups' grants would admit (RFC 18 §6.4). Grants
// naming of's groups, authenticated users or everyone are not read: the
// order among them is resolution's.
func (p Principals) Grant(ctx context.Context, share entity.ShareID, of entity.Principal) (entity.ShareAccess, bool, error) {
	var g entity.ShareGrant
	var ok bool
	err := p.s.kv.View(ctx, func(r kv.Reader) (err error) {
		g, ok, err = grantIn(r, share, of.ID)
		return err
	})
	return g.Access, ok, err
}

func grantIn(r kv.Reader, share entity.ShareID, p entity.PrincipalID) (entity.ShareGrant, bool, error) {
	b, err := r.Get(grantKey(share, p))
	if errors.Is(err, kv.ErrNotFound) {
		return entity.ShareGrant{}, false, nil
	}
	if err != nil {
		return entity.ShareGrant{}, false, err
	}
	g, err := decodeGrant(share, p, b)
	return g, err == nil, err
}

// User is the user or machine record of p: what resolution reads to refuse
// a disabled or deleted user. A deleted one, or one marked deleted, is not found, and a
// Primary naming a group deleted or marked deleted reads as zero.
func (p Principals) User(ctx context.Context, of entity.Principal) (entity.User, error) {
	var u entity.User
	err := p.s.kv.View(ctx, func(r kv.Reader) error {
		var ok bool
		var err error
		if u, ok, err = userIn(r, of); err != nil || !ok {
			return notFound(err, of)
		}
		u.Primary, err = primaryIn(r, u)
		return err
	})
	return u, err
}

// primaryIn is u's primary group, or zero when it names none or a group
// deleted or marked deleted.
func primaryIn(r kv.Reader, u entity.User) (entity.Principal, error) {
	if u.Primary.IsZero() {
		return u.Primary, nil
	}
	if ok, err := live(r, u.Primary); err != nil || !ok {
		return entity.Principal{}, err
	}
	return u.Primary, nil
}

// Records reads one group's record by ID: its spellings and its deleted mark.
type Records struct{ s *Store }

func (s *Store) Records() Records { return Records{s} }

// Group is p's group record. One deleted or marked deleted is not found, nor
// is a principal of another kind.
func (r Records) Group(ctx context.Context, p entity.Principal) (entity.Group, error) {
	var g entity.Group
	err := r.s.kv.View(ctx, func(rd kv.Reader) error {
		if p.Kind != entity.PrincipalGroup {
			return notFound(nil, p)
		}
		b, ok, err := get(rd, groupKey(p.ID))
		if err != nil || !ok {
			return notFound(err, p)
		}
		if g, err = decodeGroup(p.ID, b); err == nil && g.Deleted {
			err = notFound(nil, p)
		}
		return err
	})
	return g, err
}
