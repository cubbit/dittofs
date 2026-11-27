package identity

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// renamesUnnamedSpec counts the records a later directory answer left without
// a name: their new name held by a principal that keeps it, refused by the
// name rule, or folding equal to a name another record of the same answer
// takes.
var renamesUnnamedSpec = metrics.Spec{
	Name:   "identity_renames_unnamed_total",
	Help:   "Directory principals a later answer left without a name, which another principal kept or the name rule refuses, by kind.",
	Labels: []string{"kind"},
}

// dirBound is a minted directory principal and the directory's answer for
// its entry.
type dirBound struct {
	p entity.Principal
	a directory.Answer
}

// renamePlan is one refresh's renames, with the records listed "" against
// the answer's name, each with the principal holding that name, the zero
// one when the name rule refused it.
type renamePlan struct {
	list    []entity.Renaming
	unnamed map[entity.Principal]entity.Principal
	emails  map[entity.Principal]string // the users whose address changed, with their new one
}

// refresh brings each bound record's name and e-mail address to its answer's,
// writing only what changed: every rename the answers cause in one
// Minter.Rename, then each changed address through SetEmail. A
// PrincipalID, entry ID, SID, uid or gid is never written. On ErrExist,
// another call took a name between the plan's reads and the Rename, so the
// plan runs again on fresh reads; a run that leaves no record unnamed that
// an earlier run of this call did not fails the call ErrUnavailable, writing
// nothing, as the mint path's progress rule does.
func (s *Server) refresh(ctx context.Context, st *State, bound []dirBound) error {
	seen := map[entity.Principal]bool{} // the records this call has left unnamed, across runs
	var plan renamePlan
	for run := 0; ; run++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		}
		var err error
		if plan, err = s.planRenames(ctx, st, bound); err != nil {
			return err
		}
		newly := 0
		for p := range plan.unnamed {
			if !seen[p] {
				seen[p], newly = true, newly+1
			}
		}
		if run > 0 && newly == 0 {
			return fmt.Errorf("%w: a rename re-run left unnamed nothing the run before did not", vfserr.ErrUnavailable)
		}
		if s.beforeRename != nil {
			s.beforeRename()
		}
		err = s.deps.Store.Minter().Rename(ctx, plan.list)
		if err == nil {
			break
		}
		if !merrs.IsAlreadyExistsError(err) {
			return createErr(err)
		}
	}
	for p, holder := range plan.unnamed {
		s.renamedOff.WithLabelValues(p.Kind.String()).Inc()
		if holder.IsZero() {
			s.logger.WarnContext(ctx, "identity: directory principal left without its new name, which the name rule refuses",
				"kind", p.Kind.String(), "principal", p.ID.String())
		} else {
			s.logger.WarnContext(ctx, "identity: directory principal left without its new name, which another holds",
				"kind", p.Kind.String(), "principal", p.ID.String(), "holder", holder.ID.String())
		}
	}
	for p, email := range plan.emails {
		if err := s.deps.Store.Minter().SetEmail(ctx, p, email); err != nil && !merrs.IsNotFoundError(err) {
			return createErr(err)
		}
	}
	return nil
}

