package netlogon

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"
	nlssp "github.com/oiweiwei/go-msrpc/ssp/netlogon"
)

// errSpent answers a SetPassword on a channel an earlier SetPassword failed on.
var errSpent = errors.New("netlogon: SetPassword on a channel a failed SetPassword spent; settle it on a new Channel")

// trustPasswordRunes is NL_TRUST_PASSWORD's buffer in UTF-16 code units.
const trustPasswordRunes = 256

// SetPassword sets the account's password with NetrServerPasswordSet2 over
// the sealed association, the request authenticator the channel's next and
// the reply's return authenticator verified. It is never sent twice: a call
// that fails, its password set or not, is answered as an error the caller
// settles by asking whether the controller accepts the password. A failed
// call spends the channel, whose chain may then be ahead of the controller's:
// a later SetPassword answers errSpent without touching the wire.
func (c *channel) SetPassword(ctx context.Context, password []byte) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spent {
		return errSpent
	}
	defer func() { c.spent = err != nil }()
	as, err := c.association(ctx)
	if err != nil {
		return err
	}
	tp, err := trustPassword(ctx, c.cred, password)
	if err != nil {
		return err
	}
	ts := uint32(time.Now().Unix())
	auth, err := c.cred.Next(ctx, ts)
	if err != nil {
		return err
	}
	out := &logon.PasswordSet2Response{}
	err = invoke(ctx, as.cli.Conn(), &logon.PasswordSet2Request{
		PrimaryName: c.dc.server, AccountName: c.name + "$",
		SecureChannelType: logon.SecureChannelTypeWorkstationSecureChannel,
		ComputerName:      c.name,
		Authenticator:     &logon.Authenticator{Credential: &logon.Credential{Data: auth}, Timestamp: ts},
		ClearNewPassword:  tp,
	}, out)
	if err != nil {
		c.drop()
		return fmt.Errorf("netlogon: NetrServerPasswordSet2: %w", err)
	}
	if out.Return != 0 {
		return Status(uint32(out.Return))
	}
	if out.ReturnAuthenticator == nil || out.ReturnAuthenticator.Credential == nil {
		return errors.New("netlogon: NetrServerPasswordSet2 answered no return authenticator")
	}
	if err := c.cred.Verify(ctx, 1, out.ReturnAuthenticator.Credential.Data); err != nil {
		return fmt.Errorf("netlogon: NetrServerPasswordSet2's return authenticator: %w", err)
	}
	return nil
}

// trustPassword is password as NL_TRUST_PASSWORD (MS-NRPC 2.2.1.3.7),
// encrypted under the channel's session key (3.4.5.2.6): random filler, the
// password's UTF-16LE at the buffer's end, and its length in bytes after it,
// all 516 bytes encrypted together.
func trustPassword(ctx context.Context, cred *nlssp.SecureCredential, password []byte) (*logon.TrustPassword, error) {
	b := utf16le(password)
	defer clear(b)
	if len(b) > 2*trustPasswordRunes {
		return nil, fmt.Errorf("netlogon: a machine password of %d UTF-16 code units, more than %d", len(b)/2, trustPasswordRunes)
	}
	plain := make([]byte, 2*trustPasswordRunes+4)
	defer clear(plain)
	if _, err := rand.Read(plain[:2*trustPasswordRunes]); err != nil {
		return nil, err
	}
	copy(plain[2*trustPasswordRunes-len(b):], b)
	binary.LittleEndian.PutUint32(plain[2*trustPasswordRunes:], uint32(len(b)))
	cipher, err := cred.Encrypt(ctx, plain)
	if err != nil {
		return nil, err
	}
	// Only the AES channel's encryption keeps the length; the channel refuses
	// any other.
	if len(cipher) != len(plain) {
		return nil, fmt.Errorf("netlogon: the trust password encrypted to %d bytes, want %d", len(cipher), len(plain))
	}
	buf := make([]uint16, trustPasswordRunes)
	for i := range buf {
		buf[i] = binary.LittleEndian.Uint16(cipher[2*i:])
	}
	return &logon.TrustPassword{Buffer: buf, Length: binary.LittleEndian.Uint32(cipher[2*trustPasswordRunes:])}, nil
}
