package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/secrets"
)

var (
	// ErrNotResettable refuses a reset of an account that is missing,
	// disabled, not an administrator, or a directory principal.
	ErrNotResettable = errors.New("install: the account cannot be reset")
	// ErrNoProtocolKey refuses a reset that must seal an NT hash when the
	// bootstrap names no protocol key.
	ErrNoProtocolKey = errors.New("keys.protocol is not set: this account's NT hash is sealed under it; set it or turn NTLM off")
)

// ResetDeps is what one break-glass reset of an administrator reads and
// writes.
type ResetDeps struct {
	// KV is the metadata store's KV; the caller opens and closes it. Its
	// exclusive open, Badger's directory lock, is what refuses a reset while
	// a node holds the store.
	KV kv.KV
	// StorageKey seals the password hash; ProtocolKey the NT hash, and may be
	// nil when the account has and gets none.
	StorageKey, ProtocolKey keyprovider.KeyProvider
	// Name is the account to reset.
	Name string
	// Host and Node are what the audit record names as the actor: the host
	// the command ran on and the node identity of its bootstrap.
	Host string
	Node entity.NodeID
	// Out is where the generated password is printed, once, and nothing else.
	Out io.Writer
	// Logger records each step; nil logs nothing. It never sees a password.
	Logger *slog.Logger

	after hook
}

func (d ResetDeps) done(write string) error { return d.after.done(write) }

// ResetAdmin resets the administrator named d.Name. It refuses an account
// that is not resettable, seals the new secrets, and clears every source's
// sign-in count, in bounded batches, before it changes the account: a
// failure up to there leaves the account as it was. Then one Write makes
// auth's password write, the new generated password and its NT hash when
// the account gets one, the old versions pruned, must-change set and
// not-before raised, resets the account's sign-in count and records an audit
// record naming the host and the command. The password is printed to d.Out
// after that Write commits.
//
// decision: the break-glass reset bypasses the API, because the API is what
// is locked. It needs the host's bootstrap and the store's credential, and
// an attacker holding those already holds everything. Withdraw it if a
// deployment must prove that no change bypasses the API, and replace it with
// a second admin account kept sealed.
//
// decision: the reset relies on the store's exclusive open, Badger's
// directory lock, to keep a node and another reset out while it runs, so
// nothing retries: the password write's check of the account and of
// identity.ntlm, read before the seal and again in the Write, refuses a
// change between the two rather than waiting it out. Overturned by a shared
// backend such as FoundationDB, where the API on another host can change
// either in between, and a refused reset would want a retry.
func ResetAdmin(ctx context.Context, d ResetDeps) error {
	switch {
	case d.KV == nil:
		return errors.New("install: ResetDeps.KV is nil")
	case d.StorageKey == nil:
		return errors.New("install: ResetDeps.StorageKey is nil")
	case d.Out == nil:
		return errors.New("install: ResetDeps.Out is nil, and the generated password must be printed")
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	st, err := kvstore.Open(ctx, d.KV)
	if err != nil {
		return fmt.Errorf("install: open the metadata store: %w", err)
	}
	cp := st.ControlPlane()
	u, found, err := cp.UserNamed(ctx, d.Name)
	if err != nil {
		return fmt.Errorf("install: read the account %q: %w", d.Name, err)
	}
	if err := resettable(d.Name, u, found); err != nil {
		return err
	}
	ntlm, err := auth.ReadNTLM(ctx, cp)
	if err != nil {
		return fmt.Errorf("install: read %s: %w", auth.NTLMKey, err)
	}
	keys := map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: d.StorageKey}
	if d.ProtocolKey != nil {
		keys[secrets.Protocol] = d.ProtocolKey
	}
	p, err := secrets.New(cp, keys)
	if err != nil {
		return err
	}
	password := generatePassword()
	defer clear(password)
	ps, err := auth.SealPassword(ctx, resetSealer{p: p, protocol: d.ProtocolKey != nil}, u, password, ntlm)
	if err != nil {
		return fmt.Errorf("install: the account %q's password: %w", d.Name, err)
	}

	for more := true; more; {
		if more, err = cp.ClearSourceSignIns(ctx); err != nil {
			return fmt.Errorf("install: clear the sources' sign-in counts; the account %q is unchanged: %w", d.Name, err)
		}
	}
	if err := d.done("sources"); err != nil {
		return err
	}

	req, _ := json.Marshal(struct { // strings always encode
		Command string `json:"command"`
		Name    string `json:"name"`
		Host    string `json:"host"`
	}{"dfs reset-admin", d.Name, d.Host})
	audit := &entity.AuditRecord{
		ActorName:  d.Host,
		Credential: entity.HostCredential + d.Host,
		Node:       d.Node,
		Request:    req,
		Target:     entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])},
		Outcome:    []byte("applied"),
	}
	err = cp.Write(ctx, entity.WriteMeta{Audit: audit}, func(tx entity.ControlPlaneTx) error {
		before, after, err := auth.WritePassword(tx, ps, auth.WriteOpts{MustChange: true})
		if err != nil {
			return err
		}
		change, err := resetChange(before, after)
		if err != nil {
			return err
		}
		audit.Before, audit.After, audit.Change = before.Generation, after.Generation, change
		return auth.ResetAccount(tx, u.Principal)
	})
	if err != nil {
		return fmt.Errorf("install: reset the account %q: %w", d.Name, err)
	}
	log.Info("install: administrator reset", "account", d.Name, "host", d.Host)
	if err := d.done("account"); err != nil {
		return err
	}
	if err := printPassword(d.Out, d.Name, password); err != nil {
		return fmt.Errorf("install: the account %q is reset but its generated password could not be printed; run dfs reset-admin again: %w", d.Name, err)
	}
	return nil
}

