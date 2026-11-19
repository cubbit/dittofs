package identitytest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
)

// DCDomain names the domain a DCSeeder seeds.
type DCDomain struct {
	NetBIOS, Realm, Base string
	SID                  sid.SID
}

// StagedComputer is a computer account an administrator creates by hand,
// with its password in the creation, in CN=Computers under the base.
type StagedComputer struct {
	Name     string // without the $
	Password []byte
	EncTypes uint32 // msDS-SupportedEncryptionTypes; 0 leaves it unset
	SPNs     []string
}

// DCSeeder puts what a row of DomainControllerConformance needs into the
// domain under test, at the controller open dialled. Every account the suite
// stages or creates is named DCT and eight hexadecimal digits, and every user
// it adds dcu and eight.
type DCSeeder interface {
	Domain() DCDomain
	// Admin is a Domain Admins member's bind, for Create, Reset and Account.
	Admin() identity.JoinCredential
	// AddUser adds an enabled user with password and returns its SID.
	AddUser(t testing.TB, name string, password []byte) sid.SID
	// BadPwdCount reads the user's badPwdCount at the controller.
	BadPwdCount(t testing.TB, name string) int
	StageComputer(t testing.TB, c StagedComputer)
	// SetComputerPassword sets an account's password as its creator may.
	SetComputerPassword(t testing.TB, name string, password []byte)
	// Unreachable is a connection to a controller of the domain that cannot
	// be reached.
	Unreachable(t testing.TB) netlogon.DC
	// MoveComputersContainer creates the organisational unit ou and makes it
	// the domain's Computers container, as redircmp does.
	MoveComputersContainer(t testing.TB, ou string)
	// Reset deletes every computer account the suite staged or created and
	// puts the Computers container back.
	Reset(t testing.TB)
}

