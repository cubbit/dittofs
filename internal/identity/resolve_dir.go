package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// reasonDirectoryUnavailable counts a resolution refused for want of a
// directory answer.
const reasonDirectoryUnavailable = "directory_unavailable"

// refreshFailedSpec counts the refreshes of a directory principal's name or
// e-mail address a resolution could not write, which it carries on past.
var refreshFailedSpec = metrics.Spec{
	Name:   "identity_directory_refresh_failures_total",
	Help:   "Refreshes of a directory principal's name or e-mail address that resolution could not write, by kind.",
	Labels: []string{"kind"},
}

// callerTTL is the longest a caller of a directory principal is served after
// it was built (RFC 18 §7.1).
const callerTTL = 5 * time.Minute

// dirEntryID is the entry ID of u's dir spelling; false for a local
// principal, which holds none.
func dirEntryID(u entity.User) ([]byte, bool) {
	for _, id := range u.IDs {
		if id.Scheme == entity.SchemeDir {
			return []byte(id.Value), true
		}
	}
	return nil, false
}

// dirRefusal is err, a directory question that went unanswered, as a
// refusal counted directory_unavailable and failing ErrUnavailable.
func dirRefusal(err error) error {
	if !errors.Is(err, vfserr.ErrUnavailable) {
		err = fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)
	}
	return &refusal{reasonDirectoryUnavailable, err}
}

// dirStep is resolution's directory step for p, the directory principal
// whose dir spelling is entryID, under st. Its entry is read through the
// directory cache on every resolution, never from the store (RFC 18 §3.4):
// "no such entry" or an entry the directory shows disabled refuses the call
// ErrAccess, counted disabled. A found one refreshes p's record, and the
// closure's groups already minted, from the answers a query of this step
// gave (see refreshable), and a refresh that fails is logged and counted,
// never a refusal. With groups set, p's directory closure is read through the
// cache too, and the principal of each group of it and of each SID of ev of
// the directory's domain that no principal holds is returned, those not yet
// minted minted in one batch, the SIDs asked in batches. Every directory
// question that goes unanswered, fresh or within the stale grace, fails the
// call ErrUnavailable, counted directory_unavailable, never a caller without
// what it would have read. oldest is when the oldest answer it used was
// given. The step opens the 20-second bound once, which each of its queries,
// mints and waits inherits.
func (s *Server) dirStep(ctx context.Context, st *State, p entity.Principal, entryID []byte, ev identity.Evidence, groups bool) (dir []entity.Principal, oldest time.Time, err error) {
	if st == nil || st.dir == nil {
		return nil, time.Time{}, dirRefusal(errors.New("identity: a directory principal with no directory"))
	}
	ctx, cancel := directory.Bounded(ctx, s.deps.Clock, directoryBound, errDirectoryBound)
	defer cancel()
	grace := st.Settings.DirectoryStaleGrace
	e, queried, err := st.dir.lookupEntry(ctx, directory.ByEntryID, entryID, grace, false)
	if err != nil {
		return nil, time.Time{}, dirRefusal(err)
	}
	// "No such entry" for a principal the directory once minted is the
	// account gone, read as disabled, never as "no change"; nothing is
	// written, and the 30-second negative answer refuses the calls after it
	// without a query.
	if !e.found || !e.ans.Enabled {
		return nil, time.Time{}, &refusal{reasonDisabled, fmt.Errorf("%w: the directory account is disabled or gone", vfserr.ErrAccess)}
	}
	oldest = e.at
	if queried {
		s.refreshOrLog(ctx, st, []dirBound{{p, e.ans}})
	}
	if !groups {
		return nil, oldest, nil
	}
	g, groupsQueried, err := st.dir.groupsOf(ctx, e.ans, grace)
	if err != nil {
		return nil, time.Time{}, dirRefusal(err)
	}
	oldest = earliest(oldest, g.at)
	var domain []byte
	if st.Directory.IsAD() {
		if domain, err = st.dir.domainSID(ctx); err != nil {
			return nil, time.Time{}, dirRefusal(err)
		}
	}
	entries := s.mintableGroups(domain, g.groups)
	closure := len(entries) // the entries the closure answer gave; the evidence's SIDs' follow
	if domain != nil {
		more, at, err := s.evidenceGroups(ctx, st, domain, ev)
		if err != nil {
			return nil, time.Time{}, err
		}
		oldest = earliest(oldest, at)
		entries = append(entries, s.mintableGroups(domain, more)...)
	}
	ps, bound, err := s.mintDir(ctx, st, entries, false)
	if err != nil {
		var r *refusal
		if !errors.As(err, &r) && errors.Is(err, vfserr.ErrUnavailable) {
			err = &refusal{reasonStoreUnavailable, err}
		}
		return nil, time.Time{}, err
	}
	// A SID answer is never refreshed from: it says whom a SID names, and a
	// lookup by entry ID settles that record's name.
	s.refreshOrLog(ctx, st, refreshable(ps, entries, bound, func(i int) bool { return groupsQueried && i < closure }))
	for _, q := range ps {
		if !q.IsZero() {
			dir = append(dir, q)
		}
	}
	return dir, oldest, nil
}

