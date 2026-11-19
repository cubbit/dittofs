package identity_test

import (
	"bytes"
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
// the last confirm naming version confirmed at mt0.
func joinedState(confirmed uint64, versions ...secrets.Version) identity.State {
	st := state(1)
	st.Directory = &directory.Record{Address: "ldaps://dc1.corp.example", Realm: machineRealm, NetBIOSDomain: "CORP"}
	st.Machine = versions
	st.Join = entity.MachineJoin{Joined: true, Account: "NAS$", Names: []string{"cifs/NAS.corp.example", "host/nas.corp.example"}}
	st.Join.LastRotation.Version = confirmed
	st.Join.LastRotation.Confirmed = mt0
	return st
}

// tkt is an AP-REQ for svc under kvno, encrypted under key, issued at now.
func tkt(svc string, kvno int, key []byte, now time.Time) []byte {
	r := identitytest.NewRealm(machineRealm)
	return r.Ticket(identitytest.TicketOpts{Service: svc, KVNO: kvno, Key: key, Now: now})
}

// verify is the applied State's verdict on b at now: nil or the error.
func verify(s *identity.Server, b []byte, now time.Time) error {
	return s.VerifyAPReq(b, pkgid.ServiceCIFS, pkgid.TransportSMB, netip.MustParseAddr("192.0.2.1"), now)
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
		name   string
		ticket func(t *testing.T) []byte
		accept bool
	}
	under := func(svc string, kvno int, password string) func(t *testing.T) []byte {
		return func(t *testing.T) []byte { return tkt(svc, kvno, machineKey(t, password), mt0) }
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
				{"number 7", under(machineSvc, 7, "pw7"), true},
				{"number 6", under(machineSvc, 6, "pw6"), true},
				{"number 9, unheld, under the current key", under(machineSvc, 9, "pw7"), true},
				{"number 9, unheld, under the previous key", under(machineSvc, 9, "pw6"), true},
				{"the name in upper case", under("cifs/NAS.CORP.EXAMPLE", 7, "pw7"), true},
				{"number 7 under the previous key", under(machineSvc, 7, "pw6"), false},
				{"a name the account does not hold", under("cifs/other.corp.example", 7, "pw7"), false},
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
				{"number 300", under(machineSvc, 300, "pw300"), true},
				{"number 44", under(machineSvc, 44, "pw44"), true},
				{"number 44 under the key tagged 300", under(machineSvc, 44, "pw300"), false},
			},
		},
		{
			name: "not joined, as a failed join leaves the versions",
			state: func(*testing.T) identity.State {
				st := joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))
				st.Join.Joined = false
				return st
			},
			checks: []check{
				{"number 7", under(machineSvc, 7, "pw7"), false},
				{"number 6", under(machineSvc, 6, "pw6"), false},
			},
		},
		{
			name:   "a HandedOver version alone",
			state:  func(*testing.T) identity.State { return joinedState(0, ver(1, 1, "offline", true)) },
			checks: []check{{"its number", under(machineSvc, 1, "offline"), false}},
		},
		{
			name: "a HandedOver version current beside a confirmed one",
			state: func(*testing.T) identity.State {
				return joinedState(2, ver(3, 8, "offline", true), ver(2, 7, "pw7", false))
			},
			checks: []check{
				{"the handed-over number", under(machineSvc, 8, "offline"), false},
				{"the confirmed number", under(machineSvc, 7, "pw7"), true},
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
				{"under the imported key", func(t *testing.T) []byte {
					kt := keytab.New()
					if err := kt.Unmarshal(imported(t)); err != nil {
						t.Fatal(err)
					}
					return tkt(machineSvc, 7, kt.Entries[0].Key.KeyValue, mt0)
				}, false},
				{"under the derived key", under(machineSvc, 7, "pw7"), true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			if err := s.Apply(tc.state(t)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, c := range tc.checks {
				err := verify(s, c.ticket(t), mt0)
				switch {
				case c.accept && err != nil:
					t.Errorf("%s: verify = %v, want accepted", c.name, err)
				case !c.accept && !errors.Is(err, vfserr.ErrLogonFailure):
					t.Errorf("%s: verify = %v, want ErrLogonFailure", c.name, err)
				}
			}
		})
	}
}

// A version no longer delivered derives no key from the next Apply on.
func TestMachineKeysVersionDropped(t *testing.T) {
	s := newServer(t)
	if err := s.Apply(joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))); err != nil {
		t.Fatal(err)
	}
	b := tkt(machineSvc, 6, machineKey(t, "pw6"), mt0)
	if err := verify(s, b, mt0); err != nil {
		t.Fatalf("number 6 while held: verify = %v, want accepted", err)
	}
	if err := s.Apply(joinedState(2, ver(2, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	if err := verify(s, b, mt0); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("number 6 after its version was dropped: verify = %v, want ErrLogonFailure", err)
	}
}

// The previous version's key is accepted until 10 hours plus the 5-minute skew
// after the confirm, with no second rotation, and refused past it, by a fresh
// server too.
func TestMachineKeysRetention(t *testing.T) {
	st := joinedState(2, ver(2, 7, "pw7", false), ver(1, 6, "pw6", false))
	prof.Watch(t, prof.Options{})
	store := newStore(t)
	s, err := identity.New(identity.Deps{Store: store, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(st); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) (time.Time, []byte) {
		now := mt0.Add(d)
		return now, tkt(machineSvc, 6, machineKey(t, "pw6"), now)
	}
	now, b := at(10*time.Hour + 4*time.Minute)
	if err := verify(s, b, now); err != nil {
		t.Errorf("number 6 at 10h04m after the confirm: verify = %v, want accepted", err)
	}
	now, b = at(10*time.Hour + 5*time.Minute + time.Second)
	if err := verify(s, b, now); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("number 6 at 10h05m01s after the confirm: verify = %v, want ErrLogonFailure", err)
	}
	fresh, err := identity.New(identity.Deps{Store: store, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Apply(st); err != nil {
		t.Fatal(err)
	}
	if err := verify(fresh, b, now); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("number 6 at 10h05m01s on a fresh server: verify = %v, want ErrLogonFailure", err)
	}
	if err := verify(fresh, tkt(machineSvc, 7, machineKey(t, "pw7"), now), now); err != nil {
		t.Errorf("number 7 at 10h05m01s: verify = %v, want accepted", err)
	}
	if err := s.Apply(joinedState(2, ver(3, 8, "pw8", false), ver(2, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	if err := verify(s, tkt(machineSvc, 8, machineKey(t, "pw8"), now), now); err != nil {
		t.Errorf("a sealed version above the confirmed one at 10h05m01s: verify = %v, want accepted", err)
	}
}

// Of the versions below the confirmed one, only the newest derives a key, so a
// version two rotations back is refused while the one just below is accepted.
func TestMachineKeysRetentionOneBelow(t *testing.T) {
	s := newServer(t)
	if err := s.Apply(joinedState(3, ver(3, 9, "pw9", false), ver(2, 8, "pw8", false), ver(1, 7, "pw7", false))); err != nil {
		t.Fatal(err)
	}
	now := mt0.Add(10 * time.Hour)
	if err := verify(s, tkt(machineSvc, 7, machineKey(t, "pw7"), now), now); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("number 7, two versions below the confirmed one, at 10h: verify = %v, want ErrLogonFailure", err)
	}
	if err := verify(s, tkt(machineSvc, 8, machineKey(t, "pw8"), now), now); err != nil {
		t.Errorf("number 8, just below the confirmed one, at 10h: verify = %v, want accepted", err)
	}
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
