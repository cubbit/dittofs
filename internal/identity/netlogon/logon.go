package netlogon

import (
	"context"
	"fmt"

	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	logon "github.com/oiweiwei/go-msrpc/msrpc/nrpc/logon/v1"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Logon sends r once with NetrLogonSamLogonEx over the sealed association,
// asking for SamInfo4. A call that failed before any byte of its request was
// written is sent once more, on a new association; one that failed after is
// vfserr.ErrUnavailable and never sent again, so a wrong password is counted
// once toward the account's lockout.
func (c *channel) Logon(ctx context.Context, r LogonRequest) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	req := samLogonRequest(c.dc.server, c.name, r)
	for try := 0; ; try++ {
		as, err := c.association(ctx)
		if err != nil {
			return Reply{}, err
		}
		before := as.written.Load()
		var out *logon.SAMLogonExResponse
		err = decoded(func() (err error) {
			out, err = as.cli.SAMLogonEx(ctx, req)
			return err
		})
		if out != nil && out.Return != 0 {
			return Reply{}, Status(uint32(out.Return))
		}
		if err == nil {
			v, ok := out.ValidationInformation.GetValue().(*logon.ValidationSAMInfo4)
			if !ok || v == nil {
				return Reply{}, fmt.Errorf("netlogon: NetrLogonSamLogonEx answered %T, want SamInfo4", out.ValidationInformation.GetValue())
			}
			return replyFrom(v)
		}
		sent := as.written.Load() != before
		c.drop()
		if sent {
			return Reply{}, fmt.Errorf("netlogon: NetrLogonSamLogonEx failed after it was sent: %w: %w", vfserr.ErrUnavailable, err)
		}
		if try == 1 {
			return Reply{}, err
		}
	}
}

// samLogonRequest is r as a network logon from computer to server. It sets
// no MSV1_0_ALLOW_WORKSTATION_TRUST_ACCOUNT, so a computer account's
// response is refused.
func samLogonRequest(server, computer string, r LogonRequest) *logon.SAMLogonExRequest {
	return &logon.SAMLogonExRequest{
		LogonServer:  server,
		ComputerName: computer,
		LogonLevel:   logon.LogonInfoClassNetworkTransitiveInformation,
		LogonInformation: &logon.Level{Value: &logon.Level_LogonNetworkTransitive{
			LogonNetworkTransitive: &logon.NetworkInfo{
				Identity: &logon.LogonIdentityInfo{
					LogonDomainName: &dtyp.UnicodeString{Buffer: r.Domain},
					UserName:        &dtyp.UnicodeString{Buffer: r.User},
					Workstation:     &dtyp.UnicodeString{Buffer: r.Workstation},
				},
				LMChallenge:         &logon.LMChallenge{Data: r.Challenge[:]},
				NTChallengeResponse: &logon.String{Buffer: r.NTResponse},
				LMChallengeResponse: &logon.String{Buffer: r.LMResponse},
			},
		}},
		ValidationLevel: logon.ValidationInfoClassSAMInfo4,
	}
}
