package install_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/install"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
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

// named is the account name names.
func (f *fix) named(name string) entity.User {
	f.t.Helper()
	u, ok, err := f.store().ControlPlane().UserNamed(f.t.Context(), name)
	if err != nil || !ok {
		f.t.Fatalf("UserNamed(%q): %v, %v", name, ok, err)
	}
	return u
}

// audits is every audit record the store holds.
func (f *fix) audits() []entity.AuditRecord {
	f.t.Helper()
	var recs []entity.AuditRecord
	for r, err := range f.store().ControlPlane().AuditRecords(f.t.Context(), entity.AuditFilter{}, nil) {
		if err != nil {
			f.t.Fatalf("AuditRecords: %v", err)
		}
		recs = append(recs, r)
	}
	return recs
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

func source(i int) entity.SignInKey {
	return entity.SignInKey{Source: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), 32)}
}

// counts writes the account's count, a pair's, and sources source counts
// in batches of 100, as failed sign-ins would.
func (f *fix) counts(account entity.Principal) {
	f.t.Helper()
	cp := f.store().ControlPlane()
	e := f.clock.Now()
	for lo := 0; lo < sources; lo += 100 {
		err := cp.Write(f.t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			for i := lo; i < min(lo+100, sources); i++ {
				if err := tx.PutSignInFailures(source(i), entity.SignInFailures{Count: 1, Last: e, Failed: e}); err != nil {
					return err
				}
			}
			if lo > 0 {
				return nil
			}
			for _, k := range []entity.SignInKey{{Account: account}, {Account: account, Source: source(0).Source}} {
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

// sourcesLeft is how many of the source counts counts wrote are left.
func (f *fix) sourcesLeft() int {
	f.t.Helper()
	cp := f.store().ControlPlane()
	left := 0
	for i := range sources {
		if _, ok, err := cp.SignInFailures(f.t.Context(), source(i)); err != nil || ok {
			left++
		}
	}
	return left
}

// The reset sets a new generated password the account signs in with, marks
// it must-change, prunes the old version, raises not-before past the time
// before it, resets the account's count and clears every source's, records
// one audit record whose actor is the host, its change in the API's form,
// and prints the password once, never to the log.
func TestResetAdmin(t *testing.T) {
	f := installedFix(t)
	ctx := t.Context()
	cp := f.store().ControlPlane()
	// The first password was changed since dfs init, so must-change is
	// clear until the reset sets it.
	before := f.admin()
	before.MustChange = false
	if _, err := cp.PutUser(ctx, before, before.Generation, entity.Alloc{}); err != nil {
		t.Fatal(err)
	}
	before = f.admin()
	f.counts(before.Principal)
	t0 := f.clock.Now()
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
	if !after.NotBefore.After(t0) {
		t.Errorf("not-before %v after the reset: want it after %v, the time before the reset", after.NotBefore, t0)
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
	if left := f.sourcesLeft(); left != 0 {
		t.Errorf("source sign-in counts after the reset: %d of %d left, want none", left, sources)
	}

	recs := f.audits()
	if len(recs) != 1 {
		t.Fatalf("audit records: got %d, want the reset's one", len(recs))
	}
	r := recs[0]
	if r.Actor != (entity.Principal{}) || r.ActorName != "host-a" || r.Credential != entity.HostCredential+"host-a" {
		t.Errorf("audit actor: got %v %q %q, want no account and the host host-a", r.Actor, r.ActorName, r.Credential)
	}
	if !bytes.Contains(r.Request, []byte("dfs reset-admin")) {
		t.Errorf("audit request %s: want the command named", r.Request)
	}
	if !bytes.Equal(r.Target.ID, before.Principal.ID[:]) || r.Before != before.Generation || r.After != after.Generation {
		t.Errorf("audit target %v %d->%d: want the account %v %d->%d", r.Target, r.Before, r.After, before.Principal, before.Generation, after.Generation)
	}
	var change map[string]any
	if err := json.Unmarshal(r.Change, &change); err != nil {
		t.Fatalf("audit change %s: %v", r.Change, err)
	}
	nb, _ := after.NotBefore.MarshalJSON()
	zero, _ := time.Time{}.MarshalJSON()
	want := map[string]string{
		"password":    `"set"`,
		"must_change": `[false,true]`,
		"not_before":  `[` + string(zero) + `,` + string(nb) + `]`,
	}
	if len(change) != len(want) {
		t.Errorf("audit change %s: want exactly %v", r.Change, want)
	}
	for k, v := range want {
		got, _ := json.Marshal(change[k])
		if string(got) != v {
			t.Errorf("audit change %s: got %s, want %s", k, got, v)
		}
	}
}

// ntAccount stores an administrator "ops" with a uid, and, when old is not
// empty, the NT hash of old sealed under pk; it returns the account and the
// NT hash's owner.
func (f *fix) ntAccount(pk keyprovider.KeyProvider, old string) (entity.User, entity.SecretOwner) {
	f.t.Helper()
	ctx := f.t.Context()
	cp := f.store().ControlPlane()
	u := entity.User{Principal: entity.NewUser(), Name: "ops", Role: entity.RoleAdmin,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "4000"}}}
	gen, err := cp.PutUser(ctx, u, 0, entity.Alloc{RID: true})
	if err != nil {
		f.t.Fatal(err)
	}
	owner := entity.UserSecretOwner(u.Principal.ID, entity.UserNTHashKey)
	if old == "" {
		return f.named("ops"), owner
	}
	h, err := pid.NewHashes(u, []byte(old), pid.NTLMSetting{On: true})
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: pk})
	if err != nil {
		f.t.Fatal(err)
	}
	sec, err := p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, owner, entity.SecretRef(uuid.New()), h.NTHash, secrets.Meta{})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(sec, 0); err != nil {
			return err
		}
		cur, _, err := tx.User(u.Principal)
		if err != nil {
			return err
		}
		cur.NTHash = sec.ID
		_, err = tx.PutUser(cur, gen, entity.Alloc{})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return f.named("ops"), owner
}

