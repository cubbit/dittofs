// Package install is the initialisation step, dfs init's logic: it creates
// an installation's metadata store, initialises each journal directory,
// writes the node's identity beside each, records the journals, and commits
// the bootstrap administrator with the signing key.
package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// AdminName is the bootstrap administrator's account name.
const AdminName = "admin"

// generatedLen is how many random bytes a generated first password holds.
const generatedLen = 24

var (
	// ErrInstalled refuses a run over an installation already finished.
	ErrInstalled = errors.New("install: the installation is already initialised; to recover the administrator's password, run dfs reset-admin")
	// ErrForeignJournal refuses a journal directory of another installation.
	ErrForeignJournal = errors.New("install: journal directory belongs to another installation")
	// ErrPassword refuses a first-password reference that gives no password.
	ErrPassword = errors.New("install: the administrator's password reference gives no password")
)

// Deps is what one run of the initialisation step reads and writes.
type Deps struct {
	// Bootstrap names the journal directories, at least one, and, when set,
	// the node's identity.
	Bootstrap *bootstrap.Bootstrap
	// KV is the metadata store's KV; the caller opens and closes it.
	KV kv.KV
	// StorageKey is the storage role's wrapping key, which seals the
	// administrator's password hash and the signing key.
	StorageKey keyprovider.KeyProvider
	// Clock is the clock each journal is opened with.
	Clock journal.Clock
	// Journal are further options each journal is opened and initialised
	// with, after the clock; a test's mount probe.
	Journal []journal.Option
	// PasswordFile and PasswordEnv name where the administrator's first
	// password is read from, at most one of them; with neither, one is
	// generated and printed to Out.
	PasswordFile, PasswordEnv string
	// Out is where a generated password is printed, once, and nothing else;
	// it may be nil only when a reference gives the password.
	Out io.Writer
	// Logger records each step; nil logs nothing. It never sees a password.
	Logger *slog.Logger

	// after runs once each write has committed, naming it; a test's hook,
	// nil in production.
	after func(write string) error
}