// evidenceGroups is the directory's group entry for each SID of ev, enabled
// or deny-only, that is of domain's and that no principal holds, asked of the
// directory in batches: a SID no entry holds, or one held by a user's entry,
// is left out.
//
// decision: a deny-only SID no principal holds is minted as an enabled one
// is. Leaving it unminted would leave the deny-only list without a principal
// that a later mint could give a deny entry to; minting it costs a record and
// grants nothing, since collect keeps it out of the caller's groups.
// Overturned if minting from a deny-only SID is found to cost more than a
// record per group.
func (s *Server) evidenceGroups(ctx context.Context, st *State, domain []byte, ev identity.Evidence) (out []directory.Answer, oldest time.Time, err error) {
	var ask [][]byte
	for _, list := range [][]identity.SID{ev.SIDs, ev.DenyOnly} {
		for i := range list {
			var b bytes.Buffer
			sid.EncodeSID(&b, &list[i])
			if !inDomain(domain, b.Bytes()) {
				continue
			}
			if _, ok, err := bySID(ctx, s.deps.Store.Principals(), list[i]); err != nil {
				return nil, time.Time{}, err
			} else if !ok {
				ask = append(ask, b.Bytes())
			}
		}
	}
	if len(ask) == 0 {
		return nil, time.Time{}, nil
	}
	found, oldest, err := st.dir.lookupSIDs(ctx, ask, st.Settings.DirectoryStaleGrace)
	if err != nil {
		return nil, time.Time{}, dirRefusal(err)
	}
	for _, a := range found {
		if a.Group {
			out = append(out, a)
		}
	}
	return out, oldest, nil
}

// refreshOrLog refreshes bound, logging and counting a failure rather than
// returning it.
//
// decision: a resolution whose refresh fails carries on. The name and the
// e-mail address are not part of the caller, so refusing it would cost a
// user every file call, the deletes that would free a full store among
// them, for a write it does not need; the next resolution past the TTL
// tries again. Overturned if a caller ever carries the name.
func (s *Server) refreshOrLog(ctx context.Context, st *State, bound []dirBound) {
	if len(bound) == 0 {
		return
	}
	if err := s.refresh(ctx, st, bound); err != nil {
		for _, b := range bound {
			s.refreshErr.WithLabelValues(b.p.Kind.String()).Inc()
		}
		s.logger.WarnContext(ctx, "identity: directory principal not refreshed at resolution", "records", len(bound), "error", err)
	}
}

// earliest is the earlier of a and b, a zero one ignored.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

// decision: a caller of a directory principal is served for up to callerTTL
// after it was built, so a directory disable or a group removed reaches a
// cached caller up to 5 minutes late, and up to the TTL plus the grace late
// when the answers it used were served stale (RFC 18 §7.1, §5.3). A change
// to the principal's own record raises the principals generation, and the
// State applied with it drops the caller. Overturned if an operator needs a directory
// disable to stop a cached caller sooner than the TTL.
//
// freshUntil is when a caller built at now from directory answers the oldest
// of which was given at oldest stops being served, given expires, the
// evidence's own end, zero for none: callerTTL after now, or the TTL plus the
// grace after oldest, or expires, whichever comes first. So a caller built
// from an answer served stale never outlives that answer's bound.
func freshUntil(now, oldest, expires time.Time, grace time.Duration) time.Time {
	t := now.Add(callerTTL)
	if b := oldest.Add(dirFound + grace); b.Before(t) {
		t = b
	}
	if !expires.IsZero() && expires.Before(t) {
		return expires
	}
	return t
}
