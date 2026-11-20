package install_test

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// sources is how many source counts the reset's test writes: more than one
// of ClearSourceSignIns' batches holds over Badger.
const sources = 1000

// installedFix is a fix whose installation dfs init finished.
func installedFix(t *testing.T) *fix {
	t.Helper()
	f := newFix(t)
	if err := install.Run(t.Context(), f.deps()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.out.Reset()
	return f
}

// protocolKey writes and opens a protocol key file beside the storage key,
// sharing its passphrase.
func (f *fix) protocolKey() keyprovider.KeyProvider {
	f.t.Helper()
	b, err := keyprovider.GenerateKeyFile(os.Getenv("DITTOFS_ENCRYPTION_PASSPHRASE"))
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.root, "protocol.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(f.t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = kp.Close() })
	return kp
}

func (f *fix) resetDeps(name string) install.ResetDeps {
	return install.ResetDeps{
		KV:         f.db,
		StorageKey: f.key,
		Name:       name,
		Host:       "host-a",
		Node:       entity.NodeID{'h'},
		Out:        &f.out,
		Logger:     f.deps().Logger,
	}
}

// generated is the password the reset printed on out.
func generated(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "generated password") {
		t.Fatalf("reset output %q: want the notice and the password", out)
	}
	return lines[1]
}

// counts writes the account's count, a pair's, and sources source counts
// in batches of 100, as failed sign-ins would.
func (f *fix) counts(account entity.Principal) {
	f.t.Helper()
	cp := f.store().ControlPlane()
	e := f.clock.Now()
	src := func(i int) netip.Prefix {
		return netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), 32)
	}
	for lo := 0; lo < sources; lo += 100 {
		err := cp.Write(f.t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			for i := lo; i < min(lo+100, sources); i++ {
				if err := tx.PutSignInFailures(entity.SignInKey{Source: src(i)}, entity.SignInFailures{Count: 1, Last: e, Failed: e}); err != nil {
					return err
				}
			}
			if lo > 0 {
				return nil
			}
			for _, k := range []entity.SignInKey{{Account: account}, {Account: account, Source: src(0)}} {
				if err := tx.PutSignInFailures(k, entity.SignInFailures{Count: 100, Last: e}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			f.t.Fatalf("write sign-in counts: %v", err)
		}
	}
}

// The reset sets a new generated password the account signs in with, marks
// it must-change, prunes the old version, raises not-before past a session
// signed in before it, resets the account's count and clears every
// source's, records one audit record whose actor is the host, and prints the
// password once, never to the log.
func TestResetAdmin(t *testing.T) {
	f := installedFix(t)
	ctx := t.Context()
	before := f.admin()
	cp := f.store().ControlPlane()
	sess := entity.Session{ID: [16]byte{1}, Account: before.Principal, Refresh: [32]byte{1},
		Created: f.clock.Now(), Expires: f.clock.Now().Add(24 * time.Hour)}
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error { return tx.PutSession(sess) }); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	f.counts(before.Principal)
	f.clock.Advance(500 * time.Millisecond)

	if err := install.ResetAdmin(ctx, f.resetDeps(install.AdminName)); err != nil {
		t.Fatalf("ResetAdmin: %v", err)
	}
	password := generated(t, f.out.String())
	if strings.Contains(f.log.String(), password) {
		t.Errorf("log %q: holds the generated password", f.log.String())
	}
	if err := f.verify(before.Principal, password); err != nil {
		t.Errorf("sign in with the printed password: %v, want it accepted", err)
	}
	after := f.admin()
	if !after.MustChange {
		t.Errorf("must-change after the reset: false, want true")
	}
	if !sess.Created.Before(after.NotBefore) {
		t.Errorf("not-before %v after the reset: want it after the session signed in before it, created %v", after.NotBefore, sess.Created)
	}
	if after.Password != before.Password {
		t.Errorf("password reference: got %x, want the account's own %x", after.Password, before.Password)
	}
	if _, err := cp.Secret(ctx, before.Password, 1); err == nil {
		t.Errorf("the old password's version 1 after the reset: found, want it pruned")
	}

	if _, ok, err := cp.SignInFailures(ctx, entity.SignInKey{Account: before.Principal}); err != nil || ok {
		t.Errorf("the account's sign-in count after the reset: found %v, %v; want it gone", ok, err)
	}
	left := 0
	for i := range sources {
		k := entity.SignInKey{Source: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), 32)}
		if _, ok, err := cp.SignInFailures(ctx, k); err != nil || ok {
			left++
		}
	}
	if left != 0 {
		t.Errorf("source sign-in counts after the reset: %d of %d left, want none", left, sources)
	}

	var recs []entity.AuditRecord
	for r, err := range cp.AuditRecords(ctx, entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatalf("AuditRecords: %v", err)
		}
		recs = append(recs, r)
	}
	if len(recs) != 1 {
		t.Fatalf("audit records: got %d, want the reset's one", len(recs))
	}
	r := recs[0]
	if r.Actor != (entity.Principal{}) || r.ActorName != "host-a" || r.Credential != "host:host-a" {
		t.Errorf("audit actor: got %v %q %q, want no account and the host host-a", r.Actor, r.ActorName, r.Credential)
	}
	if !bytes.Contains(r.Request, []byte("dfs reset-admin")) {
		t.Errorf("audit request %s: want the command named", r.Request)
	}
	if !bytes.Equal(r.Target.ID, before.Principal.ID[:]) || r.Before != before.Generation || r.After != after.Generation {
		t.Errorf("audit target %v %d->%d: want the account %v %d->%d", r.Target, r.Before, r.After, before.Principal, before.Generation, after.Generation)
	}
}

