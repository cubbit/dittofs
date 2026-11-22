package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// installationScope is the scope of every identity setting.
var installationScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// identityTable is identity's two sections, as the node's table holds them.
func identityTable() settings.Table {
	return settings.Table{identity.Section(), identity.DirectorySection()}
}

// putSetting writes key = value for role as the API's route does: the
// table's Check, then the put, in one Write.
func putSetting(cp kvstore.ControlPlane, role entity.APIRole, key, value string) error {
	_, err := putSettingWith(cp, role, key, value, func(int) error { return nil })
	return err
}

// putSettingWith is putSetting running between on each attempt the Check
// passes, before the put, and returning how many attempts the Write took.
func putSettingWith(cp kvstore.ControlPlane, role entity.APIRole, key, value string,
	between func(attempt int) error,
) (attempts int, err error) {
	s := entity.Setting{Scope: installationScope, Key: key, Value: []byte(value)}
	err = cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		attempts++
		if err := identityTable().Check(tx, role, settings.Put(s), nil); err != nil {
			return err
		}
		if err := between(attempts); err != nil {
			return err
		}
		cur, _, err := tx.Setting(s.Scope, s.Key)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(s, cur.Generation)
		return err
	})
	return attempts, err
}

// idRange is an identity.id_range value.
func idRange(lo, hi uint32) string { return fmt.Sprintf(`{"lo":%d,"hi":%d}`, lo, hi) }

// allocate creates n users, each drawing a uid and a RID from r, or n groups
// drawing a gid when group is set, each under a name of its own.
func allocate(t *testing.T, st *kvstore.Store, n int, group bool, r entity.IDRange) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		var err error
		if group {
			_, err = st.ControlPlane().PutGroup(ctx, entity.Group{Principal: entity.NewGroup(), Name: "g" + entity.NewPrincipalID().String()}, 0,
				entity.Alloc{GID: true, RID: true, Range: r})
		} else {
			_, err = st.ControlPlane().PutUser(ctx, entity.User{Principal: entity.NewUser(), Name: "u" + entity.NewPrincipalID().String()}, 0,
				entity.Alloc{UID: true, RID: true, Range: r})
		}
		if err != nil {
			t.Fatalf("create %d of %d drawing from %+v: %v", i+1, n, r, err)
		}
	}
}

// marks reads the uid and gid high-water marks through a Write.
func marks(t *testing.T, st *kvstore.Store) (uid, gid uint32) {
	t.Helper()
	err := st.ControlPlane().Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
		uid, gid, err = tx.IDHighWater()
		return err
	})
	if err != nil {
		t.Fatalf("IDHighWater: %v", err)
	}
	return uid, gid
}

// cfgGen is the settings generation every settings write raises.
func cfgGen(t *testing.T, st *kvstore.Store) uint64 {
	t.Helper()
	c, err := st.Nodes().Counters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.Settings
}

