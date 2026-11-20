package identity_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Every spelling of alice's name gives her principal, an API-only account
// is found too, and a name no account holds gives the zero principal; none
// raises the principals generation.
func TestLookup(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	admin := entity.User{Principal: entity.NewUser(), Name: "admin"}
	r.user(t, admin, 0)
	before := r.pgen(t)

	for _, c := range []struct {
		name string
		want entity.Principal
	}{
		{"alice", alice.Principal},
		{"ALICE", alice.Principal},
		{`CORP\alice`, alice.Principal},
		{"alice@CORP.EXAMPLE", alice.Principal},
		{"admin", admin.Principal},
		{"nobody", entity.Principal{}},
	} {
		if got, err := r.srv.Lookup(ctx, c.name); err != nil || got != c.want {
			t.Errorf("Lookup(%q) = %v, %v; want %v, nil", c.name, got, err, c.want)
		}
	}
	if after := r.pgen(t); after != before {
		t.Errorf("principals generation after the lookups = %d, want %d unchanged", after, before)
	}
}

// A store that cannot answer is ErrUnavailable, never the zero principal of
// a name no account holds.
func TestLookupStoreOutage(t *testing.T) {
	r := naming(t)
	r.fail(t, "*", "*")
	if p, err := r.srv.Lookup(context.Background(), "alice"); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Lookup over a failing store = %v, %v; want ErrUnavailable", p, err)
	}
}

// Account reads the store on every call: a user disabled with no Apply since
// is refused, as are the zero principal and a machine's, and an outage is
// ErrUnavailable.
func TestAccount(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	gen := r.user(t, alice, 0)

	if u, err := r.srv.Account(ctx, alice.Principal); err != nil || u.Name != "alice" {
		t.Fatalf("Account(alice) = %+v, %v; want her record", u, err)
	}
	if u, err := r.srv.Account(ctx, entity.Principal{}); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(zero principal) = %+v, %v; want ErrLogonFailure", u, err)
	}
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/build01.corp.example@CORP.EXAMPLE"}}}
	r.user(t, m, 0)
	if u, err := r.srv.Account(ctx, m.Principal); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(a machine) = %+v, %v; want ErrLogonFailure", u, err)
	}
	alice.Disabled = true
	r.user(t, alice, gen)
	if u, err := r.srv.Account(ctx, alice.Principal); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(alice) once disabled = %+v, %v; want ErrLogonFailure", u, err)
	}
	r.fail(t, "*", "*")
	if u, err := r.srv.Account(ctx, alice.Principal); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Account over a failing store = %+v, %v; want ErrUnavailable", u, err)
	}
}

// A server built without a secret provider refuses alice's right password
// ErrUnavailable, never ErrLogonFailure and never a panic.
func TestVerifyPasswordWithoutSecrets(t *testing.T) {
	r := naming(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.withPassword(t, &alice, alicePassword, nil)
	r.user(t, alice, 0)
	if err := r.srv.VerifyPassword(context.Background(), alice.Principal, []byte(alicePassword)); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("VerifyPassword with no secret provider = %v, want ErrUnavailable", err)
	}
}

const alicePassword = "Password"

// signing is a naming fixture whose server resolves secrets through the real
// provider, over real key files and the same fault-wrapped store, records
// its logon events, metrics and spans, and logs at Debug.
func signing(t *testing.T) (*resolving, *secrets.Provider) {
	t.Helper()
	r := naming(t)
	p, err := secrets.New(r.cp, roleKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	r.clock = sim.NewTestClock(time.Unix(0, 0))
	r.logons = &logonRecorder{}
	r.logs.Reset()
	debug := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if r.srv, err = identity.New(identity.Deps{Store: r.st, Clock: r.clock, Secrets: p, Metrics: r.reg,
		Tracer: r.tp.Tracer("test"), Logger: debug, Logons: r.logons}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.srv.Apply(state(1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return r, p
}

// roleKeys writes one real key file per role and opens both.
func roleKeys(t *testing.T) map[secrets.Role]keyprovider.KeyProvider {
	t.Helper()
	const passphrase = "sign-in test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	keys := map[secrets.Role]keyprovider.KeyProvider{}
	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		kp, err := keyprovider.NewProvider(context.Background(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kp.Close() })
		keys[role] = kp
	}
	return keys
}

// withPassword derives u's hash with NewHashes, NTLM off, and names it in
// u.Password; with a provider it also seals and stores it, owned by u's
// record under "password" as the API stores it.
func (r *resolving) withPassword(t *testing.T, u *entity.User, password string, p *secrets.Provider) {
	t.Helper()
	h, err := pid.NewHashes(*u, []byte(password), pid.NTLMSetting{})
	if err != nil {
		t.Fatal(err)
	}
	u.Password = entity.SecretRef(uuid.New())
	if p != nil {
		r.sealPassword(t, p, *u, h.Password)
	}
}

// sealPassword seals hash as u.Password's first version and stores it.
func (r *resolving) sealPassword(t *testing.T, p *secrets.Provider, u entity.User, hash []byte) {
	t.Helper()
	ctx := context.Background()
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])}, Key: "password"}
	sec, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, owner, u.Password, hash, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cp.PutSecret(ctx, sec, 0); err != nil {
		t.Fatal(err)
	}
}

