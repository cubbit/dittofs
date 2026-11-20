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
	ErrInstalled = errors.New("install: the installation is already initialised")
	// ErrForeignJournal refuses a journal directory of another installation.
	ErrForeignJournal = errors.New("install: journal directory belongs to another installation")
	// ErrPassword refuses a first-password reference that gives no password.
	ErrPassword = errors.New("install: the administrator's password reference gives no password")
)

// Deps is what one run of the initialisation step reads and writes.
type Deps struct {
	// Bootstrap names the journal directories and, when set, the node's
	// identity.
	Bootstrap *bootstrap.Bootstrap
	// KV is the metadata store's KV; the caller opens and closes it.
	KV kv.KV
	// StorageKey is the storage role's wrapping key, which seals the
	// administrator's password hash and the signing key.
	StorageKey keyprovider.KeyProvider
	// Journal are the options each journal is opened and initialised with,
	// a clock among them, which the journal's open requires.
	Journal []journal.Option
	// PasswordFile and PasswordEnv name where the administrator's first
	// password is read from, at most one of them; with neither, one is
	// generated and printed to Out.
	PasswordFile, PasswordEnv string
	// Out is where a generated password is printed, once, and nothing else.
	Out io.Writer
	// Logger records each step; nil logs nothing. It never sees a password.
	Logger *slog.Logger

	// after runs once each write has committed, naming it; a test's crash
	// point, nil in production.
	after func(write string) error
}

// Run runs the initialisation step: the store's Init, with a minted
// installation ID or the one a store already holds; then for each journal
// directory its Init, the node's identity beside it and RecordJournal; then
// one Write holding the administrator, its password and the signing key. Run
// again after an interruption it skips each step already done, and refuses a
// finished installation, changing nothing. A generated password is printed
// to Out only once the last Write has committed.
func Run(ctx context.Context, d Deps) error {
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

	inst, err := initStore(ctx, d, log)
	if err != nil {
		return err
	}
	st, err := kvstore.Open(ctx, d.KV)
	if err != nil {
		return fmt.Errorf("install: open the metadata store: %w", err)
	}
	node, err := initJournals(ctx, d, st, inst, log)
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
	if generated {
		_, err := fmt.Fprintf(d.Out, "The administrator %q's generated password, shown once; it must be changed at the first sign-in:\n%s\n", AdminName, password)
		return err
	}
	return nil
}

// done runs the test's crash point, if one is set, after write committed.
func (d Deps) done(write string) error {
	if d.after == nil {
		return nil
	}
	return d.after(write)
}

// initStore initialises the store with a minted installation ID, or reads
// the ID of a store that holds one, refusing a finished installation.
func initStore(ctx context.Context, d Deps, log *slog.Logger) ([16]byte, error) {
	st, err := kvstore.Open(ctx, d.KV)
	if errors.Is(err, kvstore.ErrNotInitialised) {
		inst := uuid.New()
		if err := kvstore.Init(ctx, d.KV, inst); err != nil {
			return [16]byte{}, fmt.Errorf("install: initialise the metadata store: %w", err)
		}
		log.Info("install: metadata store initialised", "installation", inst)
		return inst, d.done("store")
	}
	if err != nil {
		return [16]byte{}, fmt.Errorf("install: open the metadata store: %w", err)
	}
	in, err := st.Installation(ctx)
	if err != nil {
		return [16]byte{}, fmt.Errorf("install: read the installation: %w", err)
	}
	// decision: an installation is finished when its signing key is set,
	// which the last Write commits with the administrator. Reading it as
	// "an account named admin exists" would let a rerun mint a second admin
	// after an operator renamed the first, or a first one after it was
	// deleted.
	if in.SigningKey != (entity.SecretRef{}) {
		return [16]byte{}, ErrInstalled
	}
	log.Info("install: metadata store already initialised", "installation", uuid.UUID(in.ID))
	return in.ID, nil
}

// initJournals initialises each journal directory not yet initialised,
// writes the node's identity beside it and records it, and returns the
// node's identity. A directory holding this installation's journal is not
// initialised again; one of another installation is refused by name, and
// nothing is taken from it.
func initJournals(ctx context.Context, d Deps, st *kvstore.Store, inst [16]byte, log *slog.Logger) (entity.NodeID, error) {
	var node entity.NodeID
	for _, j := range d.Bootstrap.Journals {
		dir := j.Path
		jid, err := initJournal(ctx, d, dir, inst, log)
		if err != nil {
			return entity.NodeID{}, err
		}
		id, err := bootstrap.InitNodeID(ctx, d.Bootstrap, inst, dir)
		if err != nil {
			return entity.NodeID{}, fmt.Errorf("install: the node's identity in %s: %w", dir, err)
		}
		if node != (entity.NodeID{}) && id != node {
			return entity.NodeID{}, fmt.Errorf("install: journal directory %s names node %s, another holds %s", dir, uuid.UUID(id), uuid.UUID(node))
		}
		node = id
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
// dir holds none. An open with no expected identity never opens: it names
// the journal it finds, or finds none.
func initJournal(ctx context.Context, d Deps, dir string, inst [16]byte, log *slog.Logger) (journal.JournalID, error) {
	j, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, d.Journal...)
	var idErr *journal.IdentityError
	switch {
	case err == nil:
		// Unreachable with a zero expect; closed so nothing is left open.
		_ = j.Close()
		return journal.JournalID{}, fmt.Errorf("install: journal directory %s opened with no expected identity", dir)
	case errors.As(err, &idErr):
		if [16]byte(idErr.Installation) != inst {
			return journal.JournalID{}, fmt.Errorf("%w: %s holds installation %s", ErrForeignJournal, dir, uuid.UUID(idErr.Installation))
		}
		log.Info("install: journal already initialised", "dir", dir)
		return idErr.Found, nil
	case errors.Is(err, journal.ErrNoJournal):
		jid, err := journal.Init(ctx, dir, journal.InstallationID(inst), d.Journal...)
		if err != nil {
			return journal.JournalID{}, fmt.Errorf("install: initialise the journal in %s: %w", dir, err)
		}
		log.Info("install: journal initialised", "dir", dir)
		return jid, d.done("journal " + dir)
	default:
		return journal.JournalID{}, fmt.Errorf("install: journal directory %s: %w", dir, err)
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
		return b, false, nil
	case env != "":
		v := os.Getenv(env)
		if v == "" {
			return nil, false, fmt.Errorf("%w: environment variable %s is unset or empty", ErrPassword, env)
		}
		return []byte(v), false, nil
	}
	raw := make([]byte, generatedLen)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails
	defer clear(raw)
	return []byte(base64.RawURLEncoding.EncodeToString(raw)), true, nil
}
