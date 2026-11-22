package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/identity"
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

// ntlmKey is the installation-scope setting that turns NTLM sign-in on.
const ntlmKey = "identity.ntlm"

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
// failure up to there leaves the account as it was. Then one Write sets the
// new generated password, its NT hash when the account gets one or drops the
// one it had, prunes the secrets' old versions, marks the account
// must-change, raises its not-before past the write's store time, resets the
// account's sign-in count and records an audit record naming the host and
// the command. The password is printed to d.Out after that Write commits.
//
// decision: the break-glass reset bypasses the API, because the API is what
// is locked. It needs the host's bootstrap and the store's credential, and
// an attacker holding those already holds everything. Withdraw it if a
// deployment must prove that no change bypasses the API, and replace it with
// a second admin account kept sealed.
//
// decision: the account and the NTLM setting are read once, before the
// Write, and not again inside it. The store's exclusive open, Badger's
// directory lock, keeps a node and another reset out while this one runs,
// and the Write's expected generation refuses an account changed since the
// read. Overturned by a shared backend such as FoundationDB, where the API
// on another host can change the setting between the read and the Write.
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
	ntlm, err := ntlmSetting(ctx, cp)
	if err != nil {
		return err
	}
	password := generatePassword()
	defer clear(password)
	h, err := identity.NewHashes(u, password, ntlm)
	if err != nil {
		return fmt.Errorf("install: the account %q's password: %w", d.Name, err)
	}

	keys := map[secrets.Role]keyprovider.KeyProvider{secrets.Storage: d.StorageKey}
	if d.ProtocolKey != nil {
		keys[secrets.Protocol] = d.ProtocolKey
	}
	p, err := secrets.New(cp, keys)
	if err != nil {
		return err
	}
	next := u
	next.MustChange = true
	next.Password = orNew(u.Password)
	pw, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, entity.UserSecretOwner(u.Principal.ID, entity.UserPasswordKey), next.Password, h.Password, secrets.Meta{})
	if err != nil {
		return fmt.Errorf("install: seal the account %q's password: %w", d.Name, err)
	}
	// The NT hash is the new password's when NewHashes derives one, and
	// otherwise none: the one the account had is the old password's.
	ntValue := h.NTHash
	if ntValue == nil && u.NTHash != (entity.SecretRef{}) {
		// ponytail: the store has no whole-secret delete, so the old NT hash
		// is overwritten by a version of random bytes, its own versions
		// pruned below it, and the account names it no more; the orphaned
		// reference is dropped by the next NT-hash deletion. Upgrade to that
		// delete once the store offers one.
		ntValue = make([]byte, 16)
		_, _ = rand.Read(ntValue) // crypto/rand.Read never fails
	}
	ntRef := orNew(u.NTHash)
	var nt entity.Secret
	if ntValue != nil {
		if d.ProtocolKey == nil {
			return fmt.Errorf("install: the account %q: %w", d.Name, ErrNoProtocolKey)
		}
		// decision: the role is named here, protocol for the NT hash, since
		// pkg/secrets exports no kind-to-role lookup; Seal refuses a role its
		// kind is not sealed under, so a drift fails closed. Overturned once
		// pkg/secrets exports its table.
		if nt, err = p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, entity.UserSecretOwner(u.Principal.ID, entity.UserNTHashKey), ntRef, ntValue, secrets.Meta{}); err != nil {
			return fmt.Errorf("install: seal the account %q's NT hash: %w", d.Name, err)
		}
	}
	if h.NTHash != nil {
		next.NTHash = ntRef
	} else {
		next.NTHash = entity.SecretRef{}
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
		if err := tx.PutSecret(pw, pw.Version-1); err != nil {
			return err
		}
		if err := p.Prune(ctx, tx, pw.Kind, pw.Owner, pw.ID); err != nil {
			return err
		}
		if ntValue != nil {
			if err := tx.PutSecret(nt, nt.Version-1); err != nil {
				return err
			}
			if err := p.Prune(ctx, tx, nt.Kind, nt.Owner, nt.ID); err != nil {
				return err
			}
		}
		// Not-before is kept in whole seconds, so it is raised to the second
		// after the write's store time: every credential issued before the
		// reset is earlier than it, and one issued since is raised to it.
		n := next
		n.NotBefore = later(u.NotBefore, tx.Now().Truncate(time.Second).Add(time.Second))
		gen, err := tx.PutUser(n, u.Generation, entity.Alloc{})
		if err != nil {
			return err
		}
		change, err := resetChange(u, n, ntValue != nil)
		if err != nil {
			return err
		}
		audit.Before, audit.After, audit.Change = u.Generation, gen, change
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

// resetChange is the reset's audit change in the API's form: a secret field
// as "set", a removed reference as its old value and null, and every other
// field changed as its old and new values.
func resetChange(old, new entity.User, ntSealed bool) ([]byte, error) {
	c := map[string]any{"password": "set"}
	switch {
	case new.NTHash != (entity.SecretRef{}):
		c["nt_hash"] = "set"
	case ntSealed:
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

// ntlmSetting reads identity.ntlm at installation scope, on when no row
// sets it, as the identity section's default is.
//
// ponytail: the row is decoded here, its default copied from the identity
// section, since the settings table's read is unbuilt. Upgrade to that read
// once it is built.
func ntlmSetting(ctx context.Context, cp kvstore.ControlPlane) (identity.NTLMSetting, error) {
	for s, err := range cp.Settings(ctx, entity.SettingScope{Kind: entity.ScopeInstallation}) {
		if err != nil {
			return identity.NTLMSetting{}, fmt.Errorf("install: read the settings: %w", err)
		}
		if s.Key != ntlmKey {
			continue
		}
		var on bool
		if err := json.Unmarshal(s.Value, &on); err != nil {
			return identity.NTLMSetting{}, fmt.Errorf("install: the setting %s: %w", ntlmKey, err)
		}
		return identity.NTLMSetting{On: on, Generation: s.Generation}, nil
	}
	return identity.NTLMSetting{On: true}, nil
}

// orNew is ref, or a fresh reference when ref is zero.
func orNew(ref entity.SecretRef) entity.SecretRef {
	if ref == (entity.SecretRef{}) {
		return entity.SecretRef(uuid.New())
	}
	return ref
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
