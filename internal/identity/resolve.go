package identity

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The reasons identity_resolution_refusals_total counts a refusal under.
const (
	reasonDisabled         = "disabled"
	reasonStoreUnavailable = "store_unavailable"
	reasonGroupCap         = "group_cap"
	reasonAnonymousMissing = "anonymous_missing"
	reasonNTLMOff          = "ntlm_off"
)

// groupCap is the most groups a caller may hold; one in more is refused.
//
// decision: a principal over the group cap is refused, not truncated. A
// truncated group list is a wrong answer that looks right, and it can drop
// the group a deny entry names. 1 024 sits at the SID limit of a
// directory-issued access token, so a real account rarely meets it.
//
// ponytail: the cap is a constant. Make it a setting, or per share, when a
// deployment shows accounts above it.
const groupCap = 1024

// refusal is a refusal counted under reason. It unwraps to the error the
// call answers.
type refusal struct {
	reason string
	err    error
}

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

// unavailable is a store error: never "no such principal", never anonymous
// or a default grant.
func unavailable(err error) error {
	return &refusal{reasonStoreUnavailable, fmt.Errorf("%w: %w", vfserr.ErrUnavailable, err)}
}

// noSpan stands in for the span of a resolution that opens none.
var noSpan = trace.SpanFromContext(context.Background())

// Resolve resolves id to the Caller it acts as on the share p governs,
// narrowed to read-only by narrowed. It is admission's resolution step: it
// keeps no table of callers, and reads the store on every call, so a
// disabled or deleted user or machine is refused on the first call after the
// change.
//
// The forwarded Identity is checked first, before any store read: evidence
// naming a principal below ProofSecret, or none at ProofSecret or above, is
// ErrInvalid. A ProofSecret identity, NTLM's, is refused ErrAccess while the
// State's identity.ntlm is off, or before any State is applied. Then the map
// step: a proven identity maps to the principal sign-in bound its evidence
// to, never to its name again, and that principal's user or machine record is
// re-read, one disabled or deleted refused ErrAccess; a well-known principal,
// root or anonymous, has no record and none is read. A ProofNone identity
// maps to the share's Anonymous, the well-known anonymous where the policy
// names none, a call naming no share included. A call naming no share stops
// there, with Mapped set.
//
// On a share, a caller the policy squashes, a ProofNone one, or a proven one
// whose user holds no uid acts as the policy's Anonymous, with that
// principal's closure for its groups, no deny-only principal and no flag; a
// deleted or disabled Anonymous refuses it. A caller mapped to the
// well-known root holds uid 0: a root squash replaces it, and unsquashed it
// is privileged.
// Otherwise a proven caller's groups are its closure unioned with those of
// its evidence's SIDs, its deny-only SIDs mapped apart. A caller in more than
// groupCap groups is refused.
//
// Access is the share's grant (see grant): for a proven caller with a uid,
// read for the principal it mapped to, with that principal's own groups,
// authenticated, before any squash; for any other caller, for the Anonymous
// it acts as, with Anonymous's groups, unauthenticated. A squashed proven
// caller whose grant is none is refused. Access is then narrowed to read
// where admission narrowed the call, and ReadOnly says why it is read. An
// unsquashed proven caller is authenticated, admin where its grant is admin,
// and privileged where it is admin or holds uid 0; no other caller holds a
// flag.
//
// A store error is ErrUnavailable, never a caller.
//
// The caller's Built is the serial of the State the resolution loaded before
// its first store read. A resolution overtaken by an Apply resolves once
// more, and answers that second result.
func (s *Server) Resolve(ctx context.Context, id identity.Identity, p entity.SharePolicy, narrowed identity.ReadOnlyCause) (c *identity.Caller, err error) {
	// Only a sampled request's resolution opens a span: under an unsampled
	// parent, Start would still allocate a span nobody records.
	span := noSpan
	if trace.SpanContextFromContext(ctx).IsSampled() {
		ctx, span = s.tracer.Start(ctx, "identity.resolve", trace.WithAttributes(attribute.Int("identity.proof", int(id.Proof))))
	}
	defer func() { s.finish(span, err) }()

	if err := forwarded(id); err != nil {
		return nil, err
	}
	// ponytail: a claimed identity is refused, never made anonymous, since
	// mapping a claimed uid and gid to principals is not built. Upgrade to
	// RFC 18 §4.3's mapping, with its groups and root by a claimed uid 0,
	// when the NFS side needs claimed callers resolved.
	if id.Proof == identity.ProofClaimed {
		return nil, fmt.Errorf("%w: a claimed identity is not resolved", vfserr.ErrAccess)
	}

	// The State is loaded once per pass, before its first store read, and the
	// caller stamped with it. A pass overtaken by an Apply runs once more under
	// the State then current, and that second answer is served whatever was
	// applied meanwhile, so a steady rate of Applies never starves a call.
	for range 2 {
		st := s.state.Load()
		if s.loaded != nil {
			s.loaded()
		}
		c, err = s.resolve(ctx, span, st, id, p, narrowed)
		if st.serial() == s.Current() {
			break
		}
	}
	return c, err
}

