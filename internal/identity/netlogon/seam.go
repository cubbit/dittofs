// Package netlogon is identity's domain controller: the secure channel a
// NETLOGON pass-through and a machine password set run over, and the computer
// account's reads and writes. The seam it declares stands above the NETLOGON
// client, so identitytest's in-memory controller answers the same calls with
// their results, and the client's wire behaviour is its own.
package netlogon

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
)

// Dialer finds a domain's controllers and opens one.
type Dialer interface {
	// Lookup answers the domain's controllers from its SRV records, through
	// dnsServer when set; the record's explicit address is the channel's rule.
	Lookup(ctx context.Context, realm, dnsServer string) ([]string, error)
	// Dial opens the controller at addr, one Lookup answered or the record's
	// explicit address, for the domain rec names.
	Dial(ctx context.Context, rec directory.Record, addr string) (DC, error)
}

// DC is one domain controller. A computer account is named by its name
// without the trailing $, which the controller adds, compared with ASCII
// letters folded.
type DC interface {
	// Channel sets up a secure channel as the computer account with password,
	// the server credential verified: an error matching ErrRejected when the
	// controller refuses the password or the account, any other error when it
	// could not be asked. These are the three answers of the probe whether a
	// password is accepted: nil, ErrRejected, and anything else, which is
	// never a rejection.
	Channel(ctx context.Context, account string, password []byte) (Channel, error)
	// Account reads the computer account bound as bind, its names with the
	// domain's sPNMappings applied: ErrNoAccount when absent. Account, Create
	// and Reset answer ErrRejected when the controller refuses the bind's
	// credentials or the rights the call needs.
	Account(ctx context.Context, bind identity.JoinCredential, name string) (Account, error)
	// Create creates the computer account c as admin with password in the
	// creation itself, its host/ and cifs/ names for the short and the DNS
	// host name, and msDS-SupportedEncryptionTypes 0x18: fs.ErrExist,
	// changing nothing, when an account of that name already exists. An
	// empty c.OU creates it in the domain's Computers container, as the
	// domain's wellKnownObjects names it.
	Create(ctx context.Context, admin identity.JoinCredential, c Computer, password []byte) error
	// Reset sets an existing account's password as admin.
	Reset(ctx context.Context, admin identity.JoinCredential, name string, password []byte) error
	Close() error
}

// Channel is a secure channel set up as one computer account.
type Channel interface {
	// Logon sends one NTLM response to the controller: an error matching
	// ErrRejected, a Status, when the controller refuses it.
	Logon(ctx context.Context, r LogonRequest) (Reply, error)
	// SetPassword sets the channel's account's password to password. It
	// advances the channel's authenticator chain before it sends, so after
	// an error the chain may be ahead of the controller's and the channel
	// takes no further SetPassword: the caller settles the failure on a new
	// Channel. Logon carries no authenticator and is unaffected.
	SetPassword(ctx context.Context, password []byte) error
	Close() error
}

// Computer is a computer account to create.
type Computer = directory.NewComputer

// Account is what a controller holds of a computer account.
type Account = directory.ComputerAccount

// LogonRequest is one NTLM network logon passed through.
type LogonRequest struct {
	Domain      string // the domain the client named
	User        string
	Workstation string
	Challenge   [8]byte // the server challenge the response answers
	NTResponse  []byte
	LMResponse  []byte
}

// Reply is a successful logon's answer, as the controller sent it: every
// field is checked by its reader, never here.
type Reply struct {
	User        sid.SID
	UserFlags   uint32
	Groups      []SIDAttr
	SessionKey  []byte // the user session key
	LogonDomain sid.SID
}

// SIDAttr is a SID with its group attributes.
type SIDAttr struct {
	SID   sid.SID
	Attrs uint32
}

var (
	// ErrRejected is a controller's refusal: of a logon, or of a channel's
	// account or password. A refusal carrying an NTSTATUS is a Status.
	ErrRejected = errors.New("netlogon: rejected by the domain controller")
	// ErrNoAccount is a computer account the controller does not hold.
	ErrNoAccount = errors.New("netlogon: no such computer account")
)

// Status is the NTSTATUS a controller refused a call with. It matches
// ErrRejected.
type Status uint32

func (s Status) Error() string {
	return fmt.Sprintf("netlogon: rejected with NTSTATUS 0x%08x", uint32(s))
}

// Is makes a Status match ErrRejected.
func (s Status) Is(target error) bool { return target == ErrRejected }

// The NTSTATUS values a controller refuses with (MS-ERREF 2.3.1).
const (
	StatusAccessDenied            Status = 0xC0000022 // a channel's password refused
	StatusNoLogonServers          Status = 0xC000005E
	StatusNoSuchUser              Status = 0xC0000064
	StatusWrongPassword           Status = 0xC000006A
	StatusLogonFailure            Status = 0xC000006D
	StatusAccountRestriction      Status = 0xC000006E
	StatusInvalidLogonHours       Status = 0xC000006F
	StatusInvalidWorkstation      Status = 0xC0000070
	StatusPasswordExpired         Status = 0xC0000071
	StatusAccountDisabled         Status = 0xC0000072
	StatusNoTrustSAMAccount       Status = 0xC000018B // a channel for an account the domain lacks
	StatusAccountExpired          Status = 0xC0000193
	StatusNologonInterdomainTrust Status = 0xC0000198 // an interdomain trust account's logon
	StatusNologonWorkstationTrust Status = 0xC0000199 // a computer account's logon
	StatusNologonServerTrust      Status = 0xC000019A // a server trust account's logon
	StatusPasswordMustChange      Status = 0xC0000224
	StatusAccountLockedOut        Status = 0xC0000234
)