// --name resets another administrator, one with a uid. With NTLM on it gets
// a new NT hash of the generated password, sealed under the protocol key,
// the old version pruned, and with no protocol key the reset refuses,
// naming keys.protocol, and changes nothing. With identity.ntlm false it
// gets none: an NT hash it held is overwritten and named no more, so the old
// password's resolves nowhere, and one holding none needs no protocol key.
func TestResetAdminNTHash(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ntlmOff     bool
		holds       bool // the account holds the old password's NT hash
		protocolKey bool
		refused     bool
	}{
		{name: "ntlm on, no protocol key", holds: true, refused: true},
		{name: "ntlm on", holds: true, protocolKey: true},
		{name: "ntlm off, holding one", ntlmOff: true, holds: true, protocolKey: true},
		{name: "ntlm off, holding none, no protocol key", ntlmOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := installedFix(t)
			ctx := t.Context()
			cp := f.store().ControlPlane()
			pk := f.protocolKey()
			old := ""
			if tc.holds {
				old = "old password"
			}
			u, owner := f.ntAccount(pk, old)
			if tc.ntlmOff {
				if _, err := cp.PutSetting(ctx, entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.ntlm", Value: []byte("false")}, 0); err != nil {
					t.Fatal(err)
				}
			}
			d := f.resetDeps("ops")
			if tc.protocolKey {
				d.ProtocolKey = pk
			}
			err := install.ResetAdmin(ctx, d)
			if tc.refused {
				if !errors.Is(err, install.ErrNoProtocolKey) || !strings.Contains(err.Error(), "keys.protocol is not set") || f.out.Len() != 0 {
					t.Fatalf("ResetAdmin: got %v, printed %q; want ErrNoProtocolKey naming keys.protocol, nothing printed", err, f.out.String())
				}
				if got := f.named("ops"); got.Generation != u.Generation {
					t.Errorf("account after the refused reset: generation %d, want %d unchanged", got.Generation, u.Generation)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResetAdmin: %v", err)
			}
			password := generated(t, f.out.String())
			if err := f.verify(u.Principal, password); err != nil {
				t.Errorf("sign in as ops with the printed password: %v, want it accepted", err)
			}
			after := f.named("ops")
			p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: pk})
			if err != nil {
				t.Fatal(err)
			}
			if tc.ntlmOff {
				if after.NTHash != (entity.SecretRef{}) {
					t.Errorf("NT hash after the reset with NTLM off: names %x, want none", after.NTHash)
				}
				if !tc.holds {
					return
				}
				oldHash, _ := pid.NewHashes(u, []byte(old), pid.NTLMSetting{On: true})
				vs, err := p.Versions(ctx, secrets.Protocol, entity.SecretNTHash, owner, u.NTHash)
				if err != nil {
					t.Fatalf("versions of the old NT hash: %v", err)
				}
				for _, v := range vs {
					if bytes.Equal(v.Value, oldHash.NTHash) {
						t.Errorf("the old NT hash after the reset: version %d still holds it, want it resolvable nowhere", v.Number)
					}
				}
				return
			}
			want, err := pid.NewHashes(u, []byte(password), pid.NTLMSetting{On: true})
			if err != nil {
				t.Fatal(err)
			}
			if after.NTHash != u.NTHash {
				t.Fatalf("NT hash reference: got %x, want the account's own %x", after.NTHash, u.NTHash)
			}
			got, err := p.Resolve(ctx, secrets.Protocol, entity.SecretNTHash, owner, after.NTHash)
			if err != nil {
				t.Fatalf("resolve the NT hash: %v", err)
			}
			if !bytes.Equal(got, want.NTHash) {
				t.Errorf("NT hash after the reset: got %x, want the generated password's %x", got, want.NTHash)
			}
			if _, err := cp.Secret(ctx, u.NTHash, 1); err == nil {
				t.Errorf("the old NT hash's version 1 after the reset: found, want it pruned")
			}
		})
	}
}

