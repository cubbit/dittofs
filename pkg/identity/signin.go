package identity

import (
	"context"
	"net/netip"
)

// Authenticator verifies one handshake round. It is the same shape as
// vfs.Principals.SignIn, so the filesystem passes it through.
//
// Before it returns, SignIn binds what it verified to one principal and sets
// Evidence.Principal; every call on that security context then acts as that
// principal, never mapping the name again. Every error it returns is one of
// vfserr.ErrLogonFailure, for a wrong credential or an account that cannot
// sign in; vfserr.ErrInvalid, for a malformed message; or
// vfserr.ErrUnavailable, for an answer that could not be decided. None of them may become a guest or
// anonymous session.
type Authenticator interface {
	SignIn(ctx context.Context, step SignInStep) (SignInResult, error)
}

// SignInStep is one verification round: an AP-REQ for Service, or a decoded
// NTLM AUTHENTICATE with the challenge the adapter sent. NTLM's NEGOTIATE and
// CHALLENGE never reach identity: the adapter answers them and passes the
// challenge here.
type SignInStep struct {
	Kerberos  []byte            // an AP-REQ
	Service   Service           // the keytab service name a ticket is for
	Transport Transport         // the protocol the step arrived over, which the adapter sets
	NTLM      *NTLMAuthenticate // or an NTLM response to Challenge
	Challenge [8]byte
	Client    netip.Addr
	Context   *Evidence // the context's current evidence, on a re-authentication; nil otherwise
}

// Service is the keytab service name a ticket is for.
type Service uint8

const (
	ServiceNFS Service = iota + 1
	ServiceCIFS
	ServiceHost
)

// Transport is the file protocol a sign-in step arrived over. A Kerberos
// step's client-address rule applies over SMB, whatever service name its
// ticket is for, so the step names its transport apart from its Service.
type Transport uint8

const (
	TransportNFS Transport = iota + 1
	TransportSMB
)

// NTLMAuthenticate is the decoded AUTHENTICATE message the adapter's NTLM
// mechanism hands over, with the three messages' bytes kept exactly as sent
// so the MIC can be checked. It carries no decoded MIC or MsvAvFlags: identity
// reads the flags from NTResponse, which the proof authenticates, and the MIC
// from Raw, so the adapter has no unauthenticated copy to gate on.
type NTLMAuthenticate struct {
	User, Domain, Workstation       string
	NTResponse, EncryptedSessionKey []byte
	Flags                           uint32 // NegotiateFlags
	Negotiate, Challenge, Raw       []byte // the wire bytes of NEGOTIATE, CHALLENGE and this AUTHENTICATE
}

// SignInResult is an Identity with proof kind ProofSecret (NTLM) or
// ProofTicket (Kerberos) and protection ProtectionNone, which the adapter
// raises per request as its protocol's signing or sealing allows, and the
// Keys beside it, never in it.
type SignInResult struct {
	Identity Identity
	Keys     Keys
}

// Keys is what the adapter's mechanism needs for the AP-REP and for signing
// and encryption. It is never copied into an Identity. Session is the context
// key: for NTLM the exported session key; for Kerberos the acceptor's subkey
// (Subkey) when the AP-REP sends one, else the authenticator's subkey, else
// the ticket's session key, kept whole, since SMB takes its first 16 bytes or
// all 32 by the cipher.
type Keys struct{ Session, Subkey, APRep []byte }
