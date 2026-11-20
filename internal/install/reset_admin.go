package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// ErrNotResettable refuses a reset of an account that is missing, disabled
// or not an administrator.
var ErrNotResettable = errors.New("install: the account cannot be reset")

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
	// nil when the account gets none.
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

	// after runs once each write has committed, naming it; a test's hook,
	// nil in production.
	after func(write string) error
}

// ResetAdmin resets the administrator named d.Name: in one Write it sets a
// new generated password, its NT hash when the account gets one, prunes
// both secrets' old versions, marks the account must-change, raises its
// not-before past the write's store time, resets the account's sign-in
// count and records an audit record naming the host and the command. It
// then clears every source's sign-in count, in bounded batches, and only
// then prints the password to d.Out.
//
// decision: the break-glass reset bypasses the API, because the API is what
// is locked. It needs the host's bootstrap and the store's credential, and
// an attacker holding those already holds everything. Withdraw it if a
// deployment must prove that no change bypasses the API, and replace it with
// a second admin account kept sealed.
func ResetAdmin(ctx context.Context, d ResetDeps) error {
	switch {
	case d.KV == nil:
		return errors.New("install: ResetDeps.KV is nil")
	case d.StorageKey == nil:
		return errors.New("install: ResetDeps.StorageKey is nil")
	case d.Out == nil:
		return errors.New("install: ResetDeps.Out is nil, and the generated password must be printed")
	case d.Name == "":
		return fmt.Errorf("%w: no account named", ErrNotResettable)
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
	password, _, _ := firstPassword("", "") // with no reference it always generates
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
	pwRef := orNew(u.Password)
	pwOwner := userSecretOwner(u, "password")
	pw, err := p.Seal(ctx, secrets.Storage, entity.SecretPassword, pwOwner, pwRef, h.Password, secrets.Meta{})
	if err != nil {
		return fmt.Errorf("install: seal the account %q's password: %w", d.Name, err)
	}
	next.Password = pwRef
	var nt entity.Secret
	ntOwner := userSecretOwner(u, "nt_hash")
	if h.NTHash != nil {
		// decision: the role is named here, protocol for the NT hash, since
		// pkg/secrets exports no kind-to-role lookup; Seal refuses a role its
		// kind is not sealed under, so a drift fails closed. Overturned once
		// pkg/secrets exports its table.
		next.NTHash = orNew(u.NTHash)
		if nt, err = p.Seal(ctx, secrets.Protocol, entity.SecretNTHash, ntOwner, next.NTHash, h.NTHash, secrets.Meta{}); err != nil {
			return fmt.Errorf("install: seal the account %q's NT hash: %w", d.Name, err)
		}
	}

	req, _ := json.Marshal(struct { // strings always encode
		Command string `json:"command"`
		Name    string `json:"name"`
		Host    string `json:"host"`
	}{"dfs reset-admin", d.Name, d.Host})
	change, _ := json.Marshal(map[string]any{"password": "set", "must_change": true, "not_before": "raised"}) // fixed values always encode
	// The actor is the host, not an account: the principal stays zero and
	// the name and credential carry the host.
	audit := &entity.AuditRecord{
		ActorName:  d.Host,
		Credential: "host:" + d.Host,
		Node:       d.Node,
		Request:    req,
		Target:     entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])},
		Change:     change,
		Outcome:    []byte("applied"),
	}
	err = cp.Write(ctx, entity.WriteMeta{Audit: audit}, func(tx entity.ControlPlaneTx) error {
		cur, found, err := tx.User(u.Principal)
		if err != nil {
			return err
		}
		// The account is read again tracked: a change since the first read
		// refuses, as the record the hashes were derived for is not current.
		if err := resettable(d.Name, cur, found); err != nil {
			return err
		}
		if cur.Generation != u.Generation {
			return fmt.Errorf("install: the account %q changed during the reset; run dfs reset-admin again", d.Name)
		}
		if now, err := ntlmIn(tx); err != nil {
			return err
		} else if now != ntlm {
			return errors.New("install: the NTLM setting changed during the reset; run dfs reset-admin again")
		}
		if err := tx.PutSecret(pw, pw.Version-1); err != nil {
			return err
		}
		if err := p.Prune(ctx, tx, entity.SecretPassword, pwOwner, pwRef); err != nil {
			return err
		}
		if h.NTHash != nil {
			if err := tx.PutSecret(nt, nt.Version-1); err != nil {
				return err
			}
			if err := p.Prune(ctx, tx, entity.SecretNTHash, ntOwner, next.NTHash); err != nil {
				return err
			}
		}
		// Not-before is kept in whole seconds, so it is raised to the second
		// after the write's store time: every credential issued before the
		// reset is earlier than it, and one issued since is raised to it.
		next.NotBefore = later(cur.NotBefore, tx.Now().Truncate(time.Second).Add(time.Second))
		gen, err := tx.PutUser(next, cur.Generation, entity.Alloc{})
		if err != nil {
			return err
		}
		audit.Before, audit.After = cur.Generation, gen
		return auth.ResetAccount(tx, u.Principal)
	})
	if err != nil {
		return fmt.Errorf("install: reset the account %q: %w", d.Name, err)
	}
	log.Info("install: administrator reset", "account", d.Name, "host", d.Host)
	if err := d.done("account"); err != nil {
		return err
	}
	for more := true; more; {
		if more, err = cp.ClearSourceSignIns(ctx); err != nil {
			return fmt.Errorf("install: the account %q is reset but the sources' sign-in counts were not cleared, and its password was not printed; run dfs reset-admin again: %w", d.Name, err)
		}
	}
	if err := d.done("sources"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(d.Out, "The administrator %q's generated password, shown once; it must be changed at the first sign-in:\n%s\n", d.Name, password); err != nil {
		return fmt.Errorf("install: the account %q is reset but its generated password could not be printed; run dfs reset-admin again: %w", d.Name, err)
	}
	return nil
}

