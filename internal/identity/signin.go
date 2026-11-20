package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// outcome is how one sign-in attempt ended, as the logon event and the
// sign-in series name it (RFC 18 §9.2); empty for a step that is no attempt.
type outcome string

const (
	outcomeOK            outcome = "ok"
	outcomeLogonFailure  outcome = "logon_failure"
	outcomeDisabled      outcome = "disabled"
	outcomeInvalid       outcome = "invalid"
	outcomeUnavailable   outcome = "unavailable"
	outcomeUnknownDomain outcome = "unknown_domain"
	outcomeThrottled     outcome = "throttled"
)

// SignIn verifies one handshake round under one State: a local user's
// NTLMv2 response (RFC 18 §3.5), refused before its credential is read once
// its name and client address are past the logon throttle's limit (§3.1). It
// reads the store and the secret provider only, and writes nothing.
func (s *Server) SignIn(ctx context.Context, step identity.SignInStep) (identity.SignInResult, error) {
	res, _, err := s.signIn(ctx, step)
	return res, err
}

// signIn is SignIn with the attempt's outcome.
//
// decision: on any error the result is zero: no Identity, no Keys, and never
// a guest or anonymous one. RFC 18 §3.6 has no "map to guest": a wrong
// password answered with a guest session turns every typo into a session
// the client did not ask for, and a store outage into access. Null and guest
// sessions are set up by the adapter's mechanism without SignIn. Every path
// below answers a zero result with its error already; the check after
// signInNTLM is the one place that holds the rule for any path added later.
func (s *Server) signIn(ctx context.Context, step identity.SignInStep) (identity.SignInResult, outcome, error) {
	a := step.NTLM
	// decision: a step carrying neither or both of an AP-REQ and an
	// AUTHENTICATE, or an AUTHENTICATE naming no user, is ErrInvalid with no
	// outcome: it is no attempt of either mechanism, and the adapter sets up
	// null and guest sessions without SignIn (RFC 20 §4.4), so one reaching
	// here is a caller's bug, refused, never a session. Overturned if the
	// adapter is ever meant to hand identity an anonymous AUTHENTICATE.
	switch {
	case (a == nil) == (len(step.Kerberos) == 0):
		return identity.SignInResult{}, "", fmt.Errorf("%w: a sign-in step carries exactly one of an AP-REQ and an AUTHENTICATE", vfserr.ErrInvalid)
	case a == nil:
		return s.signInKerberos(ctx, step)
	case a.User == "":
		return identity.SignInResult{}, "", fmt.Errorf("%w: an AUTHENTICATE with no user name", vfserr.ErrInvalid)
	}
	// decision: the throttle is asked before every refusal of the message,
	// NTLM off, NTLMv1 or LM and a malformed response, and every
	// ErrLogonFailure but a throttled one counts, those refusals included,
	// so no mechanism the installation refuses lets a client try names
	// uncounted. ErrInvalid tells nothing of the password and ErrUnavailable
	// is an outage, so neither counts: an outage never locks a right
	// password out. Overturned if a refusal is ever found to count a
	// client's legitimate retries against it.
	settle, ok := s.throttle.reserve(a.User, step.Client, s.deps.Clock.Now())
	if !ok {
		return identity.SignInResult{}, outcomeThrottled, vfserr.ErrLogonFailure
	}
	failed := false
	defer func() { settle(failed) }() // on every return and a panic, so no slot stays reserved
	st := s.state.Load()
	if st == nil {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	res, oc, err := s.signInNTLM(ctx, st, step)
	if err != nil {
		failed = errors.Is(err, vfserr.ErrLogonFailure)
		return identity.SignInResult{}, oc, err
	}
	return res, oc, nil
}

// signInNTLM refuses, in order, NTLM turned off, an NTLMv1 or LM response, a
// malformed response or key exchange and a domain no row routes, each before
// any credential is read, then checks the response against the local user
// the message names.
func (s *Server) signInNTLM(ctx context.Context, st *State, step identity.SignInStep) (identity.SignInResult, outcome, error) {
	a := step.NTLM
	if !st.Settings.NTLM {
		return identity.SignInResult{}, outcomeLogonFailure, fmt.Errorf("%w: NTLM is off", vfserr.ErrLogonFailure)
	}
	proof, blob, avFlags, err := parseNTLMv2(a.NTResponse)
	if err == nil {
		err = checkKeyExchange(a.Flags, a.EncryptedSessionKey)
	}
	switch {
	case errors.Is(err, vfserr.ErrInvalid):
		return identity.SignInResult{}, outcomeInvalid, err
	case err != nil:
		return identity.SignInResult{}, outcomeLogonFailure, err
	}
	if !localDomain(st, a.User, a.Domain) {
		return identity.SignInResult{}, outcomeUnknownDomain, fmt.Errorf("%w: domain routes nowhere", vfserr.ErrLogonFailure)
	}
	return s.signInLocal(ctx, step, proof, blob, avFlags)
}

// localDomain is the domain table's local row (RFC 18 §3.5): a user name
// holding neither '@' nor '\', since a local name never holds either, sent
// with an empty domain or the installation's NetBIOS name, ASCII letters
// folded only, so the Kelvin sign never matches a K. Every other domain is
// unknown until pass-through adds its row.
//
// decision: a UPN under one of the forest's alternate suffixes, or a domain
// written with a trailing dot, is unknown_domain: nothing reads the forest's
// suffixes (RFC 18 §3.5). Overturned when a deployment's users sign in with
// an alternate suffix.
func localDomain(st *State, user, domain string) bool {
	return !strings.ContainsAny(user, `@\`) &&
		(domain == "" || equalFoldASCII(domain, st.Settings.NetBIOSName))
}

// signInLocal finds the user the message names and verifies the response
// against its NT hash, with the user and domain as the message names them
// and no other domain (MS-NLMP 3.3.2), then derives the exported session key
// and checks the MIC (MS-NLMP 3.1.5.1.2, 3.2.5.1.2).
//
// decision: every refusal does a wrong password's work (RFC 18 §3.5's
// "the same work"): it reads a user record, the dummy principal's for a name
// no user holds, resolves a secret reference, the dummy reference for a user
// with nothing to resolve, and computes the proof against the dummy NT hash.
// So a store round trip does not tell a name that exists from one that does
// not, and an outage answers both ErrUnavailable. What still differs is in
// memory: a dummy reference finds no record, so its data key is not unwrapped
// and its value not opened, two AES-GCM opens, since every key provider holds
// its keys in memory once started. Overturned if a key provider unwraps with
// a round trip; a miss then needs a sealed dummy record to resolve, which
// pkg/secrets' frozen surface (RFC 13 §7) has no call for.
//
// decision: a disabled user's outcome is disabled whatever the password, its
// proof computed against the dummy hash as a miss's is; the answer is
// ErrLogonFailure either way, so only the operator's record tells the account
// stopped the sign-in (RFC 18 §3.7).
func (s *Server) signInLocal(ctx context.Context, step identity.SignInStep, proof [16]byte, blob []byte, avFlags uint32) (identity.SignInResult, outcome, error) {
	a := step.NTLM
	if s.deps.Secrets == nil {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: no secret provider", vfserr.ErrUnavailable)
	}
	u, oc, err := s.ntlmUser(ctx, a.User)
	if err != nil {
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	ntHash := s.dummyNT
	// The NT hash is owned by the user's record under "nt_hash" (RFC 16 §2.3).
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "nt_hash"}
	ref := u.NTHash
	if oc != outcomeOK {
		ref = s.dummyRef
	}
	b, err := s.deps.Secrets.Resolve(ctx, secrets.Protocol, entity.SecretNTHash, owner, ref)
	switch {
	case oc != outcomeOK && merrs.IsNotFoundError(err):
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: NT hash: %w", vfserr.ErrUnavailable, err)
	case len(b) != len(ntHash):
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: stored NT hash of %d bytes", vfserr.ErrUnavailable, len(b))
	default:
		ntHash = [16]byte(b)
	}
	if s.ntHashed != nil {
		s.ntHashed(ntHash)
	}
	base, ok := checkProof(ntowfv2(ntHash, a.User, a.Domain), step.Challenge, proof, blob)
	switch {
	case oc != outcomeOK:
		return identity.SignInResult{}, oc, vfserr.ErrLogonFailure
	case !ok:
		return identity.SignInResult{}, outcomeLogonFailure, vfserr.ErrLogonFailure
	}
	key, err := exportedKey(base, a.Flags, a.EncryptedSessionKey)
	if err == nil {
		err = checkMIC(key, a, avFlags)
	}
	if err != nil {
		return identity.SignInResult{}, outcomeLogonFailure, fmt.Errorf("%w: %w", vfserr.ErrLogonFailure, err)
	}
	return identity.SignInResult{
		Identity: identity.Identity{
			Proof:      identity.ProofSecret,
			Protection: identity.ProtectionNone,
			Evidence:   identity.Evidence{Name: u.Name, Principal: u.Principal},
			Client:     step.Client,
		},
		Keys: identity.Keys{Session: key[:]},
	}, outcomeOK, nil
}

// ntlmUser is the user name finds and outcomeOK when it can sign in over
// NTLM; otherwise the outcome its refusal is recorded with: disabled, or
// logon_failure for a name no user holds, a user Account refuses, a
// directory principal and a user with no NT hash. A name no user holds reads
// the dummy principal's record, as a name that exists reads its own. An
// error is the store's, ErrUnavailable.
func (s *Server) ntlmUser(ctx context.Context, name string) (entity.User, outcome, error) {
	p, err := s.Lookup(ctx, name)
	if err != nil {
		return entity.User{}, outcomeUnavailable, err
	}
	if p == (entity.Principal{}) {
		p = s.dummyUser
	}
	u, err := s.Account(ctx, p)
	switch {
	case errors.Is(err, errDisabled):
		return entity.User{}, outcomeDisabled, nil
	case errors.Is(err, vfserr.ErrLogonFailure):
		return entity.User{}, outcomeLogonFailure, nil
	case err != nil:
		return entity.User{}, outcomeUnavailable, err
	case u.NTHash == entity.SecretRef{}, has(u.IDs, entity.SchemeDir):
		return u, outcomeLogonFailure, nil
	}
	return u, outcomeOK, nil
}
