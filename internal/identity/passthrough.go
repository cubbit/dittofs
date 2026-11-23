package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// logonGuest is UserFlags' LOGON_GUEST: the controller signed the user in as
// its guest (MS-NRPC 2.2.1.4.11).
const logonGuest = 0x00000001

// ridGuest is the domain's Guest account's RID (MS-DTYP 2.4.2.4).
const ridGuest = 501

// passThroughDomain is the domain table's pass-through row (RFC 18 §3.5): a
// message naming the directory's NetBIOS domain or its realm, or naming no
// domain and a user name ending '@' and the realm, each compared with ASCII
// letters folded only, while the installation is joined to an Active
// Directory domain.
//
// decision: a UPN under one of the forest's alternate suffixes, or a domain
// written with a trailing dot, is unknown_domain: only the directory
// record's realm routes here, and nothing reads the forest's suffixes (RFC
// 18 §3.5). Overturned when a deployment's users sign in with an alternate
// suffix; the directory then reads uPNSuffixes from the partitions
// container at each record change.
func passThroughDomain(st *State, user, domain string) bool {
	d := st.Directory
	if d == nil || !d.IsAD() || !st.Join.Joined {
		return false
	}
	if domain == "" {
		at := strings.LastIndexByte(user, '@')
		return at > 0 && equalFoldASCII(user[at+1:], d.Realm)
	}
	return equalFoldASCII(domain, d.NetBIOSDomain) || equalFoldASCII(domain, d.Realm)
}