// DomainControllerConformance holds a domain controller to what identity
// reads of Active Directory's: pass-through, the bad-password count, a
// computer account's creation and the refusal of an existing one, its key
// version number, encryption types, names and owner, and the three answers
// of the probe. open returns a new connection to one controller and its
// seeder; the in-memory DomainController runs it in unit tests and a real
// controller in integration tests, so the in-memory one stands in only where
// both answer alike. Each row resets the seeder first and when it ends.
func DomainControllerConformance(t *testing.T, open func(t *testing.T) (netlogon.DC, DCSeeder)) {
	ctx := context.Background()

	start := func(t *testing.T) (netlogon.DC, DCSeeder) {
		t.Helper()
		dc, s := open(t)
		s.Reset(t)
		t.Cleanup(func() { s.Reset(t); _ = dc.Close() })
		return dc, s
	}
	channel := func(t *testing.T, dc netlogon.DC, account string, password []byte) netlogon.Channel {
		t.Helper()
		ch, err := dc.Channel(ctx, account, password)
		if err != nil {
			t.Fatalf("secure channel as %s$: %v", account, err)
		}
		t.Cleanup(func() { _ = ch.Close() })
		return ch
	}
	account := func(t *testing.T, dc netlogon.DC, s DCSeeder, name string) netlogon.Account {
		t.Helper()
		a, err := dc.Account(ctx, s.Admin(), name)
		if err != nil {
			t.Fatalf("Account %s$: %v", name, err)
		}
		return a
	}
	// joined stages a computer account the rows sign in with.
	joined := func(t *testing.T, s DCSeeder) (string, []byte) {
		t.Helper()
		name, pw := randomName("DCT"), password()
		s.StageComputer(t, StagedComputer{Name: name, Password: pw, EncTypes: 0x18})
		return name, pw
	}
	logon := func(ch netlogon.Channel, s DCSeeder, user string, pw []byte) (netlogon.Reply, []byte, error) {
		var challenge, client [8]byte
		_, _ = rand.Read(challenge[:])
		_, _ = rand.Read(client[:])
		dom := s.Domain().NetBIOS
		nt, lm, key := NTLMv2Response(pw, user, dom, challenge, client)
		r, err := ch.Logon(ctx, netlogon.LogonRequest{Domain: dom, User: user, Workstation: "DCTWS",
			Challenge: challenge, NTResponse: nt, LMResponse: lm})
		return r, key, err
	}
	wantStatus := func(t *testing.T, what string, err error, want netlogon.Status) {
		t.Helper()
		var got netlogon.Status
		if !errors.As(err, &got) || got != want || !errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("%s: err %v, want %v matching ErrRejected", what, err, want)
		}
	}

	t.Run("pass-through answers the user's SID and session key", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		user, upw := randomName("dcu"), password()
		usid := s.AddUser(t, user, upw)
		r, key, err := logon(channel(t, dc, name, pw), s, user, upw)
		if err != nil {
			t.Fatalf("logon of %s with its password: %v", user, err)
		}
		dom := s.Domain().SID
		if !r.User.Equal(&usid) {
			t.Errorf("reply's user SID %s, want %s", sid.FormatSID(&r.User), sid.FormatSID(&usid))
		}
		if !r.LogonDomain.Equal(&dom) {
			t.Errorf("reply's logon domain %s, want the domain's %s", sid.FormatSID(&r.LogonDomain), sid.FormatSID(&dom))
		}
		if !bytes.Equal(r.SessionKey, key) {
			t.Errorf("reply's session key %x, want the session base key %x", r.SessionKey, key)
		}
		users := dom
		users.SubAuthorityCount++
		users.SubAuthorities = append(slices.Clone(dom.SubAuthorities), ridDomainUsers)
		if !slices.ContainsFunc(r.Groups, func(g netlogon.SIDAttr) bool { return g.SID.Equal(&users) }) {
			t.Errorf("reply's groups %v, want Domain Users %s among them", r.Groups, sid.FormatSID(&users))
		}
	})

	t.Run("a wrong password counts once and a success resets the count", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		user, upw := randomName("dcu"), password()
		s.AddUser(t, user, upw)
		ch := channel(t, dc, name, pw)
		for i := 1; i <= 2; i++ {
			_, _, err := logon(ch, s, user, password())
			wantStatus(t, "logon with a wrong password", err, netlogon.StatusWrongPassword)
			if got := s.BadPwdCount(t, user); got != i {
				t.Errorf("badPwdCount after %d wrong passwords = %d, want %d", i, got, i)
			}
		}
		if _, _, err := logon(ch, s, user, upw); err != nil {
			t.Fatalf("logon with the right password: %v", err)
		}
		if got := s.BadPwdCount(t, user); got != 0 {
			t.Errorf("badPwdCount after a success = %d, want 0", got)
		}
	})

	t.Run("an unknown user is refused", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		_, _, err := logon(channel(t, dc, name, pw), s, randomName("dcu"), password())
		wantStatus(t, "logon of a user the domain lacks", err, netlogon.StatusNoSuchUser)
	})

	t.Run("a computer account's logon is refused", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		other, opw := joined(t, s)
		_, _, err := logon(channel(t, dc, name, pw), s, other+"$", opw)
		wantStatus(t, "logon of a computer account with its password", err, netlogon.StatusNologonWorkstationTrust)
	})

	t.Run("create, then create again refused with the password unchanged", func(t *testing.T) {
		dc, s := start(t)
		name, pw := randomName("DCT"), password()
		c := netlogon.Computer{Name: name, DNSHostName: strings.ToLower(name + "." + s.Domain().Realm)}
		if err := dc.Create(ctx, s.Admin(), c, pw); err != nil {
			t.Fatalf("Create %s$: %v", name, err)
		}
		if err := dc.Create(ctx, s.Admin(), c, password()); !errors.Is(err, fs.ErrExist) {
			t.Errorf("Create of the existing %s$: err %v, want fs.ErrExist", name, err)
		}
		if a := account(t, dc, s, name); a.KeyVersion != 1 {
			t.Errorf("key version after the refused create = %d, want 1", a.KeyVersion)
		}
		_ = channel(t, dc, name, pw).Close()
	})

	t.Run("a created account reads back its names, 0x18, owner and key version 1", func(t *testing.T) {
		dc, s := start(t)
		name := randomName("DCT")
		fqdn := strings.ToLower(name + "." + s.Domain().Realm)
		if err := dc.Create(ctx, s.Admin(), netlogon.Computer{Name: name, DNSHostName: fqdn}, password()); err != nil {
			t.Fatalf("Create %s$: %v", name, err)
		}
		a := account(t, dc, s, name)
		if a.KeyVersion != 1 {
			t.Errorf("msDS-KeyVersionNumber of a created account = %d, want 1: the password set in the add", a.KeyVersion)
		}
		if a.EncTypes != 0x18 {
			t.Errorf("msDS-SupportedEncryptionTypes = %#x, want 0x18", a.EncTypes)
		}
		for _, want := range []string{"host/" + name, "host/" + fqdn, "cifs/" + name, "cifs/" + fqdn} {
			if !slices.ContainsFunc(a.Names, func(n string) bool { return strings.EqualFold(n, want) }) {
				t.Errorf("names %v, want %s among them", a.Names, want)
			}
		}
		dom := s.Domain().SID
		da := dom
		da.SubAuthorityCount++
		da.SubAuthorities = append(slices.Clone(dom.SubAuthorities), ridDomainAdmins)
		if !a.Owner.Equal(&da) {
			t.Errorf("owner %s, want Domain Admins %s", sid.FormatSID(&a.Owner), sid.FormatSID(&da))
		}
		if len(a.Delegation) != 0 {
			t.Errorf("msDS-AllowedToActOnBehalfOfOtherIdentity = %x, want none", a.Delegation)
		}
		if want := "CN=" + name + ",CN=Computers," + s.Domain().Base; !strings.EqualFold(a.DN, want) {
			t.Errorf("DN %s, want %s", a.DN, want)
		}
	})

	t.Run("create with no OU uses the Computers container the domain names", func(t *testing.T) {
		dc, s := start(t)
		ou := "OU=Servers," + s.Domain().Base
		s.MoveComputersContainer(t, ou)
		name := randomName("DCT")
		if err := dc.Create(ctx, s.Admin(), netlogon.Computer{Name: name,
			DNSHostName: strings.ToLower(name + "." + s.Domain().Realm)}, password()); err != nil {
			t.Fatalf("Create %s$: %v", name, err)
		}
		if a, want := account(t, dc, s, name), "CN="+name+","+ou; !strings.EqualFold(a.DN, want) {
			t.Errorf("created at %s, want %s, the redirected container", a.DN, want)
		}
	})

	// A workstation's secure channel takes its current password alone, so a
	// rotation's previous password backs no channel at a controller that has
	// taken the new one.
	t.Run("a reset and a set each raise the key version by one and refuse the previous password", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		v0 := account(t, dc, s, name).KeyVersion
		reset := password()
		if err := dc.Reset(ctx, s.Admin(), name, reset); err != nil {
			t.Fatalf("Reset %s$: %v", name, err)
		}
		if got := account(t, dc, s, name).KeyVersion; got != v0+1 {
			t.Errorf("key version after a reset = %d, want %d", got, v0+1)
		}
		if _, err := dc.Channel(ctx, name, pw); !errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("channel with the password before a reset: err %v, want ErrRejected", err)
		}
		set := password()
		if err := channel(t, dc, name, reset).SetPassword(ctx, set); err != nil {
			t.Fatalf("SetPassword over the channel: %v", err)
		}
		if got := account(t, dc, s, name).KeyVersion; got != v0+2 {
			t.Errorf("key version after a set = %d, want %d", got, v0+2)
		}
		_ = channel(t, dc, name, set).Close()
		if _, err := dc.Channel(ctx, name, reset); !errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("channel with the password before a set: err %v, want ErrRejected", err)
		}
	})

	t.Run("encryption types read as written and 0 when unset", func(t *testing.T) {
		dc, s := start(t)
		set, unset := randomName("DCT"), randomName("DCT")
		s.StageComputer(t, StagedComputer{Name: set, Password: password(), EncTypes: 0x18})
		s.StageComputer(t, StagedComputer{Name: unset, Password: password()})
		if got := account(t, dc, s, set).EncTypes; got != 0x18 {
			t.Errorf("msDS-SupportedEncryptionTypes written 0x18, read %#x", got)
		}
		if got := account(t, dc, s, unset).EncTypes; got != 0 {
			t.Errorf("msDS-SupportedEncryptionTypes unset, read %#x, want 0", got)
		}
	})

	t.Run("a host name is answered with the cifs name sPNMappings maps to it", func(t *testing.T) {
		dc, s := start(t)
		name := randomName("DCT")
		fqdn := strings.ToLower(name + "." + s.Domain().Realm)
		s.StageComputer(t, StagedComputer{Name: name, Password: password(), SPNs: []string{"HOST/" + name, "HOST/" + fqdn}})
		a := account(t, dc, s, name)
		for _, want := range []string{"cifs/" + name, "cifs/" + fqdn} {
			if !slices.ContainsFunc(a.Names, func(n string) bool { return strings.EqualFold(n, want) }) {
				t.Errorf("names %v of an account holding only host/ names, want %s among them", a.Names, want)
			}
		}
	})

	t.Run("an absent account is ErrNoAccount", func(t *testing.T) {
		dc, s := start(t)
		if _, err := dc.Account(ctx, s.Admin(), randomName("DCT")); !errors.Is(err, netlogon.ErrNoAccount) {
			t.Errorf("Account of an absent computer: err %v, want ErrNoAccount", err)
		}
	})

	t.Run("the probe's three answers", func(t *testing.T) {
		dc, s := start(t)
		name, pw := joined(t, s)
		ch, err := dc.Channel(ctx, name, pw)
		if err != nil {
			t.Errorf("probe with the password: %v, want accepted", err)
		} else {
			_ = ch.Close()
		}
		if _, err := dc.Channel(ctx, name, password()); !errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("probe with a wrong password: err %v, want ErrRejected", err)
		}
		_ = dc.Close()
		if _, err := dc.Channel(ctx, name, pw); err == nil || errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("probe on a closed connection: err %v, want an error that is not ErrRejected", err)
		}
		gone := s.Unreachable(t)
		t.Cleanup(func() { _ = gone.Close() })
		if _, err := gone.Channel(ctx, name, pw); err == nil || errors.Is(err, netlogon.ErrRejected) {
			t.Errorf("probe at a controller that cannot be reached: err %v, want an error that is not ErrRejected", err)
		}
	})
}

// randomName is prefix and eight hexadecimal digits.
func randomName(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToUpper(hex.EncodeToString(b[:]))
}

// password is a random password meeting a domain's complexity rules.
func password() []byte {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return []byte("Dc1!" + hex.EncodeToString(b[:]))
}
