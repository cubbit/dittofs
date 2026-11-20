//go:build ad_dc

package ad_dc_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// alice's NTLMv2 response, its MsvAvNbComputerName the name identity hands
// the CHALLENGE, passes through over the DITTOFS$ channel and answers her
// SID; the same response over a channel set up as another computer account
// is refused, her badPwdCount unchanged, as Samba refuses a response naming
// a computer other than the channel's account.
func TestPassThroughComputerName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	waitForMachinePassword(t, dcIP, "dittofs", machineAccountPass)
	rec, ca := dcRecord(t)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca), ip: dcIP}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	name := computerName(t)
	var challenge [8]byte
	_, _ = rand.Read(challenge[:])
	nt, lm := ntlmResponseFor(ctx, t, name, adUserAlice, adUserPass, challenge)
	req := netlogon.LogonRequest{Domain: adDomain, User: adUserAlice, Workstation: "DCTWS", Challenge: challenge, NTResponse: nt, LMResponse: lm}
	dialer := netlogon.NewDialer((&net.Dialer{}).DialContext)
	logonAs := func(account string, password []byte) (netlogon.Reply, error) {
		dc, err := dialer.Dial(ctx, rec, dcIP)
		if err != nil {
			t.Fatalf("dial the DC: %v", err)
		}
		defer func() { _ = dc.Close() }()
		ch, err := dc.Channel(ctx, account, password)
		if err != nil {
			t.Fatalf("secure channel as %s$: %v", account, err)
		}
		defer func() { _ = ch.Close() }()
		return ch.Logon(ctx, req)
	}

	rep, err := logonAs("dittofs", []byte(machineAccountPass))
	if err != nil {
		t.Fatalf("alice's response naming %s over the DITTOFS$ channel: %v, want answered", name, err)
	}
	if got, want := sid.FormatSID(&rep.User), adAttrs(t, "user", adUserAlice, "objectSid")["objectSid"]; got != want {
		t.Errorf("the reply's user SID %s, want alice's %s", got, want)
	}

	seed.Reset(t)
	t.Cleanup(func() { seed.Reset(t) })
	other, pw := "DCT"+randomHex(), []byte("Dc1!"+randomHex()+randomHex())
	seed.StageComputer(t, identitytest.StagedComputer{Name: other, Password: pw, EncTypes: 0x18})
	before := seed.BadPwdCount(t, adUserAlice)
	if _, err := logonAs(other, pw); !errors.Is(err, netlogon.ErrRejected) {
		t.Errorf("the response naming %s over the %s$ channel: %v, want refused", name, other, err)
	}
	if got := seed.BadPwdCount(t, adUserAlice); got != before {
		t.Errorf("alice's badPwdCount after the refusal = %d, want %d unchanged", got, before)
	}
}

// computerName is the name identity's seam answers for an installation whose
// identity.netbios_name is the fixture's computer account's name.
func computerName(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s, err := identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(ctx) })
	if err := s.Apply(identity.State{Settings: identity.Settings{IDRange: entity.DefaultIDRange, NTLM: true, NetBIOSName: "DITTOFS"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	name, err := s.ComputerName()
	if err != nil {
		t.Fatalf("ComputerName: %v", err)
	}
	return name
}
