package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// mintsSpec counts every record identity mints, by its kind: machine, user
// or group.
var mintsSpec = metrics.Spec{
	Name:   "identity_mints_total",
	Help:   "Principal records identity minted, by kind.",
	Labels: []string{"kind"},
}

// unnamedSpec counts the records minted without their name because another
// principal of their kind held it.
var unnamedSpec = metrics.Spec{
	Name:   "identity_mints_unnamed_total",
	Help:   "Principal records minted without their name, which another principal held, by kind.",
	Labels: []string{"kind"},
}

// mint is the one path that writes new principal records. plan looks up the
// principals one sign-in, lookup or resolution needs, and returns the batch
// of those still missing with the number it found bound. The batch is
// written in one Create; on ErrExist, another call minted something this one
// needs, so plan runs again. Any other failure ends the call: a refusal no
// retry clears, such as an exhausted ID range, with its own class, and the
// store's own failure ErrUnavailable. A record whose name is held is minted
// without it. A re-run fails the call ErrUnavailable, having written
// nothing, when it neither binds more principals than the run before nor
// newly marks a record Unnamed. Newly is per call: a record this call marked
// in an earlier run, known by its SID, is not newly marked again, whatever
// PrincipalID the plan proposes it under this run.
//
// decision: the path re-runs the plan, not the batch, and no count bounds
// the loop. Progress is either step, never their sum: a run that binds one
// more principal progressed whatever it unnamed, and so did one that unnamed
// a record for the first time, which a name another call takes between the
// name check and the commit calls for, since that commit fails ErrExist with
// nothing to bind. Counting a mark per call, not per run, keeps a record whose
// name stays held from passing for progress on every run. Each record can
// make each step once, so the rule ends the re-runs, and the caller's
// deadline bounds them in time. A run with neither step means the clash is
// with a principal the plan cannot bind, such as another entry holding the
// same uid, and running it again would answer the same. Overturned if a
// clash is ever found that a later run resolves without either step.
//
// decision: a mark is known by the record's SID, which every directory
// principal carries, so a plan free to build a fresh PrincipalID each run
// cannot pass a re-mark for progress. A record with no SID, one whose SID the
// mint allocates, is known by its kind and folded name instead: never by
// its PrincipalID, which may change per run and would let the call re-run to
// its deadline. The name may merge two records' marks into one, which can
// only end the call ErrUnavailable sooner, never loop it. Overturned if a
// record can be minted unnamed with neither a SID nor a name.
func (s *Server) mint(ctx context.Context, plan func(context.Context) (entity.MintBatch, int, error)) error {
	var failed entity.MintBatch
	last := -1
	marked := map[string]entity.Principal{} // the holder of each record this call has unnamed, by markKey
	for {
		// The store may not watch ctx, so the loop does: the caller's
		// deadline bounds the re-runs whatever answers them.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		}
		b, bound, err := plan(ctx)
		if err != nil {
			return err
		}
		if len(b.Users) == 0 && len(b.Groups) == 0 {
			return nil
		}
		newly, err := s.unnameHeld(ctx, &b, marked)
		if err != nil {
			return err
		}
		if last >= 0 && bound <= last && newly == 0 {
			s.logClash(ctx, failed)
			return fmt.Errorf("%w: a mint re-run bound or unnamed nothing the run before did not", vfserr.ErrUnavailable)
		}
		err = s.deps.Store.Minter().Create(ctx, b)
		switch {
		case err == nil:
			s.countMints(ctx, b, marked)
			return nil
		case !merrs.IsAlreadyExistsError(err):
			return createErr(err)
		}
		failed, last = b, bound
	}
}

// createCodes is the class of each refusal of Create's that no retry clears.
var createCodes = map[merrs.ErrorCode]error{
	merrs.ErrNoSpace:         vfserr.ErrNoSpace,
	merrs.ErrInvalidArgument: vfserr.ErrInvalid,
}

// createErr is the class of a Create failure other than ErrExist: a refusal
// in createCodes keeps its own, and any other failure is the store's, so
// ErrUnavailable.
func createErr(err error) error {
	var se *merrs.StoreError
	if errors.As(err, &se) && createCodes[se.Code] != nil {
		return fmt.Errorf("%w: %w", createCodes[se.Code], err)
	}
	return fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
}