// --name of a missing account, a disabled administrator, a non-admin and a
// directory principal each refuses, naming why, and changes nothing: no
// record, no audit, no output.
func TestResetAdminRefusesTheAccount(t *testing.T) {
	f := installedFix(t)
	ctx := t.Context()
	cp := f.store().ControlPlane()
	for _, u := range []entity.User{
		{Principal: entity.NewUser(), Name: "off", Role: entity.RoleAdmin, Disabled: true},
		{Principal: entity.NewUser(), Name: "viewer", Role: entity.RoleViewer},
		{Principal: entity.NewUser(), Name: "dir", Role: entity.RoleAdmin, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeDir, Value: "entry-dir"}}},
	} {
		if _, err := cp.PutUser(ctx, u, 0, entity.Alloc{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, why string }{
		{"nobody", "no account"},
		{"off", "disabled"},
		{"viewer", "not admin"},
		{"dir", "directory principal"},
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
			if recs := f.audits(); len(recs) != 0 {
				t.Errorf("audit records after a refusal: %v, want none", recs)
			}
		})
	}
}

// A clear of the sources' counts that fails part way leaves the account as
// it was: its password, must-change, not-before and count, with no audit
// record and nothing printed; the error says the account is unchanged.
func TestResetAdminFailingClearChangesNoAccount(t *testing.T) {
	f := installedFix(t)
	before := f.admin()
	f.counts(before.Principal)
	fk := kvtest.NewFaultKV(f.db)
	schedule, err := sim.NewSchedule(1, f.clock, sim.Rule{Op: kvtest.OpScan, Key: "SF*", From: 2, Count: 1,
		Action: sim.FaultAction{Err: errors.New("store failed")}})
	if err != nil {
		t.Fatal(err)
	}
	fk.SetFault(schedule)
	d := f.resetDeps(install.AdminName)
	d.KV = fk
	err = install.ResetAdmin(t.Context(), d)
	if err == nil || !strings.Contains(err.Error(), "unchanged") || f.out.Len() != 0 {
		t.Fatalf("ResetAdmin with a failing clear: got %v, printed %q; want the account named unchanged, nothing printed", err, f.out.String())
	}
	if left := f.sourcesLeft(); left == 0 || left == sources {
		t.Fatalf("source counts left: %d of %d, want the clear stopped part way", left, sources)
	}
	after := f.admin()
	if after.Generation != before.Generation || after.MustChange != before.MustChange || !after.NotBefore.Equal(before.NotBefore) {
		t.Errorf("account after the failed clear: generation %d must-change %v not-before %v, want %d %v %v", after.Generation, after.MustChange, after.NotBefore, before.Generation, before.MustChange, before.NotBefore)
	}
	if _, ok, _ := f.store().ControlPlane().SignInFailures(t.Context(), entity.SignInKey{Account: before.Principal}); !ok {
		t.Errorf("the account's count after the failed clear: gone, want it kept")
	}
	if err := f.verify(before.Principal, "anything"); err == nil {
		t.Errorf("sign in after the failed clear: accepted, want the old password kept")
	}
	if recs := f.audits(); len(recs) != 0 {
		t.Errorf("audit records after the failed clear: %v, want none", recs)
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
	before := f.admin()
	if err := install.ResetAdmin(t.Context(), d); !errors.Is(err, install.ErrCrash) || f.out.Len() != 0 {
		t.Fatalf("ResetAdmin stopped after its write: got %v, printed %q; want the crash point, nothing printed", err, f.out.String())
	}
	if f.admin().Generation == before.Generation {
		t.Fatalf("account after the stopped reset: unchanged, want the write committed")
	}
	d = f.resetDeps(install.AdminName)
	d.Out = failingWriter{}
	if err := install.ResetAdmin(t.Context(), d); err == nil || !strings.Contains(err.Error(), "run dfs reset-admin again") {
		t.Errorf("ResetAdmin printing to a failing writer: got %v, want a rerun of dfs reset-admin named", err)
	}
}
