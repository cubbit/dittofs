package identity_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/prof"
	pkgid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// RFC 3962 appendix B's string-to-key vectors for "password" under
// "ATHENA.MIT.EDUraeburn".
func TestStringToKeyRFC3962(t *testing.T) {
	for _, tc := range []struct {
		iter   uint32
		etype  int32
		hexKey string
	}{
		{1, etypeID.AES128_CTS_HMAC_SHA1_96, "42263c6e89f4fc28b8df68ee09799f15"},
		{1, etypeID.AES256_CTS_HMAC_SHA1_96, "fe697b52bc0d3ce14432ba036a92e65bbb52280990a2fa27883998d72af30161"},
		{2, etypeID.AES128_CTS_HMAC_SHA1_96, "c651bf29e2300ac27fa469d693bdda13"},
		{2, etypeID.AES256_CTS_HMAC_SHA1_96, "a2e16d16b36069c135d5e9d2e25f896102685618b95914b467c67622225824ff"},
		{1200, etypeID.AES128_CTS_HMAC_SHA1_96, "4c01cd46d632d01e6dbe230a01ed642a"},
		{1200, etypeID.AES256_CTS_HMAC_SHA1_96, "55a6ac740ad17b4846941051e1e8b0a7548d93b0ab30a8bc3ff16280382b8c2a"},
	} {
		k, err := identity.StringToKey(tc.etype, []byte("password"), "ATHENA.MIT.EDUraeburn", tc.iter)
		if err != nil {
			t.Fatalf("StringToKey(etype %d, %d iterations): %v", tc.etype, tc.iter, err)
		}
		if got := hex.EncodeToString(k); got != tc.hexKey {
			t.Errorf("StringToKey(etype %d, %d iterations) = %s, want %s", tc.etype, tc.iter, got, tc.hexKey)
		}
	}
}

const (
	machineRealm = "CORP.EXAMPLE"
	machineSvc   = "cifs/nas.corp.example"
	// machineSalt is the computer account NAS$'s salt in CORP.EXAMPLE, written
	// out rather than built by the code under test.
	machineSalt = "CORP.EXAMPLEhostnas.corp.example"
)

// mt0 is the store time of the last confirm in these tests.
var mt0 = time.Date(2031, 3, 4, 12, 0, 0, 0, time.UTC)