// resettable refuses an account that is missing, disabled or not an
// administrator, naming why.
//
// decision: the reset creates and re-enables nothing, as RFC 23 §5.4's
// "nothing else" says: it resets only an existing, enabled admin. The API's
// last_admin rule keeps one enabled admin, so an installation whose admin
// was deleted or renamed still has one to name with --name. Overturned if an
// installation can reach no enabled admin through the API.
func resettable(name string, u entity.User, found bool) error {
	switch {
	case !found || u.Deleted:
		return fmt.Errorf("%w: no account is named %q", ErrNotResettable, name)
	case u.Disabled:
		return fmt.Errorf("%w: the account %q is disabled", ErrNotResettable, name)
	case u.Role != entity.RoleAdmin:
		return fmt.Errorf("%w: the account %q has role %s, not admin", ErrNotResettable, name, u.Role)
	}
	return nil
}

// ntlmSetting reads identity.ntlm at installation scope, on when no row
// sets it, with the generation the reset's write is conditional on.
//
// ponytail: the row is decoded here, its default copied from the identity
// section, since the settings table's read is unbuilt. Upgrade to that read
// once it is built.
func ntlmSetting(ctx context.Context, cp kvstore.ControlPlane) (identity.NTLMSetting, error) {
	for s, err := range cp.Settings(ctx, entity.SettingScope{Kind: entity.ScopeInstallation}) {
		if err != nil {
			return identity.NTLMSetting{}, fmt.Errorf("install: read the settings: %w", err)
		}
		if s.Key == ntlmKey {
			return decodeNTLM(s)
		}
	}
	return identity.NTLMSetting{On: true}, nil
}

// ntlmIn is ntlmSetting read tracked inside tx.
func ntlmIn(tx entity.ControlPlaneTx) (identity.NTLMSetting, error) {
	s, ok, err := tx.Setting(entity.SettingScope{Kind: entity.ScopeInstallation}, ntlmKey)
	if err != nil || !ok {
		return identity.NTLMSetting{On: true}, err
	}
	return decodeNTLM(s)
}

func decodeNTLM(s entity.Setting) (identity.NTLMSetting, error) {
	var on bool
	if err := json.Unmarshal(s.Value, &on); err != nil {
		return identity.NTLMSetting{}, fmt.Errorf("install: the setting %s: %w", ntlmKey, err)
	}
	return identity.NTLMSetting{On: on, Generation: s.Generation}, nil
}

// userSecretOwner is the owner of u's secret under key, as the API names it.
func userSecretOwner(u entity.User, key string) entity.SecretOwner {
	return entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceUser, ID: bytes.Clone(u.Principal.ID[:])}, Key: key}
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

// done runs the test's hook, if one is set, after write committed.
func (d ResetDeps) done(write string) error {
	if d.after == nil {
		return nil
	}
	return d.after(write)
}
