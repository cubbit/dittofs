package kvstore

import (
	"context"
	"fmt"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/settings"
)

// Netgroups lists netgroups in name order, resuming after the name after.
func (c ControlPlane) Netgroups(ctx context.Context, after string) iter.Seq2[entity.Netgroup, error] {
	prefix := []byte{keys.KindNG}
	var cursor []byte
	if after != "" {
		cursor = netgroupKey(after)
	}
	return pages(ctx, c.s.kv, prefix, cursor, func(_ kv.Reader, row kv.KeyValue) (entity.Netgroup, error) {
		name, _, err := keys.ReadString(row.Key[len(prefix):])
		if err != nil {
			return entity.Netgroup{}, err
		}
		return decodeNetgroup(string(name), row.Value)
	})
}

// Netgroup is the netgroup of name; false when none is stored.
func (c ControlPlane) Netgroup(ctx context.Context, name string) (g entity.Netgroup, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		g, err = netgroupIn(r, name)
		return err
	})
	return g, err == nil && g.Version != 0, err
}

// PutNetgroup creates a netgroup, expect zero, or replaces the one whose
// version expect names, raising its version.
func (c ControlPlane) PutNetgroup(ctx context.Context, g entity.Netgroup, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutNetgroup(g, expect) })
}

func (c controlTx) PutNetgroup(g entity.Netgroup, expect uint64) (uint64, error) {
	if g.Name == "" {
		return 0, merrs.NewInvalidArgumentError("a netgroup needs a name")
	}
	old, err := netgroupIn(c.tx, g.Name)
	if err != nil {
		return 0, err
	}
	if err := expected(old.Version, expect); err != nil {
		return 0, err
	}
	g.Version = nextVersion(c.tx, old.Version)
	return g.Version, c.tx.Set(netgroupKey(g.Name), encodeNetgroup(g))
}

// netgroupIn reads a netgroup, the zero one when none is stored.
func netgroupIn(r kv.Reader, name string) (entity.Netgroup, error) {
	b, ok, err := get(r, netgroupKey(name))
	if err != nil || !ok {
		return entity.Netgroup{}, err
	}
	return decodeNetgroup(name, b)
}

// DeleteNetgroup removes the netgroup whose version expect names. No rule
// naming it is rewritten: such a rule then fails closed, an allow matching no
// host and a deny every host.
func (c ControlPlane) DeleteNetgroup(ctx context.Context, name string, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteNetgroup(name, expect) })
}

func (c controlTx) DeleteNetgroup(name string, expect uint64) error {
	old, err := netgroupIn(c.tx, name)
	if err != nil {
		return err
	}
	if err := expected(old.Version, expect); err != nil {
		return err
	}
	return c.tx.Delete(netgroupKey(name))
}

// Settings lists the settings at one scope, in key order.
func (c ControlPlane) Settings(ctx context.Context, scope entity.SettingScope) iter.Seq2[entity.Setting, error] {
	prefix := scopePrefix(scope)
	return pages(ctx, c.s.kv, prefix, nil, func(_ kv.Reader, row kv.KeyValue) (entity.Setting, error) {
		key, _, err := keys.ReadString(row.Key[len(prefix):])
		if err != nil {
			return entity.Setting{}, err
		}
		return decodeSetting(scope, string(key), row.Value)
	})
}

// raiseCfgGen raises the one settings generation every write of a setting or
// a secret moves in its own transaction, and every node polls to re-read
// settings, so each such write costs every node one reconcile walk.
//
// ponytail: one generation for every settings and secret write. This helper
// serves the setting writes: the secret write, the principal removal and the
// share removal raise the counter directly until the secrets work unifies
// them. Upgrade to a generation per section, or secrets off CFGGEN with
// their own counter, when the commit rate of the secret-write benchmark
// under this contention falls under half the rate of the same run without
// the raise.
func raiseCfgGen(tx kv.Txn) (uint64, error) {
	return raise(tx, cfgGenKey)
}

// PutSetting writes one setting, the generation expect names or zero for a
// new one, raising its generation and the settings generation every node
// polls, in one transaction. A share's setting is refused while the share is
// being removed, and once it is gone. A row of a schema above the binary's
// registered one is refused whole, its stored row and the settings generation
// unchanged, since a record this store wrote would be a record every node of
// this binary's version refuses whole (RFC 13 §2.3). The generation never
// repeats a value a removed row already held, or a writer that read the row
// before its removal matches the tag back and overwrites the new row.
func (c ControlPlane) PutSetting(ctx context.Context, s entity.Setting, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutSetting(s, expect) })
}

func (c controlTx) PutSetting(s entity.Setting, expect uint64) (uint64, error) {
	if s.Scope.Kind.String() == "" || (s.Scope.Kind == entity.ScopeInstallation) != (s.Scope.ID == [16]byte{}) || s.Key == "" {
		return 0, merrs.NewInvalidArgumentError("a setting needs a key and a scope, with an ID below installation scope")
	}

	if s.Schema > settings.Versions()[entity.FormatSettings].Max {
		return 0, fmt.Errorf("put setting %s: %w", s.Key, settings.ErrNewerSchema)
	}
	if s.Scope.Kind == entity.ScopeShare {
		if err := liveShare(c.tx, entity.ShareID(s.Scope.ID)); err != nil {
			return 0, err
		}
	}
	key := settingKey(s.Scope, s.Key)
	b, ok, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	var old entity.Setting
	if ok {
		if old, err = decodeSetting(s.Scope, s.Key, b); err != nil {
			return 0, err
		}
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}

	if s.Position, err = raiseCfgGen(c.tx); err != nil {
		return 0, err
	}

	// Position, the settings generation this write raises, never repeats, so
	// a row created again after a delete never hands out a generation a
	// writer before the delete already holds.
	s.Generation = max(old.Generation+1, s.Position)

	return s.Generation, c.tx.Set(key, encodeSetting(s))
}

// DeleteSetting removes the setting at scope whose generation expect names,
// so the wider scope's value is effective, raising the settings generation
// as a put does. A key or scope a put refuses is refused the same. A
// share's setting is refused while the share is being removed, and once it
// is gone, as its put is. Deleting one that is not there, expect zero,
// changes nothing: an absent row has no tag, so any other expect names a tag
// that is not there, and no write moved for any node to re-read.
func (c ControlPlane) DeleteSetting(
	ctx context.Context,
	scope entity.SettingScope,
	key string,
	expect uint64,
) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error {
		return tx.DeleteSetting(scope, key, expect)
	})
}

func (c controlTx) DeleteSetting(scope entity.SettingScope, key string, expect uint64) error {
	if scope.Kind.String() == "" || (scope.Kind == entity.ScopeInstallation) != (scope.ID == [16]byte{}) || key == "" {
		return merrs.NewInvalidArgumentError("a setting needs a key and a scope, with an ID below installation scope")
	}

	if scope.Kind == entity.ScopeShare {
		if err := liveShare(c.tx, entity.ShareID(scope.ID)); err != nil {
			return err
		}
	}

	k := settingKey(scope, key)

	b, ok, err := get(c.tx, k)
	if err != nil {
		return err
	}

	if !ok {
		return expected(0, expect)
	}

	old, err := decodeSetting(scope, key, b)
	if err != nil {
		return err
	}

	if err := expected(old.Generation, expect); err != nil {
		return err
	}

	if _, err := raiseCfgGen(c.tx); err != nil {
		return err
	}

	return c.tx.Delete(k)
}
