package install_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fix is one host: a metadata store in Badger on disk and two journal
// directories, all under t.TempDir(), and the storage role's key.
type fix struct {
	t     *testing.T
	root  string
	clock *sim.TestClock
	key   keyprovider.KeyProvider
	b     *bootstrap.Bootstrap
	db    *badgerkv.Store
	out   bytes.Buffer
	log   bytes.Buffer
}

func newFix(t *testing.T) *fix {
	t.Helper()
	const passphrase = "install test passphrase"
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)
	root := t.TempDir()
	b, err := keyprovider.GenerateKeyFile(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "storage.key")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kp.Close() })
	f := &fix{t: t, root: root, clock: sim.NewTestClock(start), key: kp, b: &bootstrap.Bootstrap{}}
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f.b.Journals = append(f.b.Journals, bootstrap.Journal{Path: dir})
	}
	f.open()
	return f
}

// open opens Badger over the store's directory, closing the KV it had, as a
// process starting again after a crash would.
func (f *fix) open() {
	f.t.Helper()
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			f.t.Fatalf("close the KV: %v", err)
		}
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(f.root, "store"), NodeID: entity.NodeID{'h'}, Clock: f.clock.Now})
	if err != nil {
		f.t.Fatalf("open the KV: %v", err)
	}
	f.t.Cleanup(func() { _ = db.Close() })
	f.db = db
}

func (f *fix) opts() []journal.Option {
	return []journal.Option{journal.WithClock(f.clock), journaltest.SuitableMount()}
}

