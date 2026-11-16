package kvstore

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
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

// PutSetting writes one setting, the generation expect names or zero for a
// new one, raising its generation and the settings generation every node
// polls, in one transaction. A share's setting is refused while the share is
// being removed.
func (c ControlPlane) PutSetting(ctx context.Context, s entity.Setting, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutSetting(s, expect) })
}

func (c controlTx) PutSetting(s entity.Setting, expect uint64) (uint64, error) {
	if s.Scope.Kind.String() == "" || (s.Scope.Kind == entity.ScopeInstallation) != (s.Scope.ID == [16]byte{}) || s.Key == "" {
		return 0, merrs.NewInvalidArgumentError("a setting needs a key and a scope, with an ID below installation scope")
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
	s.Generation = old.Generation + 1
	if s.Position, err = raise(c.tx, cfgGenKey); err != nil {
		return 0, err
	}
	return s.Generation, c.tx.Set(key, encodeSetting(s))
}