// resolve is one pass of Resolve's map step, squash and grant under st,
// whose serial it stamps on the caller.
func (s *Server) resolve(ctx context.Context, span trace.Span, st *State, id identity.Identity, p entity.SharePolicy, narrowed identity.ReadOnlyCause) (c *identity.Caller, err error) {
	// A Secret caller is refused under the State that turned NTLM off, so
	// a session signed in before the change is refused at its next call.
	if id.Proof == identity.ProofSecret && (st == nil || !st.Settings.NTLM) {
		return nil, &refusal{reasonNTLMOff, fmt.Errorf("%w: NTLM is off", vfserr.ErrAccess)}
	}
	pr := s.deps.Store.Principals()
	proven := id.Proof >= identity.ProofSecret
	// decision: a policy naming no Anonymous acts as the well-known anonymous,
	// never the zero principal, which owns every share's root, and is not
	// refused: the well-known anonymous has no record to delete and no group,
	// and no grant may name it, so it reaches what an operator opened to
	// everyone and nothing more. Overturned if a zero
	// Anonymous is ever meant as "refuse squashed callers"; that then needs
	// its own policy field.
	if p.Anonymous.IsZero() {
		p.Anonymous = entity.AnonymousPrincipal
	}
	mapped := p.Anonymous
	if proven {
		mapped = id.Evidence.Principal
	}
	span.SetAttributes(attribute.String("identity.principal", mapped.ID.String()))
	var u entity.User
	if proven {
		if u, err = live(ctx, pr, mapped, reasonDisabled); err != nil {
			return nil, err
		}
	}

	// decision: Fresh is the evidence's Expires, zero for NTLM and for every
	// unproven caller, and resolution never checks it: the adapter refuses a
	// request on a context past its Expires. A cache never stores a caller
	// with a zero Fresh, so a local principal resolves on every call and its
	// disable reaches its next call. Overturned once each new State reaches
	// a running node, which then bounds such a caller instead.
	c = &identity.Caller{Mapped: mapped, Fresh: id.Evidence.Expires, Built: st.serial()}

	// decision: a call naming no share, such as a client record's creation,
	// runs the map step alone, under no share's policy, and acts as Mapped:
	// no squash, group or grant applies outside a share. Overturned if a call
	// naming no share ever acts on a share's files.
	if p.Share == (entity.ShareID{}) {
		return c, nil
	}

	// decision: a proven caller mapped to the well-known root is a file user
	// with uid 0 though no record holds one, and unsquashed it is privileged
	// with no admin grant, as the superuser is on a POSIX server. Only a name
	// an operator linked to root maps to it, so the link is the grant; a
	// share that squashes root takes the privilege away. Overturned if root
	// on one client must be told apart from root on another.
	root := proven && mapped == entity.RootPrincipal
	fileUser := root || proven && slices.ContainsFunc(u.IDs, func(i entity.ProtocolID) bool { return i.Scheme == entity.SchemeUID })
	squashed := !fileUser || p.Squash == entity.SquashAll || root && p.Squash == entity.SquashRoot
	// decision: a claimed identity is squashed before its grant is read, a
	// proven one after. A grant on a claimed uid has no security value, since
	// any client can claim an administrator's uid; a proven principal's grant
	// names who the proof established, so it decides who may reach a
	// squashed share. Overturned if a proof kind between Claimed and Secret
	// is added; it then needs its own order.
	var groups, deny []entity.Principal
	if fileUser {
		if groups, deny, err = collect(ctx, pr, mapped, id.Evidence); err != nil {
			return nil, err
		}
		if err := s.capped(ctx, mapped, groups); err != nil {
			return nil, err
		}
		if c.Access, err = grant(ctx, pr, p.Share, mapped, groups, true); err != nil {
			return nil, err
		}
		// decision: a squashed proven caller whose grant is none, its own
		// none or no grant at all, is refused here, at resolution, whatever
		// the call, while an unsquashed one resolves with Access none, which
		// admission refuses unless the call only releases state. Neither
		// refusal is audited. So a squashed caller whose grant was removed
		// cannot release the opens and locks it holds. Overturned once
		// resolution's or admission's refusals are audited, or once
		// resolution is told a call only releases state, to admit that
		// squashed caller as admission admits the unsquashed one.
		if squashed && c.Access == entity.AccessNone {
			return nil, fmt.Errorf("%w: the share grants the caller none", vfserr.ErrAccess)
		}
	}
	// decision: squashing replaces the whole principal. A squashed caller
	// acts as the policy's Anonymous with that principal's groups alone, no
	// deny-only principal and every flag false: a squash that kept the
	// mapped principal's groups or SIDs would leave every ACL entry naming
	// them matching. It applies to every protocol alike, so a share's policy
	// means one thing whatever the protocol. Revisit if an operator needs
	// NFS squashed and SMB not on one share; that is two shares today.
	if squashed {
		c.Principal = p.Anonymous
		if _, err := live(ctx, pr, c.Principal, reasonAnonymousMissing); err != nil {
			return nil, err
		}
		if c.Groups, err = closure(ctx, pr, c.Principal); err != nil {
			return nil, err
		}
		if err := s.capped(ctx, c.Principal, c.Groups); err != nil {
			return nil, err
		}
		if !fileUser {
			if c.Access, err = grant(ctx, pr, p.Share, c.Principal, c.Groups, false); err != nil {
				return nil, err
			}
		}
	} else {
		c.Principal, c.Groups, c.DenyOnly = mapped, groups, deny
		c.Authenticated = true
		c.Admin = c.Access == entity.AccessAdmin
		c.Privileged = c.Admin || root
	}
	slices.SortFunc(c.Groups, comparePrincipals)
	switch {
	case c.Access == entity.AccessNone:
	case narrowed != identity.ReadOnlyNotNarrowed:
		c.Access, c.ReadOnly = entity.AccessRead, narrowed
	case c.Access == entity.AccessRead:
		c.ReadOnly = identity.ReadOnlyGrant
	}
	return c, nil
}