func (f *fix) deps() install.Deps {
	return install.Deps{
		Bootstrap:  f.b,
		KV:         f.db,
		StorageKey: f.key,
		Journal:    f.opts(),
		Out:        &f.out,
		Logger:     slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (f *fix) dir(i int) string { return f.b.Journals[i].Path }

func (f *fix) store() *kvstore.Store {
	f.t.Helper()
	st, err := kvstore.Open(f.t.Context(), f.db)
	if err != nil {
		f.t.Fatalf("open the store: %v", err)
	}
	return st
}

// users is every user the store lists by name.
func (f *fix) users() []entity.User {
	f.t.Helper()
	var us []entity.User
	for u, err := range f.store().ControlPlane().Users(f.t.Context(), "") {
		if err != nil {
			f.t.Fatalf("list users: %v", err)
		}
		us = append(us, u)
	}
	return us
}

// admin is the store's one user, which must be the administrator.
func (f *fix) admin() entity.User {
	f.t.Helper()
	us := f.users()
	if len(us) != 1 || us[0].Name != install.AdminName {
		f.t.Fatalf("users: got %d (%v), want the one account %q", len(us), us, install.AdminName)
	}
	return us[0]
}

// installed checks a finished installation: one installation ID in the
// store and in every journal's format file, one node identity in every
// node-id copy, each journal recorded for that node and opening with the
// recorded identity, as a node's start opens it, and the administrator.
func (f *fix) installed() {
	f.t.Helper()
	ctx := f.t.Context()
	st := f.store()
	in, err := st.Installation(ctx)
	if err != nil {
		f.t.Fatalf("read the installation: %v", err)
	}
	if in.SigningKey == (entity.SecretRef{}) {
		f.t.Errorf("installation's signing key: unset, want the one the last write set")
	}
	node, err := bootstrap.NodeID(f.b)
	if err != nil {
		f.t.Fatalf("the node's identity over every journal directory: %v", err)
	}
	for _, j := range f.b.Journals {
		jid, inst, err := journal.ReadFormat(ctx, j.Path)
		if err != nil {
			f.t.Fatalf("format of %s: %v", j.Path, err)
		}
		if [16]byte(inst) != in.ID {
			f.t.Errorf("format of %s: installation %x, want the store's %x", j.Path, inst, in.ID)
		}
		rec, err := st.Lifecycle().JournalID(ctx, node, entity.JournalDir(j.Path))
		if err != nil || rec != [16]byte(jid) {
			f.t.Errorf("journal recorded for %s: got %x, %v; want its format's %x", j.Path, rec, err, jid)
		}
		jn, err := journal.Open(ctx, j.Path, journal.Version{}, jid, nil, f.opts()...)
		if err != nil {
			f.t.Errorf("open %s with its recorded identity: %v", j.Path, err)
			continue
		}
		if err := jn.Close(); err != nil {
			f.t.Errorf("close %s: %v", j.Path, err)
		}
	}
	f.admin()
}

// verify signs password in against the administrator's stored hash, as the
// API's sign-in does.
func (f *fix) verify(p entity.Principal, password string) error {
	f.t.Helper()
	st := f.store()
	prov, err := secrets.New(st.ControlPlane(), map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: f.key})
	if err != nil {
		f.t.Fatal(err)
	}
	srv, err := identity.New(identity.Deps{Store: st, Clock: f.clock, Secrets: prov, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		f.t.Fatal(err)
	}
	return srv.VerifyPassword(f.t.Context(), p, []byte(password))
}

// Stopped after each write, two journals' Init, node-id copy and record
// among them, and run again, the step finishes one installation: one ID in
// the store and every format file, one node identity in every copy, each
// journal opening as the store records it. Stopped after the last write it
// is finished, so the rerun refuses, and the generated password, never
// printed, is lost with the stop.
func TestCrashAfterEachWriteThenRerun(t *testing.T) {
	var order []string
	f := newFix(t)
	if err := install.Run(t.Context(), install.CrashAfter(f.deps(), 0, &order)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	a, b := f.dir(0), f.dir(1)
	want := []string{"store", "journal " + a, "node-id " + a, "record " + a, "journal " + b, "node-id " + b, "record " + b, "admin"}
	if !slices.Equal(order, want) {
		t.Fatalf("writes: got %q, want %q", order, want)
	}

	for n := 1; n <= len(want); n++ {
		t.Run(want[n-1], func(t *testing.T) {
			f := newFix(t)
			if err := install.Run(t.Context(), install.CrashAfter(f.deps(), n, nil)); !errors.Is(err, install.ErrCrash) {
				t.Fatalf("Run stopped after write %d: got %v, want the crash point", n, err)
			}
			if f.out.Len() != 0 {
				t.Errorf("output of the stopped run: %q, want nothing printed", f.out.String())
			}
			f.open()
			err := install.Run(t.Context(), f.deps())
			switch {
			case n == len(want) && !errors.Is(err, install.ErrInstalled):
				t.Fatalf("rerun after the last write: got %v, want ErrInstalled", err)
			case n < len(want) && err != nil:
				t.Fatalf("rerun: %v", err)
			}
			f.installed()
		})
	}
}

// A leftover journal directory of another installation is refused by name:
// its journal is not recorded, and neither its node-id copy is adopted nor
// one written beside it.
func TestForeignJournalRefused(t *testing.T) {
	f := newFix(t)
	ctx := t.Context()
	foreign := f.dir(1)
	other := uuid.New()
	if _, err := journal.Init(ctx, foreign, journal.InstallationID(other), f.opts()...); err != nil {
		t.Fatal(err)
	}
	theirs, err := bootstrap.InitNodeID(ctx, &bootstrap.Bootstrap{Journals: []bootstrap.Journal{{Path: foreign}}}, other, foreign)
	if err != nil {
		t.Fatal(err)
	}
	copyBefore, err := os.ReadFile(filepath.Join(foreign, journal.NodeIDFileName))
	if err != nil {
		t.Fatal(err)
	}

	err = install.Run(ctx, f.deps())
	if !errors.Is(err, install.ErrForeignJournal) || !strings.Contains(err.Error(), foreign) {
		t.Fatalf("Run over %s: got %v, want ErrForeignJournal naming it", foreign, err)
	}
	ours, err := bootstrap.NodeID(&bootstrap.Bootstrap{Journals: f.b.Journals[:1]})
	if err != nil {
		t.Fatalf("the node's identity in %s: %v", f.dir(0), err)
	}
	if ours == theirs {
		t.Errorf("node identity in %s: %x, the foreign directory's, want one not adopted from it", f.dir(0), ours)
	}
	if got, err := os.ReadFile(filepath.Join(foreign, journal.NodeIDFileName)); err != nil || !bytes.Equal(got, copyBefore) {
		t.Errorf("node-id copy in %s: got %x, %v; want it as it was, %x", foreign, got, err, copyBefore)
	}
	if rec, err := f.store().Lifecycle().JournalID(ctx, ours, entity.JournalDir(foreign)); err != nil || rec != ([16]byte{}) {
		t.Errorf("journal recorded for %s: got %x, %v; want none", foreign, rec, err)
	}
	if us := f.users(); len(us) != 0 {
		t.Errorf("users after the refusal: %v, want none", us)
	}
}

// snapshot is every key and value in the KV and every file under the
// journal directories.
func (f *fix) snapshot() map[string]string {
	f.t.Helper()
	m := map[string]string{}
	err := f.db.View(f.t.Context(), func(r kv.Reader) error {
		for kvp, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			m["kv:"+string(kvp.Key)] = string(kvp.Value)
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, j := range f.b.Journals {
		err := filepath.WalkDir(j.Path, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			m["file:"+p] = string(b)
			return err
		})
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return m
}

// A run over a finished installation refuses and changes nothing.
func TestSecondRunRefused(t *testing.T) {
	f := newFix(t)
	if err := install.Run(t.Context(), f.deps()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := f.snapshot()
	f.out.Reset()
	if err := install.Run(t.Context(), f.deps()); !errors.Is(err, install.ErrInstalled) {
		t.Fatalf("second Run: got %v, want ErrInstalled", err)
	}
	if after := f.snapshot(); !mapsEqual(before, after) {
		t.Errorf("the refused run changed the store or a journal directory: %d entries before, %d after", len(before), len(after))
	}
	if f.out.Len() != 0 {
		t.Errorf("output of the refused run: %q, want nothing", f.out.String())
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The first password from a file is read whole, one trailing LF or CRLF
// dropped, and from a variable as it is set; neither marks the account
// must-change or prints anything. An empty or missing file, or an unset or
// empty variable, refuses the step, naming the reference, before anything
// is written.
func TestPasswordReference(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    *string // the file's content; nil names a missing file
		env     *string // the variable's value; nil leaves it unset
		useFile bool
		useEnv  bool
		want    string // the password, empty for a refusal
	}{
		{name: "file with LF", file: ptr("s3cret pass\n"), useFile: true, want: "s3cret pass"},
		{name: "file with CRLF", file: ptr("s3cret pass\r\n"), useFile: true, want: "s3cret pass"},
		{name: "file with two LFs keeps one", file: ptr("s3cret\n\n"), useFile: true, want: "s3cret\n"},
		{name: "file with a lone CR keeps it", file: ptr("s3cret\r"), useFile: true, want: "s3cret\r"},
		{name: "file empty", file: ptr(""), useFile: true},
		{name: "file holding only LF", file: ptr("\n"), useFile: true},
		{name: "file missing", useFile: true},
		{name: "variable", env: ptr("from the env"), useEnv: true, want: "from the env"},
		{name: "variable unset", useEnv: true},
		{name: "variable empty", env: ptr(""), useEnv: true},
		{name: "both named", file: ptr("a\n"), env: ptr("b"), useFile: true, useEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFix(t)
			d := f.deps()
			var ref string
			if tc.useFile {
				d.PasswordFile = filepath.Join(t.TempDir(), "admin-password")
				ref = d.PasswordFile
				if tc.file != nil {
					if err := os.WriteFile(d.PasswordFile, []byte(*tc.file), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.useEnv {
				d.PasswordEnv = "DITTOFS_INSTALL_TEST_ADMIN_PASSWORD"
				ref = d.PasswordEnv
				if tc.env != nil {
					t.Setenv(d.PasswordEnv, *tc.env)
				} else {
					t.Setenv(d.PasswordEnv, "")
					if err := os.Unsetenv(d.PasswordEnv); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.useFile && tc.useEnv {
				ref = "both"
			}
			err := install.Run(t.Context(), d)
			if tc.want == "" {
				if !errors.Is(err, install.ErrPassword) || !strings.Contains(err.Error(), ref) {
					t.Fatalf("Run: got %v, want ErrPassword naming %s", err, ref)
				}
				if _, err := kvstore.Open(t.Context(), f.db); !errors.Is(err, kvstore.ErrNotInitialised) {
					t.Errorf("store after the refusal: open got %v, want it not initialised", err)
				}
				if f.out.Len() != 0 {
					t.Errorf("output of the refused run: %q, want nothing", f.out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			admin := f.admin()
			if admin.MustChange {
				t.Errorf("admin's must-change: set, want it clear for a given password")
			}
			if f.out.Len() != 0 {
				t.Errorf("output: %q, want nothing printed for a given password", f.out.String())
			}
			if err := f.verify(admin.Principal, tc.want); err != nil {
				t.Errorf("sign-in with %q: %v, want it accepted", tc.want, err)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// With no reference the password is 24 random bytes, encoded, printed once
// to the step's writer and nowhere in its log; it signs in, and marks the
// account must-change.
func TestGeneratedPassword(t *testing.T) {
	f := newFix(t)
	if err := install.Run(t.Context(), f.deps()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := strings.TrimSuffix(f.out.String(), "\n")
	pw := out[strings.LastIndexByte(out, '\n')+1:]
	raw, err := base64.RawURLEncoding.DecodeString(pw)
	if err != nil || len(raw) != 24 {
		t.Fatalf("printed password %q: decodes to %d bytes, %v; want 24", pw, len(raw), err)
	}
	if n := strings.Count(f.out.String(), pw); n != 1 {
		t.Errorf("password printed %d times, want once", n)
	}
	if f.log.Len() == 0 || strings.Contains(f.log.String(), pw) {
		t.Errorf("log %q: want the steps logged and the password absent", f.log.String())
	}
	admin := f.admin()
	if !admin.MustChange {
		t.Errorf("admin's must-change: clear, want it set for a generated password")
	}
	if err := f.verify(admin.Principal, pw); err != nil {
		t.Errorf("sign-in with the printed password: %v, want it accepted", err)
	}
}

// The administrator is a bare principal with role admin: no uid, gid, SID or
// Kerberos name, no primary group, no group membership and no share grant.
func TestBareAdministrator(t *testing.T) {
	f := newFix(t)
	if err := install.Run(t.Context(), f.deps()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	admin := f.admin()
	if admin.Role != entity.RoleAdmin || admin.Disabled {
		t.Errorf("admin: role %v, disabled %t; want role admin, enabled", admin.Role, admin.Disabled)
	}
	if len(admin.IDs) != 0 || admin.Primary != (entity.Principal{}) || admin.NTHash != (entity.SecretRef{}) {
		t.Errorf("admin: spellings %v, primary %v, NT hash %x; want none of them", admin.IDs, admin.Primary, admin.NTHash)
	}
	cp := f.store().ControlPlane()
	ctx := context.Background()
	for g, err := range cp.Groups(ctx, "") {
		t.Errorf("group %v (%v): want none, so no membership", g, err)
	}
	for s, err := range cp.Shares(ctx) {
		t.Errorf("share %v (%v): want none, so no grant", s, err)
	}
}
