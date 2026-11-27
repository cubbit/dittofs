package settings_test

import (
	"maps"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The content read the bound gate makes is tracked, and it reads the scope it
// is handed: a bound change at a node whose content a lower read judged passes
// while the content record it names exists, and a content record written
// between the gate's read and the commit makes the write retry, its second
// pass refusing the change (RFC 13 §6).
func TestBoundConflict(t *testing.T) {
	prof.Watch(t, prof.Options{})

	nodeB := entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   [16]byte{0x3f, 0x2a, 15: 2},
	}

	tb := settings.Table{
		settings.Of(
			"test.override",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			boundOverride{},
		),
	}

	key := "test.override.mode"

	s := open(t)
	s.netgroup(t, contentName(nodeA))

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(nodeB, key, `"b"`),
		holdsContent,
		nil,
	)
	passed(t, "a bound change at another node", err)

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(nodeA, key, `"a"`),
		holdsContent,
		nil,
	)
	refused(t, "a bound change at the node with content", err, settings.ErrBound, "")

	fresh := open(t)

	var calls int

	build := func(tx entity.ControlPlaneTx) settings.HoldsContent {
		return func(scope entity.SettingScope) (bool, error) {
			calls++

			_, found, err := tx.Netgroup(contentName(scope))
			if err != nil {
				return false, err
			}

			return found, nil
		}
	}

	_, err = fresh.attemptPut(
		tb,
		entity.RoleOperator,
		setting(nodeA, key, `"a"`),
		build,
		func() {
			fresh.netgroup(t, contentName(nodeA))
		},
	)
	refused(t, "the change beside content written with it", err, settings.ErrBound, "")

	if calls != 2 {
		t.Errorf("the content read ran %d times, want 2, once per attempt", calls)
	}

	if got := rowsStored(t, fresh, nodeA); len(got) != 0 {
		t.Errorf("the refused change stored rows: %v", got)
	}
}

// The row a store location's first value is decided on is read in the write's
// transaction: a raw row written beneath Check between the gate's read and the
// commit makes the write retry, and its second attempt meets the row and
// refuses the write, so "first value" is never decided on a stale read and the
// store keeps the concurrent row.
func TestStoreLocationConflict(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.remote",
			settings.At(entity.ScopeInstallation),
			remoteEndpoint{},
			settings.Required("Endpoint"),
		),
	}

	key := "test.remote.endpoint"

	build := func(entity.ControlPlaneTx) settings.HoldsContent {
		return func(entity.SettingScope) (bool, error) {
			return false, nil
		}
	}

	attempts, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"https://s3"`),
		build,
		func() {
			s.raw(t, installation, key, rowValue(`"https://other"`, 1, 1))
		},
	)
	refused(t, "the first put over a concurrent row", err, settings.ErrUnproven,
		`setting (installation): test.remote.endpoint: `+
			`store location change not provable yet, got "https://s3" (RFC 13 §5.2)`)

	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the row the gate read made the write retry)", attempts)
	}

	got := rowsStored(t, s, installation)
	if want := map[string]string{key: `"https://other"`}; !maps.Equal(got, want) {
		t.Errorf("rows = %v, want the concurrent row %v", got, want)
	}
}