// capped refuses p's groups when they number more than groupCap, logging
// the count.
//
// decision: deny-only principals do not count toward the cap. It caps
// groups, and a deny-only principal is never one: counting it would refuse a
// restricted token for the denies it carries. Both are still mapped whole or
// the call fails. Overturned if the cap is read to bound a token's SIDs,
// deny-only included.
func (s *Server) capped(ctx context.Context, p entity.Principal, groups []entity.Principal) error {
	if n := len(groups); n > groupCap {
		s.logger.WarnContext(ctx, "identity: caller refused over the group cap", "principal", p.ID.String(), "groups", n, "cap", groupCap)
		return &refusal{reasonGroupCap, fmt.Errorf("%w: the caller is in %d groups, over the cap of %d", vfserr.ErrAccess, n, groupCap)}
	}
	return nil
}

// grant is of's access to share, read from the share's grants in order,
// the first that matches deciding: the grant naming of itself, whatever it
// says; the widest grant naming one of groups; for an authenticated caller,
// the grant naming authenticated users; the grant naming everyone; else
// none. groups never holds a deny-only principal, so a deny-only principal's
// grant never admits.
//
// decision: a principal's own grant overrides its groups', including a
// none. That is how an operator blocks one user of a group without leaving
// the group, and it is the only way to express it with four access levels.
// The cost is that a user granted read alone misses a group's read-write.
// Revisit if operators are shown to expect group grants to widen a user's.
//
// ponytail: reads one grant per group, up to groupCap point reads on a
// cache miss. Read the share's grant list once, under its grant-list
// version, when a profile shows misses dominated by grant reads.
func grant(ctx context.Context, pr kvstore.Principals, share entity.ShareID, of entity.Principal, groups []entity.Principal, authenticated bool) (entity.ShareAccess, error) {
	if a, ok, err := pr.Grant(ctx, share, of); err != nil || ok {
		return a, storeErr(err)
	}
	var access entity.ShareAccess
	found := false
	for _, g := range groups {
		a, ok, err := pr.Grant(ctx, share, g)
		if err != nil {
			return 0, unavailable(err)
		}
		if ok {
			found, access = true, max(access, a)
		}
	}
	if found {
		return access, nil
	}
	defaults := []entity.Principal{entity.Everyone}
	if authenticated {
		defaults = []entity.Principal{entity.AuthenticatedUsers, entity.Everyone}
	}
	for _, d := range defaults {
		if a, ok, err := pr.Grant(ctx, share, d); err != nil || ok {
			return a, storeErr(err)
		}
	}
	return entity.AccessNone, nil
}

