package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

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
)

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
// disabled or deleted user is refused on the first call after the change.
//
// The forwarded Identity is checked first, before any store read: evidence
// naming a principal below ProofSecret, or none at ProofSecret or above, is
// ErrInvalid. Then the map step: a proven identity maps to the principal
// sign-in bound its evidence to, never to its name again, and that
// principal's user record is re-read, a disabled or deleted user refused
// ErrAccess; a ProofNone identity maps to the share's Anonymous. A call
// naming no share stops there, with Mapped set.
//
// On a share, a caller the policy squashes, a ProofNone one, or a proven one
// whose user holds no uid acts as the policy's Anonymous, with its groups and
// no flag; a policy naming none refuses it, as does a deleted or disabled
// Anonymous. Otherwise a proven caller acts as itself, authenticated, admin
// where its share grant names admin, and privileged where it is admin or
// holds uid 0. A proven caller's share grant is read for the principal it
// mapped to, before any squash; no other caller's grant is read, so its
// Access is none.
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
		c, err = s.resolve(ctx, span, st.serial(), id, p, narrowed)
		if st.serial() == s.Current() {
			break
		}
	}
	return c, err
}

// resolve is one pass of Resolve's map step, squash and grant under the
// State whose serial is built, which it stamps on the caller.
func (s *Server) resolve(ctx context.Context, span trace.Span, built uint64, id identity.Identity, p entity.SharePolicy, narrowed identity.ReadOnlyCause) (c *identity.Caller, err error) {
	pr := s.deps.Store.Principals()
	proven := id.Proof >= identity.ProofSecret
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
	c = &identity.Caller{Mapped: mapped, Fresh: id.Evidence.Expires, Built: built}

	// decision: a call naming no share, such as a client record's creation,
	// runs the map step alone, under no share's policy, and acts as Mapped:
	// no squash, group or grant applies outside a share. Overturned if a call
	// naming no share ever acts on a share's files.
	if p.Share == (entity.ShareID{}) {
		return c, nil
	}

	root := proven && slices.Contains(u.IDs, entity.ProtocolID{Scheme: entity.SchemeUID, Value: "0"})
	fileUser := proven && slices.ContainsFunc(u.IDs, func(i entity.ProtocolID) bool { return i.Scheme == entity.SchemeUID })
	squashed := !fileUser || p.Squash == entity.SquashAll || root && p.Squash == entity.SquashRoot
	if fileUser {
		if c.Access, err = pr.Grant(ctx, p.Share, mapped); err != nil {
			return nil, unavailable(err)
		}
		// decision: a squashed proven caller whose own grant is none is
		// refused here, at resolution, and so is not audited, while an
		// unsquashed one is refused by the store's check and audited there.
		// The store reads the grants of the principal a caller acts as, and a
		// squashed caller acts as another. Overturned once resolution's
		// refusals are audited, or the store takes a grant of none.
		if squashed && c.Access == entity.AccessNone {
			return nil, fmt.Errorf("%w: the share grants the caller none", vfserr.ErrAccess)
		}
	}
	if squashed {
		if p.Anonymous.IsZero() {
			return nil, fmt.Errorf("%w: the share names no anonymous principal", vfserr.ErrAccess)
		}
		c.Principal = p.Anonymous
		if _, err := live(ctx, pr, c.Principal, ""); err != nil {
			return nil, err
		}
	} else {
		c.Principal, c.Authenticated = mapped, true
		c.Admin = c.Access == entity.AccessAdmin
		c.Privileged = c.Admin || root
	}
	c.ReadOnly = narrowed
	for g, err := range pr.Groups(ctx, c.Principal) {
		if err != nil {
			return nil, unavailable(err)
		}
		c.Groups = append(c.Groups, g)
	}
	return c, nil
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

// live is p's user record, refusing a user disabled or deleted, counted
// under reason when it names one; any other principal passes, with the zero
// record.
func live(ctx context.Context, pr kvstore.Principals, p entity.Principal, reason string) (entity.User, error) {
	if p.Kind != entity.PrincipalUser {
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