// Run runs the initialisation step: the store's Init, with a minted
// installation ID or the one a store already holds; then for each journal
// directory its Init, the node's identity beside it and RecordJournal; then
// one Write holding the administrator, its password and the signing key. Run
// again after an interruption it skips each step already done, and refuses a
// finished installation, changing nothing. A generated password is printed
// to Out only once the last Write has committed.
//
// decision: two concurrent runs over one store are kept apart by the store's
// exclusive open, Badger's directory lock, which lets only one process hold
// the KV; within the process, Init and the last Write each refuse a second
// commit. Overturned by a shared backend such as FoundationDB, where two
// hosts' runs can interleave their journal steps over one store.
func Run(ctx context.Context, d Deps) error {
	if err := d.check(); err != nil {
		return err
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// The password is read first, so a reference that gives none refuses
	// before anything is written.
	password, generated, err := firstPassword(d.PasswordFile, d.PasswordEnv)
	if err != nil {
		return err
	}
	defer clear(password)
	if generated && d.Out == nil {
		return errors.New("install: Deps.Out is nil, and a generated password must be printed")
	}
	opts := append([]journal.Option{journal.WithClock(d.Clock)}, d.Journal...)

	st, inst, err := initStore(ctx, d, opts, log)
	if err != nil {
		return err
	}
	node, err := initJournals(ctx, d, opts, st, inst, log)
	if err != nil {
		return err
	}
	if err := commitAdmin(ctx, d, st, inst, password, generated); err != nil {
		return err
	}
	log.Info("install: administrator and signing key written", "account", AdminName, "node", uuid.UUID(node))
	if err := d.done("admin"); err != nil {
		return err
	}
	if !generated {
		return nil
	}
	// decision: the password is printed after the Write that stores its hash
	// commits, so a print that fails, or a crash before it, leaves a finished
	// installation whose administrator nobody can sign in as; dfs reset-admin
	// is the way back. Printing first would show a password that a failed
	// commit never stored. Overturned if the step can hand the password over
	// inside the commit, which no output stream allows.
	if _, err := fmt.Fprintf(d.Out, "The administrator %q's generated password, shown once; it must be changed at the first sign-in:\n%s\n", AdminName, password); err != nil {
		return fmt.Errorf("install: the installation is initialised but the generated password could not be printed; reset it with dfs reset-admin: %w", err)
	}
	return nil
}

// check refuses Deps that miss what every run needs, before any write.
func (d Deps) check() error {
	switch {
	case d.Bootstrap == nil:
		return errors.New("install: Deps.Bootstrap is nil")
	case len(d.Bootstrap.Journals) == 0:
		// A run with no journal would finish an installation with no journal
		// and no node identity, which no rerun could complete.
		return errors.New("install: the bootstrap names no journal directory")
	case d.KV == nil:
		return errors.New("install: Deps.KV is nil")
	case d.StorageKey == nil:
		return errors.New("install: Deps.StorageKey is nil")
	case d.Clock == nil:
		return errors.New("install: Deps.Clock is nil")
	}
	return nil
}

// done runs the test's hook, if one is set, after write committed.
func (d Deps) done(write string) error {
	if d.after == nil {
		return nil
	}
	return d.after(write)
}

// initStore opens the store, first initialising it with a minted
// installation ID when it holds none, and returns it with the installation's
// ID, refusing a finished installation. A store that holds none is
// initialised only once every journal directory is found to hold no
// journal: any journal there belongs to another installation.
func initStore(ctx context.Context, d Deps, opts []journal.Option, log *slog.Logger) (*kvstore.Store, [16]byte, error) {
	st, err := kvstore.Open(ctx, d.KV)
	if errors.Is(err, kvstore.ErrNotInitialised) {
		for _, j := range d.Bootstrap.Journals {
			_, other, found, err := classify(ctx, j.Path, opts)
			if err != nil {
				return nil, [16]byte{}, err
			}
			if found {
				return nil, [16]byte{}, fmt.Errorf("%w: %s holds installation %s", ErrForeignJournal, j.Path, uuid.UUID(other))
			}
		}
		inst := uuid.New()
		switch err := kvstore.Init(ctx, d.KV, inst); {
		case err == nil:
			log.Info("install: metadata store initialised", "installation", inst)
			if err := d.done("store"); err != nil {
				return nil, [16]byte{}, err
			}
		case errors.Is(err, kvstore.ErrInitialised):
			// Another initialisation committed first: its ID is read below.
		default:
			return nil, [16]byte{}, fmt.Errorf("install: initialise the metadata store: %w", err)
		}
		st, err = kvstore.Open(ctx, d.KV)
	}
	if err != nil {
		return nil, [16]byte{}, fmt.Errorf("install: open the metadata store: %w", err)
	}
	in, err := st.Installation(ctx)
	if err != nil {
		return nil, [16]byte{}, fmt.Errorf("install: read the installation: %w", err)
	}
	// decision: an installation is finished when its signing key is set,
	// which the last Write commits with the administrator. Reading it as
	// "an account named admin exists" would let a rerun mint a second admin
	// after an operator renamed the first, or a first one after it was
	// deleted.
	if in.SigningKey != (entity.SecretRef{}) {
		return nil, [16]byte{}, ErrInstalled
	}
	log.Info("install: metadata store open", "installation", uuid.UUID(in.ID))
	return st, in.ID, nil
}

// initJournals initialises each journal directory not yet initialised,
// writes the node's identity beside it and records it, one directory after
// another, and returns the node's identity. A directory holding this
// installation's journal is not initialised again; one of another
// installation is refused by name, and nothing is taken from it.
func initJournals(ctx context.Context, d Deps, opts []journal.Option, st *kvstore.Store, inst [16]byte, log *slog.Logger) (entity.NodeID, error) {
	var node entity.NodeID
	for _, j := range d.Bootstrap.Journals {
		dir := j.Path
		jid, err := initJournal(ctx, d, opts, dir, inst, log)
		if err != nil {
			return entity.NodeID{}, err
		}
		// InitNodeID adopts the copy an earlier directory got, and refuses
		// copies that differ, so every directory names one node.
		if node, err = bootstrap.InitNodeID(ctx, d.Bootstrap, inst, dir); err != nil {
			return entity.NodeID{}, fmt.Errorf("install: the node's identity in %s: %w", dir, err)
		}
		if err := d.done("node-id " + dir); err != nil {
			return entity.NodeID{}, err
		}
		if err := st.RecordJournal(ctx, [16]byte(jid), node, entity.JournalDir(dir)); err != nil {
			return entity.NodeID{}, fmt.Errorf("install: record the journal in %s: %w", dir, err)
		}
		log.Info("install: journal recorded", "dir", dir, "node", uuid.UUID(node))
		if err := d.done("record " + dir); err != nil {
			return entity.NodeID{}, err
		}
	}
	return node, nil
}

// initJournal returns the identity of dir's journal, initialising it when
// dir holds none, and refuses one of another installation than inst.
func initJournal(ctx context.Context, d Deps, opts []journal.Option, dir string, inst [16]byte, log *slog.Logger) (journal.JournalID, error) {
	jid, other, found, err := classify(ctx, dir, opts)
	switch {
	case err != nil:
		return journal.JournalID{}, err
	case found && [16]byte(other) != inst:
		return journal.JournalID{}, fmt.Errorf("%w: %s holds installation %s", ErrForeignJournal, dir, uuid.UUID(other))
	case found:
		log.Info("install: journal already initialised", "dir", dir)
		return jid, nil
	}
	jid, err = journal.Init(ctx, dir, journal.InstallationID(inst), opts...)
	if err != nil {
		return journal.JournalID{}, fmt.Errorf("install: initialise the journal in %s: %w", dir, err)
	}
	log.Info("install: journal initialised", "dir", dir)
	return jid, d.done("journal " + dir)
}

// classify names the journal in dir, its identity and installation, found
// false when dir holds none. An open with no expected identity never opens:
// it names the journal it finds, or finds none.
func classify(ctx context.Context, dir string, opts []journal.Option) (journal.JournalID, journal.InstallationID, bool, error) {
	j, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, opts...)
	var idErr *journal.IdentityError
	switch {
	case err == nil:
		// Unreachable with a zero expect; closed so nothing is left open.
		_ = j.Close()
		return journal.JournalID{}, journal.InstallationID{}, false, fmt.Errorf("install: journal directory %s opened with no expected identity", dir)
	case errors.As(err, &idErr):
		return idErr.Found, idErr.Installation, true, nil
	case errors.Is(err, journal.ErrNoJournal):
		return journal.JournalID{}, journal.InstallationID{}, false, nil
	default:
		return journal.JournalID{}, journal.InstallationID{}, false, fmt.Errorf("install: journal directory %s: %w", dir, err)
	}
}