// resetSealer is the secret provider as the reset seals a password with it:
// the password hash under the storage role and the NT hash under the
// protocol role, refused ErrNoProtocolKey when the bootstrap names no
// protocol key.
//
// decision: the role is named here, protocol for the NT hash, since
// pkg/secrets exports no kind-to-role lookup; Seal refuses a role its kind
// is not sealed under, so a drift fails closed. Overturned once pkg/secrets
// exports its table.
type resetSealer struct {
	p        *secrets.Provider
	protocol bool
}

func (s resetSealer) Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error) {
	role := secrets.Storage
	if kind == entity.SecretNTHash {
		if !s.protocol {
			return entity.Secret{}, ErrNoProtocolKey
		}
		role = secrets.Protocol
	}
	return s.p.Seal(ctx, role, kind, owner, ref, value, secrets.Meta{})
}

// resetChange is the reset's audit change in the API's form: a secret field
// as "set", a removed reference as its old value and null, and every other
// field changed as its old and new values.
func resetChange(old, new entity.User) ([]byte, error) {
	c := map[string]any{"password": "set"}
	switch {
	case new.NTHash != (entity.SecretRef{}):
		c["nt_hash"] = "set"
	case old.NTHash != (entity.SecretRef{}):
		c["nt_hash"] = [2]any{uuid.UUID(old.NTHash).String(), nil}
	}
	if old.MustChange != new.MustChange {
		c["must_change"] = [2]any{old.MustChange, new.MustChange}
	}
	c["not_before"] = [2]any{old.NotBefore, new.NotBefore}
	return json.Marshal(c)
}

// resettable refuses an account that is missing, disabled, not an
// administrator or a directory principal, naming why.
//
// decision: the reset creates and re-enables nothing, as RFC 23 §5.4's
// "nothing else" says: it resets only an existing, enabled admin with a
// local password. The API's last_admin rule keeps one enabled admin, so an
// installation whose admin was deleted or renamed still has one to name with
// --name. Overturned if an installation can reach no enabled admin through
// the API.
func resettable(name string, u entity.User, found bool) error {
	switch {
	case !found || u.Deleted:
		return fmt.Errorf("%w: no account is named %q", ErrNotResettable, name)
	case u.Disabled:
		return fmt.Errorf("%w: the account %q is disabled", ErrNotResettable, name)
	case u.Role != entity.RoleAdmin:
		return fmt.Errorf("%w: the account %q has role %s, not admin", ErrNotResettable, name, u.Role)
	case slices.ContainsFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == entity.SchemeDir }):
		return fmt.Errorf("%w: the account %q is a directory principal, with no local password", ErrNotResettable, name)
	}
	return nil
}