// unnameHeld marks Unnamed each record of b whose name another principal of
// its kind holds under the principal fold: one the name index holds, or an
// earlier record of its kind in b. It returns how many it marked that marked
// did not hold, and records each one's holder in marked.
func (s *Server) unnameHeld(ctx context.Context, b *entity.MintBatch, marked map[string]entity.Principal) (int, error) {
	users, groups := map[string]entity.Principal{}, map[string]entity.Principal{}
	n := 0
	for i := range b.Users {
		r := &b.Users[i]
		holder, held, err := s.held(ctx, entity.PrincipalUser, r.User.Principal, r.User.Name, r.Unnamed, users)
		if err != nil {
			return 0, err
		}
		if held {
			r.Unnamed = true
			n += mark(marked, markKey(r.User.Principal.Kind, r.User.IDs, r.User.Name), holder)
		}
	}
	for i := range b.Groups {
		r := &b.Groups[i]
		holder, held, err := s.held(ctx, entity.PrincipalGroup, r.Group.Principal, r.Group.Name, r.Unnamed, groups)
		if err != nil {
			return 0, err
		}
		if held {
			r.Unnamed = true
			n += mark(marked, markKey(r.Group.Principal.Kind, r.Group.IDs, r.Group.Name), holder)
		}
	}
	return n, nil
}

// markKey is what a record's mark is known by: its SID, or with none its kind
// and folded name.
func markKey(kind entity.PrincipalKind, ids entity.ProtocolIDs, name string) string {
	for _, id := range ids {
		if id.Scheme == entity.SchemeSID {
			return "sid\x00" + id.Value
		}
	}
	return "name\x00" + kind.String() + "\x00" + entity.FoldPrincipalName(name)
}

// mark records holder as id's in marked, and returns 1 if id was not there.
func mark(marked map[string]entity.Principal, id string, holder entity.Principal) int {
	_, was := marked[id]
	marked[id] = holder
	if was {
		return 0
	}
	return 1
}

// held reports whether name is held by a principal other than p, and by
// which: in batch, the names the batch's earlier records of kind claim, or in
// the name index. A name p keeps is added to batch.
func (s *Server) held(ctx context.Context, kind entity.PrincipalKind, p entity.Principal, name string, unnamed bool, batch map[string]entity.Principal) (entity.Principal, bool, error) {
	if name == "" || unnamed {
		return entity.Principal{}, false, nil
	}
	folded := entity.FoldPrincipalName(name)
	holder, ok := batch[folded]
	if !ok {
		var err error
		holder, err = s.deps.Store.Principals().ByName(ctx, kind, name)
		switch {
		case merrs.IsNotFoundError(err):
			batch[folded] = p
			return entity.Principal{}, false, nil
		case err != nil:
			return entity.Principal{}, false, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
		case holder.ID == p.ID:
			// The record itself exists: Create refuses it ErrExist.
			return entity.Principal{}, false, nil
		}
	}
	return holder, true, nil
}

// countMints counts each record of a committed batch by its kind, and logs
// each record this call unnamed, naming the principal holding its name.
func (s *Server) countMints(ctx context.Context, b entity.MintBatch, marked map[string]entity.Principal) {
	count := func(p entity.Principal, ids entity.ProtocolIDs, name string, unnamed bool) {
		kind := p.Kind.String()
		s.mints.WithLabelValues(kind).Inc()
		if !unnamed {
			return
		}
		s.unnamed.WithLabelValues(kind).Inc()
		if holder, ok := marked[markKey(p.Kind, ids, name)]; ok {
			s.logger.WarnContext(ctx, "identity: principal minted without its name, which another holds",
				"kind", kind, "principal", p.ID.String(), "holder", holder.ID.String())
		}
	}
	for _, r := range b.Users {
		count(r.User.Principal, r.User.IDs, r.User.Name, r.Unnamed)
	}
	for _, r := range b.Groups {
		count(r.Group.Principal, r.Group.IDs, r.Group.Name, r.Unnamed)
	}
}

// logClash logs the first record of b whose spelling another principal
// holds, naming both, after a re-run that bound nothing new. A clash it
// cannot find, the holder deleted since, is logged naming b's first record.
func (s *Server) logClash(ctx context.Context, b entity.MintBatch) {
	try := func(p entity.Principal, ids entity.ProtocolIDs) bool {
		for _, id := range ids {
			holder, err := s.deps.Store.Principals().ByProtocolID(ctx, id)
			if err == nil && holder.ID != p.ID {
				s.logger.WarnContext(ctx, "identity: mint refused, a spelling it needs is held by a principal it cannot bind",
					"principal", p.ID.String(), "holder", holder.ID.String(), "scheme", id.Scheme.String())
				return true
			}
		}
		return false
	}
	for _, r := range b.Users {
		if try(r.User.Principal, r.User.IDs) {
			return
		}
	}
	for _, r := range b.Groups {
		if try(r.Group.Principal, r.Group.IDs) {
			return
		}
	}
	var first entity.Principal
	switch {
	case len(b.Users) > 0:
		first = b.Users[0].User.Principal
	case len(b.Groups) > 0:
		first = b.Groups[0].Group.Principal
	default:
		return
	}
	s.logger.WarnContext(ctx, "identity: mint refused, its re-run bound nothing new", "principal", first.ID.String())
}