// commitAdmin seals the administrator's password hash and a fresh signing
// key, then commits both with the account in one Write, refused if another
// run finished the installation first.
func commitAdmin(ctx context.Context, d Deps, st *kvstore.Store, inst [16]byte, password []byte, generated bool) error {
	cp := st.ControlPlane()
	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: d.StorageKey})
	if err != nil {
		return err
	}
	s := sealer{p}
	// The account holds no uid, gid, SID or Kerberos name, so NewHashes
	// derives no NT hash whatever NTLM's setting.
	admin := entity.User{
		Principal:  entity.NewUser(),
		Name:       AdminName,
		Role:       entity.RoleAdmin,
		MustChange: generated,
		Password:   entity.SecretRef(uuid.New()),
	}
	h, err := identity.NewHashes(admin, password, identity.NTLMSetting{})
	if err != nil {
		return fmt.Errorf("install: the administrator's password: %w", err)
	}
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(admin.Principal.ID[:])}, Key: "password"}
	pw, err := s.Seal(ctx, entity.SecretPassword, owner, admin.Password, h.Password)
	if err != nil {
		return fmt.Errorf("install: seal the administrator's password: %w", err)
	}
	keyRef := entity.SecretRef(uuid.New())
	key, err := auth.SealSigningKey(ctx, s, inst, keyRef)
	if err != nil {
		return fmt.Errorf("install: seal the signing key: %w", err)
	}
	err = cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		if in.SigningKey != (entity.SecretRef{}) {
			return ErrInstalled
		}
		if _, err := tx.PutUser(admin, 0, entity.Alloc{}); err != nil {
			return err
		}
		if err := tx.PutSecret(pw, 0); err != nil {
			return err
		}
		if err := tx.PutSecret(key, 0); err != nil {
			return err
		}
		return tx.SetSigningKey(keyRef)
	})
	if err != nil && !errors.Is(err, ErrInstalled) {
		return fmt.Errorf("install: write the administrator: %w", err)
	}
	return err
}

// sealer seals under the storage role, the one the password hash and the
// signing key are sealed under.
//
// ponytail: the role is fixed to storage, as both secrets sealed here are
// storage-role kinds. A caller sealing a protocol-role kind, the reset's NT
// hash, needs a kind-to-role lookup exported by pkg/secrets, which would
// also remove this adapter and the two like it.
type sealer struct{ p *secrets.Provider }

func (s sealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error) {
	return s.p.Seal(ctx, secrets.Storage, kind, owner, ref, value, secrets.Meta{})
}

// firstPassword reads the administrator's first password from the file or
// the environment variable named, at most one of them, or generates one when
// neither is. A file is read whole, one trailing LF or CRLF dropped. A
// reference that gives no password, missing or empty, refuses, naming it,
// and never falls back to a generated one.
func firstPassword(file, env string) (password []byte, generated bool, err error) {
	switch {
	case file != "" && env != "":
		return nil, false, fmt.Errorf("%w: both a file and an environment variable are named; name one", ErrPassword)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, false, fmt.Errorf("%w: file %s: %w", ErrPassword, file, err)
		}
		if t, ok := bytes.CutSuffix(b, []byte("\r\n")); ok {
			b = t
		} else {
			b = bytes.TrimSuffix(b, []byte("\n"))
		}
		if len(b) == 0 {
			return nil, false, fmt.Errorf("%w: file %s is empty", ErrPassword, file)
		}
		if !utf8.Valid(b) {
			return nil, false, fmt.Errorf("%w: file %s is not UTF-8 text", ErrPassword, file)
		}
		return b, false, nil
	case env != "":
		v := os.Getenv(env)
		if v == "" {
			return nil, false, fmt.Errorf("%w: environment variable %s is unset or empty", ErrPassword, env)
		}
		if !utf8.Valid([]byte(v)) {
			return nil, false, fmt.Errorf("%w: environment variable %s is not UTF-8 text", ErrPassword, env)
		}
		return []byte(v), false, nil
	}
	raw := make([]byte, generatedLen)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails
	defer clear(raw)
	return []byte(base64.RawURLEncoding.EncodeToString(raw)), true, nil
}