// signInPassThrough sends the response once to a domain controller over the
// node's secure channel, the user and domain as the message names them, and
// binds the reply: its session key, refused missing, short or all zero; no
// guest logon; its logon domain the directory's; then the exported key and
// the MIC, as a local sign-in checks them; then the user SID through Find,
// minting a directory principal, refused when disabled or found by no
// principal; then its groups through the PAC's filter. A controller's
// refusal of the user's credentials or account is ErrLogonFailure; any
// other failure to get an answer, an NTSTATUS among them, is ErrUnavailable.
//
// decision: the directory's domain SID is read before the response is
// sent, so a directory outage answers ErrUnavailable without the controller
// counting a wrong password the node could not have bound anyway.
// Overturned if the domain SID is ever read from somewhere a sign-in need
// not wait on.
func (s *Server) signInPassThrough(ctx context.Context, st *State, step identity.SignInStep, avFlags uint32) (identity.SignInResult, outcome, error) {
	a := step.NTLM
	if st.dir == nil {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: pass-through: no directory", vfserr.ErrUnavailable)
	}
	// One bound for the whole sign-in: the domain SID, the secure channel's
	// slot and call, the mint and the filter's reads (RFC 18 §5.1). The
	// lookups below open their own, which this one ends first.
	caller := ctx
	ctx, cancel := directory.Bounded(caller, s.deps.Clock, directoryBound, errDirectoryBound)
	var returned atomic.Bool
	defer cancel()
	defer returned.Store(true) // before cancel, so the AfterFunc below sees it
	// The call's context ends with the bound or the caller, never with this
	// return, so a call left past its own bound keeps its slot, as logon has
	// it. It carries the caller's values but is no child of the caller, so
	// nothing of this sign-in stays registered on a long-lived caller.
	callCtx, endCall := context.WithCancelCause(context.WithoutCancel(caller))
	context.AfterFunc(ctx, func() {
		if !returned.Load() {
			endCall(context.Cause(ctx))
		}
	})
	domain, err := st.dir.domainSID(ctx)
	if err != nil {
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: pass-through: the domain SID: %w", vfserr.ErrUnavailable, err)
	}
	rep, err := s.logon(callCtx, netlogon.LogonRequest{Domain: a.Domain, User: a.User, Workstation: a.Workstation,
		Challenge: step.Challenge, NTResponse: a.NTResponse})
	var status netlogon.Status
	switch {
	case errors.As(err, &status) && credentialStatus[status]:
		return identity.SignInResult{}, outcomeLogonFailure, fmt.Errorf("%w: pass-through: %w", vfserr.ErrLogonFailure, err)
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, fmt.Errorf("%w: pass-through: %w", vfserr.ErrUnavailable, err)
	}
	if err := checkReply(rep, domain); err != nil {
		return identity.SignInResult{}, outcomeLogonFailure, fmt.Errorf("%w: pass-through: %w", vfserr.ErrLogonFailure, err)
	}
	key, err := ntlmKey([16]byte(rep.SessionKey), a, avFlags)
	if err != nil {
		return identity.SignInResult{}, outcomeLogonFailure, err
	}
	// Find mints the entry the reply's SID names; one whose principal is
	// marked deleted is ErrUnknownPrincipal until its deletion ends, as an
	// unknown name is (RFC 18 §3.7).
	p, err := s.Find(ctx, identity.Spelling{Scheme: identity.SchemeSID, SID: rep.User}, identity.FindOpts{Mint: true})
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return identity.SignInResult{}, outcomeLogonFailure, fmt.Errorf("%w: pass-through: no principal for the reply's user SID", vfserr.ErrLogonFailure)
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	u, err := s.Account(ctx, p)
	switch {
	case errors.Is(err, errDisabled):
		return identity.SignInResult{}, outcomeDisabled, err
	case errors.Is(err, vfserr.ErrLogonFailure):
		return identity.SignInResult{}, outcomeLogonFailure, err
	case err != nil:
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	members := make([]sidMember, 0, len(rep.Groups))
	for _, g := range rep.Groups {
		members = append(members, sidMember{g.SID, g.Attrs})
	}
	sids, deny, err := s.filterSIDs(ctx, rep.LogonDomain, rep.User, members)
	if err != nil {
		return identity.SignInResult{}, outcomeUnavailable, err
	}
	return ntlmResult(step, identity.Evidence{Name: st.Directory.NetBIOSDomain + `\` + u.Name, Principal: p, SIDs: sids, DenyOnly: deny}, key), outcomeOK, nil
}

// credentialStatus is each NTSTATUS a controller refuses a logon with
// because of the user's credentials or account: a wrong password or name,
// an account locked, disabled, expired or restricted, an expired password,
// a trust account's logon, and the refusals of policy MS-APDS 3.1.5 names,
// a smart card required, an authentication policy, and NTLM blocked. Any
// other status is the controller failing to answer, ErrUnavailable, which
// the throttle never counts.
var credentialStatus = map[netlogon.Status]bool{
	netlogon.StatusNoSuchUser: true, netlogon.StatusWrongPassword: true, netlogon.StatusLogonFailure: true,
	netlogon.StatusAccountRestriction: true, netlogon.StatusInvalidLogonHours: true, netlogon.StatusInvalidWorkstation: true,
	netlogon.StatusPasswordExpired: true, netlogon.StatusPasswordMustChange: true, netlogon.StatusAccountDisabled: true,
	netlogon.StatusAccountExpired: true, netlogon.StatusAccountLockedOut: true,
	netlogon.StatusNologonInterdomainTrust: true, netlogon.StatusNologonWorkstationTrust: true, netlogon.StatusNologonServerTrust: true,
	netlogon.StatusSmartcardLogonRequired: true, netlogon.StatusAuthFirewallFailed: true, netlogon.StatusNTLMBlocked: true,
}

// checkReply refuses a reply whose user session key is not 16 bytes or is
// all zero, a guest logon, by UserFlags or by the user's RID, and a logon
// domain other than domain, the directory's domain SID in binary form.
func checkReply(rep netlogon.Reply, domain []byte) error {
	subs := rep.User.SubAuthorities
	switch {
	case len(rep.SessionKey) != 16 || bytes.Equal(rep.SessionKey, make([]byte, 16)):
		return fmt.Errorf("a user session key of %d bytes, or all zero", len(rep.SessionKey))
	case rep.UserFlags&logonGuest != 0:
		return errors.New("a guest logon")
	case len(subs) == 0 || subs[len(subs)-1] == ridGuest:
		return errors.New("the domain's Guest, or no user SID")
	case !bytes.Equal(sidBytes(rep.LogonDomain), domain):
		return errors.New("a logon domain other than the directory's")
	}
	return nil
}

// ComputerName is the NetBIOS name the NTLM CHALLENGE's MsvAvNbComputerName
// carries: identity.netbios_name in the State in force, which a join writes
// as the machine account's name, so a response passed through names the
// computer the secure channel is set up as, which a controller may require
// (RFC 18 §3.5). ErrUnavailable before the first Apply and after Stop.
func (s *Server) ComputerName() (string, error) {
	st := s.state.Load()
	if st == nil {
		return "", fmt.Errorf("%w: no State applied", vfserr.ErrUnavailable)
	}
	return st.Settings.NetBIOSName, nil
}