// A write of identity.id_range whose high end is at or below the uid
// counter's mark is refused ErrInvalid, as RFC 13 §6's line naming the key,
// and commits nothing; one above it commits; once that range is used up, a
// write of another identity key still commits, and the same range is refused.
func TestCheckRefusesDeadIDRange(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st := newStore(t)
	cp := st.ControlPlane()
	allocate(t, st, 7, false, entity.DefaultIDRange)
	if uid, gid := marks(t, st); uid != 1_000_006 || gid != 0 {
		t.Fatalf("marks after 7 uids = uid %d, gid %d; want 1000006, 0", uid, gid)
	}
	gen := cfgGen(t, st)

	refused := func(name, value string) {
		t.Helper()
		err := putSetting(cp, entity.RoleAdmin, "identity.id_range", value)
		if !errors.Is(err, settings.ErrInvalid) {
			t.Errorf("%s: put of identity.id_range %s = %v, want ErrInvalid", name, value, err)
		} else if line := "setting (installation): identity.id_range: high end "; !strings.HasPrefix(err.Error(), line) {
			t.Errorf("%s: refusal %q, want it to start %q", name, err, line)
		}
	}

	refused("below the mark", idRange(1000, 2000))
	if got := cfgGen(t, st); got != gen {
		t.Errorf("settings generation after a refused range = %d, want %d unchanged", got, gen)
	}
	if uid, _ := marks(t, st); uid != 1_000_006 {
		t.Errorf("uid mark after a refused range = %d, want 1000006 unchanged", uid)
	}
	refused("at the mark", idRange(1_000_000, 1_000_006))

	if err := putSetting(cp, entity.RoleAdmin, "identity.id_range", idRange(1_000_000, 1_000_007)); err != nil {
		t.Fatalf("put of identity.id_range one above the mark: %v, want committed", err)
	}
	allocate(t, st, 1, false, entity.IDRange{Lo: 1_000_000, Hi: 1_000_007})

	if err := putSetting(cp, entity.RoleAdmin, "identity.ntlm", "false"); err != nil {
		t.Errorf("put of identity.ntlm with the range used up: %v, want committed", err)
	}
	refused("used up", idRange(1_000_000, 1_000_007))
}

// IDHighWater's read of the counters is tracked: a uid drawn between it and
// the commit of a settings write retries that write.
func TestIDHighWaterReadTracked(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st := newStore(t)
	attempts := 0
	err := st.ControlPlane().Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		attempts++
		if _, _, err := tx.IDHighWater(); err != nil {
			return err
		}
		if attempts == 1 {
			allocate(t, st, 1, false, entity.DefaultIDRange)
		}
		_, err := tx.PutSetting(entity.Setting{Scope: installationScope, Key: "identity.ntlm", Value: []byte("false")}, 0)
		return err
	})
	if err != nil {
		t.Fatalf("settings write: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts of the settings write = %d, want 2: a uid drawn after IDHighWater must retry it", attempts)
	}
}

// The gid counter's mark refuses a range alone, the uid counter never drawn.
func TestCheckRefusesIDRangeBelowGIDMark(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st := newStore(t)
	allocate(t, st, 3, true, entity.DefaultIDRange)
	if uid, gid := marks(t, st); uid != 0 || gid != 1_000_002 {
		t.Fatalf("marks after 3 gids = uid %d, gid %d; want 0, 1000002", uid, gid)
	}
	err := putSetting(st.ControlPlane(), entity.RoleAdmin, "identity.id_range", idRange(1_000_000, 1_000_002))
	if !errors.Is(err, settings.ErrInvalid) {
		t.Errorf("put of identity.id_range at the gid mark = %v, want ErrInvalid", err)
	}
}

// Apply never checks the range against the marks: a State whose range lies
// below them, over a store holding them, is applied.
func TestApplyKeepsRangeBelowMarks(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st := newStore(t)
	allocate(t, st, 7, false, entity.DefaultIDRange)
	allocate(t, st, 7, true, entity.DefaultIDRange)
	s, err := identity.New(identity.Deps{Store: st, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	want := entity.IDRange{Lo: 1000, Hi: 2000}
	in := state(1)
	in.Settings.IDRange = want
	if err := s.Apply(in); err != nil {
		t.Fatalf("Apply of a range below the marks: %v, want applied", err)
	}
	if got := s.Applied().Settings.IDRange; got != want {
		t.Errorf("applied range = %+v, want %+v", got, want)
	}
}

// A value the section's Validate refuses reads as one line naming the
// section once, then the field.
func TestCheckValidateLineNamesField(t *testing.T) {
	prof.Watch(t, prof.Options{})
	st := newStore(t)
	err := putSetting(st.ControlPlane(), entity.RoleAdmin, "identity.id_range", idRange(6, 5))
	want := "setting (installation): identity: id_range: low end 6 above high end 5"
	if err == nil || err.Error() != want {
		t.Errorf("put of identity.id_range lo 6, hi 5 = %v, want %q", err, want)
	}
}