// --name resets another administrator. One holding a file spelling, with
// NTLM on, gets a new NT hash of the generated password, sealed under the
// protocol key, its old version pruned, and a password hash it had none of.
// With no protocol key the reset refuses and changes nothing.
func TestResetAdminNTHash(t *testing.T) {
	f := installedFix(t)
	ctx := t.Context()
	pk := f.protocolKey()
	cp := f.store().ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "ops", Role: entity.RoleAdmin,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "4000"}}}
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{RID: true})
	if err != nil {
		t.Fatal(err)
	}
	h, err := pid.NewHashes(u, []byte("old"), pid.NTLMSetting{On: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: pk})
	if err != nil {
		t.Fatal(err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: u.Principal.ID[:]}, Key: "nt_hash"}
	old, err := p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, owner, entity.SecretRef{7}, h.NTHash, secrets.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(old, 0); err != nil {
			return err
		}
		cur, _, err := tx.User(u.Principal)
		if err != nil {
			return err
		}
		cur.NTHash = old.ID
		_, err = tx.PutUser(cur, gen, entity.Alloc{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	u, _, err = cp.UserNamed(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}

	if err := install.ResetAdmin(ctx, f.resetDeps("ops")); err == nil || f.out.Len() != 0 {
		t.Fatalf("ResetAdmin without a protocol key: got %v, printed %q; want a refusal, nothing printed", err, f.out.String())
	}
	if got, _, _ := cp.UserNamed(ctx, "ops"); got.Generation != u.Generation {
		t.Fatalf("account after the refused reset: generation %d, want %d unchanged", got.Generation, u.Generation)
	}

	d := f.resetDeps("ops")
	d.ProtocolKey = pk
	if err := install.ResetAdmin(ctx, d); err != nil {
		t.Fatalf("ResetAdmin: %v", err)
	}
	password := generated(t, f.out.String())
	want, err := pid.NewHashes(u, []byte(password), pid.NTLMSetting{On: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve(ctx, secrets.Protocol, entity.SecretNTHash, owner, u.NTHash)
	if err != nil {
		t.Fatalf("resolve the NT hash: %v", err)
	}
	if !bytes.Equal(got, want.NTHash) {
		t.Errorf("NT hash after the reset: got %x, want the generated password's %x", got, want.NTHash)
	}
	if _, err := cp.Secret(ctx, u.NTHash, 1); err == nil {
		t.Errorf("the old NT hash's version 1 after the reset: found, want it pruned")
	}
	if err := f.verify(u.Principal, password); err != nil {
		t.Errorf("sign in as ops with the printed password: %v, want it accepted", err)
	}
}

// --name of a missing account, a disabled administrator and a non-admin
// each refuses, naming why, and changes nothing: no record, no audit, no
// output.
func TestResetAdminRefusesTheAccount(t *testing.T) {
	f := installedFix(t)
	ctx := t.Context()
	cp := f.store().ControlPlane()
	for _, u := range []entity.User{
		{Principal: entity.NewUser(), Name: "off", Role: entity.RoleAdmin, Disabled: true},
		{Principal: entity.NewUser(), Name: "viewer", Role: entity.RoleViewer},
	} {
		if _, err := cp.PutUser(ctx, u, 0, entity.Alloc{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, why string }{
		{"nobody", "no account"},
		{"off", "disabled"},
		{"viewer", "not admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.users()
			err := install.ResetAdmin(ctx, f.resetDeps(tc.name))
			if !errors.Is(err, install.ErrNotResettable) || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("ResetAdmin --name %s: got %v, want ErrNotResettable naming %q", tc.name, err, tc.why)
			}
			if f.out.Len() != 0 {
				t.Errorf("output: %q, want nothing", f.out.String())
			}
			after := f.users()
			for i := range before {
				if after[i].Generation != before[i].Generation {
					t.Errorf("user %q: generation %d, want %d unchanged", before[i].Name, after[i].Generation, before[i].Generation)
				}
			}
			for r, err := range cp.AuditRecords(ctx, entity.AuditFilter{}, nil) {
				t.Errorf("audit record after a refusal: %v (%v), want none", r, err)
			}
		})
	}
}

// Stopped after its write commits, or printing to a failing writer, the
// reset has set a password nobody was shown, and its error names a rerun of
// dfs reset-admin.
func TestResetAdminFailureAfterTheCommitNamesARerun(t *testing.T) {
	f := installedFix(t)
	d := install.ResetAfter(f.resetDeps(install.AdminName), func(write string) error {
		if write == "account" {
			return install.ErrCrash
		}
		return nil
	})
	if err := install.ResetAdmin(t.Context(), d); !errors.Is(err, install.ErrCrash) || f.out.Len() != 0 {
		t.Fatalf("ResetAdmin stopped after its write: got %v, printed %q; want the crash point, nothing printed", err, f.out.String())
	}
	if !f.admin().MustChange {
		t.Fatalf("must-change after the stopped reset: false, want the write committed")
	}
	d = f.resetDeps(install.AdminName)
	d.Out = failingWriter{}
	if err := install.ResetAdmin(t.Context(), d); err == nil || !strings.Contains(err.Error(), "run dfs reset-admin again") {
		t.Errorf("ResetAdmin printing to a failing writer: got %v, want a rerun of dfs reset-admin named", err)
	}
}