// planRenames reads each bound record and lists the renames its answer
// causes: a name free, or held by a record of the list that gives it up, is
// taken; one held by a local principal leaves the record unnamed; one held
// by a directory principal asks the directory about that holder's entry,
// fresh only and one hop, never further, and only "no such entry" or a name
// not folding equal to the one held makes the holder give it up, the holder
// then taking its own new name when free or when it is the name the renaming
// record gives up, else unnamed. Of two takes whose names fold equal within
// a kind, the second is listed "". A record deleted since it was bound is
// left out.
func (s *Server) planRenames(ctx context.Context, st *State, bound []dirBound) (renamePlan, error) {
	plan := renamePlan{unnamed: map[entity.Principal]entity.Principal{}, emails: map[entity.Principal]string{}}
	type rec struct {
		p         entity.Principal
		cur, want string
		refused   bool // the answer's name is one the name rule refuses
	}
	var recs []rec
	listed := map[entity.Principal]string{} // the names the list's records give up
	inRecs := map[entity.Principal]bool{}   // the records listed, or about to be
	for _, b := range bound {
		name, email, _, ok, err := s.dirRecord(ctx, b.p)
		if err != nil {
			return renamePlan{}, err
		}
		if !ok {
			continue
		}
		if b.p.Kind == entity.PrincipalUser && email != b.a.Mail {
			plan.emails[b.p] = b.a.Mail
		}
		r := rec{p: b.p, cur: name, want: b.a.Name}
		if entity.CheckPrincipalName(r.want) != nil {
			r.refused, r.want = r.want != "", ""
		}
		if r.want == r.cur {
			continue
		}
		recs = append(recs, r)
		inRecs[r.p] = true
		if r.cur != "" && entity.FoldPrincipalName(r.cur) != entity.FoldPrincipalName(r.want) {
			listed[r.p] = r.cur
		}
	}
	takes := map[string]bool{} // kind and folded name of every take listed
	takeKey := func(p entity.Principal, name string) string {
		return p.Kind.String() + "\x00" + entity.FoldPrincipalName(name)
	}
	// holdsFor reports the principal holding name against p: the zero one
	// when it is free, p's own, or one the list gives it up from.
	holdsFor := func(p entity.Principal, name string) (entity.Principal, error) {
		h, err := s.deps.Store.Principals().ByName(ctx, p.Kind, name)
		switch {
		case merrs.IsNotFoundError(err):
			return entity.Principal{}, nil
		case err != nil:
			return entity.Principal{}, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		case h == p:
			return entity.Principal{}, nil
		}
		if gave, ok := listed[h]; ok && entity.FoldPrincipalName(gave) == entity.FoldPrincipalName(name) {
			return entity.Principal{}, nil
		}
		return h, nil
	}
	list := func(p entity.Principal, name string, holder entity.Principal, unnamed bool) {
		if unnamed {
			plan.unnamed[p] = holder
			name = ""
		} else if name != "" {
			takes[takeKey(p, name)] = true
		}
		plan.list = append(plan.list, entity.Renaming{P: p, Name: name})
	}
	for _, r := range recs {
		if r.want == "" {
			list(r.p, "", entity.Principal{}, r.refused)
			continue
		}
		if takes[takeKey(r.p, r.want)] {
			list(r.p, "", entity.Principal{}, true)
			continue
		}
		h, err := holdsFor(r.p, r.want)
		if err != nil {
			return renamePlan{}, err
		}
		if h.IsZero() {
			list(r.p, r.want, h, false)
			continue
		}
		// A holder this answer also names keeps its name: its own answer
		// decides it, so it is never listed twice.
		hn, gives := "", false
		if !inRecs[h] {
			if hn, gives, err = s.holderGivesUp(ctx, st, h, r.want); err != nil {
				return renamePlan{}, err
			}
		}
		if !gives {
			list(r.p, "", h, true)
			continue
		}
		list(r.p, r.want, h, false)
		listed[h] = r.want
		inRecs[h] = true
		// The holder takes its new name when free or when it is the one the
		// renaming record gives up, as holdsFor has it.
		switch {
		case hn == "":
			list(h, "", entity.Principal{}, false)
		case entity.CheckPrincipalName(hn) != nil, takes[takeKey(h, hn)]:
			list(h, "", entity.Principal{}, true)
		default:
			hh, err := holdsFor(h, hn)
			if err != nil {
				return renamePlan{}, err
			}
			list(h, hn, hh, !hh.IsZero())
		}
	}
	return plan, nil
}

// holderGivesUp asks the directory, fresh only, about the entry of the
// directory principal h holding name: gives is true for "no such entry",
// newName "", and for an answer whose name does not fold equal to name, with
// that name as newName. A local principal, an error, a timeout or an answer
// only the stale grace could give leaves h its name.
func (s *Server) holderGivesUp(ctx context.Context, st *State, h entity.Principal, name string) (newName string, gives bool, err error) {
	_, _, ids, ok, err := s.dirRecord(ctx, h)
	if err != nil || !ok {
		return "", false, err
	}
	var entry []byte
	for _, id := range ids {
		if id.Scheme == entity.SchemeDir {
			entry = []byte(id.Value)
		}
	}
	if entry == nil {
		return "", false, nil
	}
	a, found, err := st.dirLookup(ctx, directory.ByEntryID, entry, true)
	switch {
	case err != nil:
		s.logger.DebugContext(ctx, "identity: a name's holder keeps it, its entry unanswered", "holder", h.ID.String(), "error", err)
		return "", false, nil
	case !found:
		return "", true, nil
	case entity.FoldPrincipalName(a.Name) == entity.FoldPrincipalName(name):
		return "", false, nil
	}
	return a.Name, true, nil
}

// dirRecord is p's name, e-mail address (a user's) and protocol IDs; ok false
// for a record absent or marked deleted.
func (s *Server) dirRecord(ctx context.Context, p entity.Principal) (name, email string, ids entity.ProtocolIDs, ok bool, err error) {
	if p.Kind == entity.PrincipalGroup {
		g, err := s.deps.Store.Records().Group(ctx, p)
		if err != nil {
			return "", "", nil, false, readErr(err)
		}
		return g.Name, "", g.IDs, true, nil
	}
	u, err := s.deps.Store.Principals().User(ctx, p)
	if err != nil {
		return "", "", nil, false, readErr(err)
	}
	return u.Name, u.Email, u.IDs, true, nil
}

// readErr is nil for a store's "not found", read as no live record, and
// ErrUnavailable for any other failure.
func readErr(err error) error {
	if merrs.IsNotFoundError(err) {
		return nil
	}
	return fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
}