// storeErr is err as unavailable, nil kept.
func storeErr(err error) error {
	if err == nil {
		return nil
	}
	return unavailable(err)
}

// closure is every group p is in, directly or through nested groups, each
// once, none in stop nor reached only through one. A failed read fails it
// whole: a partial list can grant access by leaving out the group a deny
// entry names.
func closure(ctx context.Context, pr kvstore.Principals, p entity.Principal, stop ...entity.Principal) ([]entity.Principal, error) {
	var out []entity.Principal
	for g, err := range pr.Groups(ctx, p, stop...) {
		if err != nil {
			return nil, unavailable(err)
		}
		out = append(out, g)
	}
	return out, nil
}

// collect is a proven caller's groups and deny-only principals. Its groups
// are the closure of its store memberships, unioned with each group an
// evidence SID maps to and that group's closure, so a deny entry naming a
// parent of a PAC's group still matches; each listed once. Each
// deny-only SID maps to a principal in deny, never in groups, and adds none
// of its groups to groups: a group the store reaches only through it goes
// to deny too. collect alone keeps the two lists disjoint. A SID no
// principal holds stays unmapped; a lookup error mapping any SID fails the
// whole collection, since dropping a deny fails open.
//
// decision: a deny-only principal stops every closure walk, so a group the
// store reaches only through it (a parent of a deny-only Finance) is never
// in groups; it is in deny, with the deny-only principal itself. A group the
// PAC lists enabled, or that another membership reaches, stays in groups
// and out of deny. Both sides fail closed: such a parent grants nothing,
// since the directory marked the SID deny-only and RFC 18 §6.3 says it adds
// none of its groups, and a deny entry naming it still matches, since a PAC
// never removes a group the store records. The cost is that a parent the
// directory meant to drop entirely still denies. Overturned if a deny-only
// membership must not carry its parents' denies.
func collect(ctx context.Context, pr kvstore.Principals, of entity.Principal, ev identity.Evidence) (groups, deny []entity.Principal, err error) {
	for _, sd := range ev.DenyOnly {
		d, ok, err := bySID(ctx, pr, sd)
		if err != nil {
			return nil, nil, err
		}
		if ok && !slices.Contains(deny, d) {
			deny = append(deny, d)
		}
	}
	slices.SortFunc(deny, comparePrincipals)
	seen := map[entity.Principal]bool{}
	for _, d := range deny {
		seen[d] = true
	}
	add := func(gs []entity.Principal) {
		for _, g := range gs {
			if !seen[g] {
				seen[g] = true
				groups = append(groups, g)
			}
		}
	}
	gs, err := closure(ctx, pr, of, deny...)
	if err != nil {
		return nil, nil, err
	}
	add(gs)
	for _, sd := range ev.SIDs {
		g, ok, err := bySID(ctx, pr, sd)
		if err != nil {
			return nil, nil, err
		}
		// The user's own SID, first in a PAC, maps to a user, not a group:
		// its closure is the mapped principal's, already collected. A SID
		// also marked deny-only stays deny-only.
		if !ok || g.Kind != entity.PrincipalGroup || slices.Contains(deny, g) {
			continue
		}
		if gs, err = closure(ctx, pr, g, deny...); err != nil {
			return nil, nil, err
		}
		add(append(gs, g))
	}
	// A parent the store reaches only through a deny-only principal is not
	// in seen yet: it joins deny, so its deny entries still match and it
	// grants nothing.
	for _, d := range slices.Clone(deny) {
		ps, err := closure(ctx, pr, d)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				deny = append(deny, p)
			}
		}
	}
	slices.SortFunc(deny, comparePrincipals)
	return groups, deny, nil
}