// machineKey is gokrb5's AES256 key for password under machineSalt, at its
// default iteration count, RFC 3962's 4096.
func machineKey(t *testing.T, password string) []byte {
	t.Helper()
	et, err := crypto.GetEtype(etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	k, err := et.StringToKey(password, machineSalt, et.GetDefaultStringToKeyParams())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// ver is a machine password version numbered n, tagged kvno.
func ver(n uint64, kvno uint32, password string, handedOver bool) secrets.Version {
	return secrets.Version{Number: n, Value: []byte(password), Meta: secrets.Meta{KeyVersion: kvno, HandedOver: handedOver}}
}

// joinedState is a State joined as NAS$ in CORP.EXAMPLE, holding versions,
// the last confirm naming version confirmed at mt0. Its directory takes
// tickets without a PAC, so a ticket the derived keys verify signs in.
func joinedState(confirmed uint64, versions ...secrets.Version) identity.State {
	st := state(1)
	noPAC := false
	st.Directory = &directory.Record{Address: "ldaps://dc1.corp.example", Realm: machineRealm, NetBIOSDomain: "CORP", RequirePAC: &noPAC}
	st.Machine = versions
	st.Join = entity.MachineJoin{Joined: true, Account: "NAS$", Names: []string{"cifs/NAS.corp.example", "host/nas.corp.example"}}
	st.Join.LastRotation.Version = confirmed
	st.Join.LastRotation.Confirmed = mt0
	return st
}

// machineServer is a server over store on clock.
func machineServer(t *testing.T, store *kvstore.Store, clock *sim.TestClock) *identity.Server {
	t.Helper()
	s, err := identity.New(identity.Deps{Store: store, Clock: clock, Logons: &logonRecorder{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// newMachineServer is machineServer over a fresh store and clock.
func newMachineServer(t *testing.T) (*identity.Server, *sim.TestClock) {
	t.Helper()
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(mt0)
	return machineServer(t, newStore(t), clock), clock
}

// ticketSignIn signs in over SMB with a ticket for svc under kvno and key, issued
// at clock's now to a machine client, minted on its first sign-in, and
// returns the outcome SignIn reports.
func ticketSignIn(s *identity.Server, clock *sim.TestClock, svc string, kvno int, key []byte) (string, error) {
	r := identitytest.NewRealm(machineRealm)
	b := r.Ticket(identitytest.TicketOpts{Client: "host/client.corp.example", Service: svc, KVNO: kvno, Key: key, Now: clock.Now()})
	_, oc, err := s.SignInOutcome(context.Background(), pkgid.SignInStep{Kerberos: b, Service: pkgid.ServiceCIFS, Transport: pkgid.TransportSMB, Client: netip.MustParseAddr("192.0.2.1")})
	return oc, err
}

// wantSignIn checks a sign-in's outcome: ok and no error, or want with
// ErrLogonFailure or, for "unavailable", ErrUnavailable.
func wantSignIn(t *testing.T, what, oc string, err error, want string) {
	t.Helper()
	wantErr := vfserr.ErrLogonFailure
	switch want {
	case "ok":
		wantErr = nil
	case "unavailable":
		wantErr = vfserr.ErrUnavailable
	}
	if oc != want || (wantErr == nil) != (err == nil) || (wantErr != nil && !errors.Is(err, wantErr)) {
		t.Errorf("%s: SignIn = %v, outcome %q; want outcome %q, error %v", what, err, oc, want, wantErr)
	}
}

func TestMachineKeys(t *testing.T) {
	imported := func(t *testing.T) []byte {
		kt := keytab.New()
		if err := kt.AddEntry(machineSvc, machineRealm, "imported", time.Unix(1, 0), 7, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
			t.Fatal(err)
		}
		b, err := kt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	type check struct {
		name string
		svc  string
		kvno int
		key  func(t *testing.T) []byte
		want string // the outcome SignIn reports
	}
	under := func(password string) func(t *testing.T) []byte {
		return func(t *testing.T) []byte { return machineKey(t, password) }
	}
	for _, tc := range []struct {
		name   string
		state  func(t *testing.T) identity.State
		checks []check
	}{
		{
			name: "two versions tagged 7 and 6",
			state: func(*testing.T) identity.State {
				return joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))
			},
			checks: []check{
				{"number 7", machineSvc, 7, under("pw7"), "ok"},
				{"number 6", machineSvc, 6, under("pw6"), "ok"},
				{"number 9, unheld, under the current key", machineSvc, 9, under("pw7"), "ok"},
				{"number 9, unheld, under the previous key", machineSvc, 9, under("pw6"), "ok"},
				{"the name in upper case", "cifs/NAS.CORP.EXAMPLE", 7, under("pw7"), "ok"},
				{"number 7 under the previous key", machineSvc, 7, under("pw6"), "logon_failure"},
				{"a name the account does not hold", "cifs/other.corp.example", 7, under("pw7"), "logon_failure"},
			},
		},
		{
			// A number above 255 truncated to a keytab file's 8 bits would read
			// as 44, the previous version's number, and shadow its key.
			name: "a number above 255",
			state: func(*testing.T) identity.State {
				return joinedState(2, ver(2, 300, "pw300", false), ver(1, 44, "pw44", false))
			},
			checks: []check{
				{"number 300", machineSvc, 300, under("pw300"), "ok"},
				{"number 44", machineSvc, 44, under("pw44"), "ok"},
				{"number 44 under the key tagged 300", machineSvc, 44, under("pw300"), "logon_failure"},
			},
		},
		{
			// Nothing derived leaves the server no service key at all, which
			// SignIn reports as unavailable.
			name: "not joined, as a failed join leaves the versions",
			state: func(*testing.T) identity.State {
				st := joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))
				st.Join.Joined = false
				return st
			},
			checks: []check{
				{"number 7", machineSvc, 7, under("pw7"), "unavailable"},
				{"number 6", machineSvc, 6, under("pw6"), "unavailable"},
			},
		},
		{
			name:   "joined with no versions",
			state:  func(*testing.T) identity.State { return joinedState(2) },
			checks: []check{{"number 7", machineSvc, 7, under("pw7"), "unavailable"}},
		},
		{
			name:   "a HandedOver version alone",
			state:  func(*testing.T) identity.State { return joinedState(0, ver(1, 1, "offline", true)) },
			checks: []check{{"its number", machineSvc, 1, under("offline"), "unavailable"}},
		},
		{
			name: "a HandedOver version current beside a confirmed one",
			state: func(*testing.T) identity.State {
				return joinedState(2, ver(3, 8, "offline", true), ver(2, 7, "pw7", false))
			},
			checks: []check{
				{"the handed-over number", machineSvc, 8, under("offline"), "logon_failure"},
				{"the confirmed number", machineSvc, 7, under("pw7"), "ok"},
			},
		},
		{
			name: "an imported key for an account's name",
			state: func(t *testing.T) identity.State {
				st := joinedState(1, ver(1, 7, "pw7", false))
				st.Keytab = []secrets.Version{{Number: 1, Value: imported(t)}}
				return st
			},
			checks: []check{
				{"under the imported key", machineSvc, 7, func(t *testing.T) []byte {
					kt := keytab.New()
					if err := kt.Unmarshal(imported(t)); err != nil {
						t.Fatal(err)
					}
					return kt.Entries[0].Key.KeyValue
				}, "logon_failure"},
				{"under the derived key", machineSvc, 7, under("pw7"), "ok"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := newMachineServer(t)
			if err := s.Apply(tc.state(t)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, c := range tc.checks {
				oc, err := ticketSignIn(s, clock, c.svc, c.kvno, c.key(t))
				wantSignIn(t, c.name, oc, err, c.want)
			}
		})
	}
}

// A version no longer delivered derives no key from the next Apply on.
func TestMachineKeysVersionDropped(t *testing.T) {
	s, clock := newMachineServer(t)
	if err := s.Apply(joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))); err != nil {
		t.Fatal(err)
	}
	oc, err := ticketSignIn(s, clock, machineSvc, 6, machineKey(t, "pw6"))
	wantSignIn(t, "number 6 while held", oc, err, "ok")
	if err := s.Apply(joinedState(2, ver(2, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	oc, err = ticketSignIn(s, clock, machineSvc, 6, machineKey(t, "pw6"))
	wantSignIn(t, "number 6 after its version was dropped", oc, err, "logon_failure")
}

// The previous version's key is accepted until 10 hours plus the 5-minute skew
// after the confirm, with no second rotation, and refused past it, by a fresh
// server too.
func TestMachineKeysRetention(t *testing.T) {
	st := joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))
	prof.Watch(t, prof.Options{})
	store := newStore(t)
	clock := sim.NewTestClock(mt0)
	s := machineServer(t, store, clock)
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10*time.Hour + 4*time.Minute)
	oc, err := ticketSignIn(s, clock, machineSvc, 6, machineKey(t, "pw6"))
	wantSignIn(t, "number 6 at 10h04m after the confirm", oc, err, "ok")
	clock.Advance(time.Minute + time.Second)
	oc, err = ticketSignIn(s, clock, machineSvc, 6, machineKey(t, "pw6"))
	wantSignIn(t, "number 6 at 10h05m01s after the confirm", oc, err, "logon_failure")
	fresh := machineServer(t, store, clock)
	if err := fresh.Apply(st); err != nil {
		t.Fatal(err)
	}
	oc, err = ticketSignIn(fresh, clock, machineSvc, 6, machineKey(t, "pw6"))
	wantSignIn(t, "number 6 at 10h05m01s on a fresh server", oc, err, "logon_failure")
	oc, err = ticketSignIn(fresh, clock, machineSvc, 7, machineKey(t, "pw7"))
	wantSignIn(t, "number 7 at 10h05m01s on a fresh server", oc, err, "ok")
	if err := s.Apply(joinedState(2, ver(3, 8, "pw8", false), ver(2, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	oc, err = ticketSignIn(s, clock, machineSvc, 8, machineKey(t, "pw8"))
	wantSignIn(t, "a sealed version above the confirmed one at 10h05m01s", oc, err, "ok")
}

// Of the versions below the confirmed one, only the newest derives a key, so a
// version two rotations back is refused while the one just below is accepted.
func TestMachineKeysRetentionOneBelow(t *testing.T) {
	s, clock := newMachineServer(t)
	if err := s.Apply(joinedState(3, ver(3, 9, "pw9", false), ver(2, 8, "pw8", false), ver(1, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Hour)
	oc, err := ticketSignIn(s, clock, machineSvc, 7, machineKey(t, "pw7"))
	wantSignIn(t, "number 7, two versions below the confirmed one, at 10h", oc, err, "logon_failure")
	oc, err = ticketSignIn(s, clock, machineSvc, 8, machineKey(t, "pw8"))
	wantSignIn(t, "number 8, just below the confirmed one, at 10h", oc, err, "ok")
}

// An imported key for one of the account's names is logged at the Apply that
// drops it, not again at an equal Apply.
func TestMachineKeysImportedDropLoggedOnce(t *testing.T) {
	kt := keytab.New()
	if err := kt.AddEntry(machineSvc, machineRealm, "imported", time.Unix(1, 0), 7, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatal(err)
	}
	b, err := kt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	st := joinedState(1, ver(1, 7, "pw7", false))
	st.Keytab = []secrets.Version{{Number: 1, Value: b}}
	logs := &bytes.Buffer{}
	prof.Watch(t, prof.Options{})
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(time.Unix(0, 0)), Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Apply(st); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "imported key for a computer account's name skipped"); n != 1 {
		t.Errorf("drop log lines after two equal Applies = %d, want 1: %s", n, logs)
	}
}