// Alice signs in with her password only. A wrong password, the zero
// principal, a disabled user and one with no password are each
// ErrLogonFailure, as R26 lists them.
func TestVerifyPassword(t *testing.T) {
	r, p := signing(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.withPassword(t, &alice, alicePassword, p)
	r.user(t, alice, 0)
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1501"), Disabled: true}
	r.withPassword(t, &bob, "bob's password", p)
	r.user(t, bob, 0)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502")}
	r.user(t, carol, 0)

	if err := r.srv.VerifyPassword(ctx, alice.Principal, []byte(alicePassword)); err != nil {
		t.Fatalf("VerifyPassword(alice, her password) = %v, want nil", err)
	}
	for _, c := range []struct {
		name     string
		p        entity.Principal
		password string
	}{
		{"alice, a wrong password", alice.Principal, "password"},
		{"the zero principal", entity.Principal{}, alicePassword},
		{"the zero principal, the dummy hash's password", entity.Principal{}, "dummy"},
		{"bob, disabled, his password", bob.Principal, "bob's password"},
		{"carol, no password", carol.Principal, ""},
	} {
		if err := r.srv.VerifyPassword(ctx, c.p, []byte(c.password)); !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("VerifyPassword(%s) = %v, want ErrLogonFailure", c.name, err)
		}
	}
}

// A user whose deletion has run to its end, and one marked deleted whose
// batches have not run, are each refused by VerifyPassword with the right
// password and by Account, as an API token issued before the deletion
// reaches it: ErrLogonFailure.
func TestDeletedUserSignsInNowhere(t *testing.T) {
	r, p := signing(t)
	ctx := context.Background()
	gone := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.withPassword(t, &gone, alicePassword, p)
	goneGen := r.user(t, gone, 0)
	marked := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1501")}
	r.withPassword(t, &marked, alicePassword, p)
	markedGen := r.user(t, marked, 0)
	for _, u := range []entity.User{gone, marked} {
		if err := r.srv.VerifyPassword(ctx, u.Principal, []byte(alicePassword)); err != nil {
			t.Fatalf("VerifyPassword(%s) before the deletion = %v, want nil", u.Name, err)
		}
	}

	if err := r.cp.DeleteUser(ctx, gone.Principal, goneGen); err != nil {
		t.Fatal(err)
	}
	for done := false; !done; {
		var err error
		if done, err = r.st.Nodes().ContinueDelete(ctx, gone.Principal); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.cp.DeleteUser(ctx, marked.Principal, markedGen); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		u    entity.User
	}{
		{"deleted", gone},
		{"marked deleted", marked},
	} {
		if err := r.srv.VerifyPassword(ctx, c.u.Principal, []byte(alicePassword)); !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("VerifyPassword(%s user, the right password) = %v, want ErrLogonFailure", c.name, err)
		}
		if u, err := r.srv.Account(ctx, c.u.Principal); !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("Account(%s user) = %+v, %v; want ErrLogonFailure", c.name, u, err)
		}
	}
}

// A stored hash that is not an argon2id PHC string is ErrUnavailable, never
// a wrong password.
func TestVerifyPasswordCorruptHash(t *testing.T) {
	r, p := signing(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500"), Password: entity.SecretRef(uuid.New())}
	r.sealPassword(t, p, alice, []byte("$argon2id$v=19$m=19456,t=2,p=1$not base64!$"))
	r.user(t, alice, 0)
	if err := r.srv.VerifyPassword(context.Background(), alice.Principal, []byte(alicePassword)); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("VerifyPassword against a corrupt hash = %v, want ErrUnavailable", err)
	}
}

// A store that cannot answer, the user's read or the hash's, is
// ErrUnavailable, never a wrong password.
func TestVerifyPasswordStoreOutage(t *testing.T) {
	for _, c := range []struct {
		name, op, key string
	}{
		{"the user's read", kvtest.OpGet, "U‖*"},
		{"the hash's read", kvtest.OpBefore, "SEC‖*"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, p := signing(t)
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
			r.withPassword(t, &alice, alicePassword, p)
			r.user(t, alice, 0)
			r.fail(t, c.op, c.key)
			if err := r.srv.VerifyPassword(context.Background(), alice.Principal, []byte(alicePassword)); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("VerifyPassword with %s failing = %v, want ErrUnavailable", c.name, err)
			}
		})
	}
}