// bySID is the principal holding sd's spelling; false when none does.
func bySID(ctx context.Context, pr kvstore.Principals, sd identity.SID) (entity.Principal, bool, error) {
	var b bytes.Buffer
	sid.EncodeSID(&b, &sd)
	p, err := pr.ByProtocolID(ctx, entity.ProtocolID{Scheme: entity.SchemeSID, Value: b.String()})
	if merrs.IsNotFoundError(err) {
		return p, false, nil
	}
	if err != nil {
		return p, false, unavailable(err)
	}
	return p, true, nil
}

func comparePrincipals(a, b entity.Principal) int {
	if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return bytes.Compare(a.ID[:], b.ID[:])
}

// forwarded refuses an Identity inconsistent with itself: evidence below
// ProofSecret, which no sign-in established, or none naming a principal at
// ProofSecret or above, which every sign-in binds.
func forwarded(id identity.Identity) error {
	ev := id.Evidence
	switch {
	case id.Proof > identity.ProofTicket:
		return fmt.Errorf("%w: unknown proof kind %d", vfserr.ErrInvalid, id.Proof)
	case id.Proof >= identity.ProofSecret && ev.Principal.IsZero():
		return fmt.Errorf("%w: a proven identity whose evidence names no principal", vfserr.ErrInvalid)
	case id.Proof < identity.ProofSecret && (!ev.Principal.IsZero() || ev.Name != "" || len(ev.SIDs) > 0 || len(ev.DenyOnly) > 0 || !ev.Expires.IsZero()):
		return fmt.Errorf("%w: evidence on an unproven identity", vfserr.ErrInvalid)
	}
	return nil
}

// live is p's user or machine record, refusing one disabled or deleted,
// counted under reason when it names one; any other principal passes, with
// the zero record.
func live(ctx context.Context, pr kvstore.Principals, p entity.Principal, reason string) (entity.User, error) {
	if p.Kind != entity.PrincipalUser && p.Kind != entity.PrincipalMachine {
		return entity.User{}, nil
	}
	u, err := pr.User(ctx, p)
	if err != nil && !merrs.IsNotFoundError(err) {
		return u, unavailable(err)
	}
	if err != nil || u.Disabled {
		refused := fmt.Errorf("%w: the principal is disabled or deleted", vfserr.ErrAccess)
		if reason == "" {
			return u, refused
		}
		return u, &refusal{reason, refused}
	}
	return u, nil
}

// finish counts a refusal under its reason and ends the span with the
// outcome. No attribute of the span carries a name or a SID.
func (s *Server) finish(span trace.Span, err error) {
	outcome := "ok"
	var r *refusal
	switch {
	case errors.As(err, &r):
		outcome = r.reason
		s.refusals.WithLabelValues(r.reason).Inc()
	case errors.Is(err, vfserr.ErrInvalid):
		outcome = "invalid"
	case err != nil:
		outcome = "refused"
	}
	span.SetAttributes(attribute.String("identity.outcome", outcome))
	if err != nil {
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}