// The zero principal's call, and that of a user with no password, verifies
// against an argon2id hash with the parameters a stored one has, as a wrong
// password's does, and never short-cuts.
func TestVerifyPasswordHashesWithoutAnAccount(t *testing.T) {
	r, _ := signing(t)
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", IDs: fileIDs("1502")}
	r.user(t, carol, 0)
	for _, c := range []struct {
		name string
		p    entity.Principal
	}{
		{"the zero principal", entity.Principal{}},
		{"carol, no password", carol.Principal},
	} {
		var got [][]byte
		r.srv.OnVerify(func(phc []byte) { got = append(got, phc) })
		if err := r.srv.VerifyPassword(context.Background(), c.p, []byte(alicePassword)); !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Fatalf("VerifyPassword(%s) = %v, want ErrLogonFailure", c.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("hashes verified for %s = %d, want 1", c.name, len(got))
		}
		m, tm, th, _, key, err := identity.ParsePHC(got[0])
		if err != nil || m != 19456 || tm != 2 || th != 1 || len(key) != 32 {
			t.Errorf("hash verified for %s = m=%d t=%d p=%d key %d bytes, %v; want m=19456 t=2 p=1, 32 bytes",
				c.name, m, tm, th, len(key), err)
		}
	}
}

// With every hashing slot held, alice's sign-in waits 5 s on the clock and
// answers ErrUnavailable, leaving no goroutine behind.
func TestVerifyPasswordWaitsForASlot(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	st := newStore(t)
	p, err := secrets.New(st.ControlPlane(), roleKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := identity.New(identity.Deps{Store: st, Clock: clock, Secrets: p})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := &resolving{cp: st.ControlPlane()}
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.withPassword(t, &alice, alicePassword, p)
	r.user(t, alice, 0)
	defer s.HoldHashSlots(0)()

	done := make(chan error, 1)
	go func() { done <- s.VerifyPassword(ctx, alice.Principal, []byte(alicePassword)) }()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() == 0 {
		select {
		case err := <-done:
			t.Fatalf("VerifyPassword with every slot held = %v without waiting on the clock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the verification never waited on the clock for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(hashWait)
	select {
	case err := <-done:
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("VerifyPassword with every slot held = %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("VerifyPassword did not return once the clock passed 5 s")
	}
}

// With every hashing slot held, a verification waits 5 s on the clock, not
// less, then answers ErrUnavailable and leaves no goroutine behind.
func TestHashSlotWaitIsBounded(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(0)()

	done := make(chan error, 1)
	go func() { done <- s.HashSlot(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the verification never waited on the clock for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(hashWait - time.Nanosecond)
	if n := clock.Pending(); n != 1 {
		t.Fatalf("waits pending just before 5 s = %d, want 1", n)
	}
	clock.Advance(time.Nanosecond)
	select {
	case err := <-done:
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("HashSlot with every slot held = %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HashSlot did not return once the clock passed 5 s")
	}
}

// slotServer builds a server over a test clock, under the leak check.
func slotServer(t *testing.T) (*identity.Server, *sim.TestClock) {
	t.Helper()
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: clock})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, clock
}

// A slot is given back: with one free, two verifications in turn each take
// it, and the second never waits on the clock.
func TestHashSlotIsReleased(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(1)()

	for i := range 2 {
		done := make(chan error, 1)
		go func() { done <- s.HashSlot(context.Background()) }()
		deadline := time.Now().Add(10 * time.Second)
	wait:
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("HashSlot %d with one slot free = %v, want nil", i+1, err)
				}
				break wait
			default:
			}
			if clock.Pending() != 0 {
				clock.Advance(hashWait)
				t.Fatalf("HashSlot %d waited on the clock for a slot and answered %v, want the one free slot back", i+1, <-done)
			}
			if time.Now().After(deadline) {
				t.Fatalf("HashSlot %d neither returned nor waited on the clock", i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A wait for a slot ended by its context answers the context's error.
func TestHashSlotWaitEndsWithContext(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(0)()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.HashSlot(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the verification never waited on the clock for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("HashSlot cancelled while waiting = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		clock.Advance(hashWait)
		t.Fatalf("HashSlot did not return once its context was cancelled; past the wait it answered %v", <-done)
	}
}

// hashWait is how long a verification waits for a slot.
const hashWait = 5 * time.Second
