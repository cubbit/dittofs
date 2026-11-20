package settings_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// valid gives the test structs that embed it a Validate that passes.
type valid struct{}

func (valid) Validate() error {
	return nil
}

// The settings structs the check tests declare sections over, each under a
// key RFC 13 writes where one fits.
type (
	identitySettings struct {
		valid
		Mode string `json:"mode" class:"live"`
	}
	oidcSettings struct {
		valid
		Issuer   string `json:"issuer"   class:"live"`
		Audience string `json:"audience" class:"live"`
	}
	signInSettings struct {
		valid
		Enabled bool `json:"enabled" class:"live"`
	}
	portSettings struct {
		Port uint32 `json:"port" class:"restart"`
		Host string `json:"host" class:"restart"`
	}
	journalSettings struct {
		valid
		SpareTarget uint32 `json:"spare_target" class:"live"`
	}
	overrideSettings struct {
		valid
		MaxFiles uint32 `json:"max_files" class:"live"`
	}
	rehomeSettings struct {
		valid
		Rate uint32 `json:"rate" class:"live"`
	}
)

// Validate refuses a port past 65535, printing it, as RFC 13 §6's example
// does.
func (p portSettings) Validate() error {
	if p.Port > 65535 {
		return fmt.Errorf("port must be at most 65535, got %d", p.Port)
	}

	return nil
}

var (
	ctx          = context.Background()
	installation = entity.SettingScope{Kind: entity.ScopeInstallation}
	nodeA        = entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   [16]byte{0x3f, 0x2a, 15: 1},
	}
	shareA = entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   [16]byte{0x5a, 15: 1},
	}
)

// journalID is the journal a per-journal key names, in hex as the key holds it.
const journalID = "3f2a00000000000000000000000000a1"

// table is every section the check tests write to.
func table() settings.Table {
	return settings.Table{
		settings.Of("identity", settings.At(entity.ScopeInstallation), identitySettings{}),
		settings.Of(
			"identity.oidc",
			settings.At(entity.ScopeInstallation),
			oidcSettings{},
			settings.WriteRole(entity.RoleAdmin),
		),
		settings.Of(
			"api.password_sign_in",
			settings.At(entity.ScopeInstallation),
			signInSettings{},
			settings.WriteRole(entity.RoleAdmin),
		),
		settings.Of(
			"test.port",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			portSettings{Port: 80},
			settings.OverrideOnly("Port"),
		),
		settings.Of(
			"adapters.smb",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			adaptersettings.DefaultSMB(),
			settings.OverrideOnly("Port"),
		),
		settings.Of(
			"journal",
			settings.PerThing(entity.ScopeNode, "journal"),
			journalSettings{},
		),
		settings.Of(
			"test.override",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			overrideSettings{},
		),
		settings.Of(
			"rehome",
			settings.Override(entity.ScopeInstallation, entity.ScopeShare),
			rehomeSettings{},
		),
	}
}

// store is a control plane over its own Badger directory, and that Badger,
// which a test writes beneath the store.
type store struct {
	cp kvstore.ControlPlane
	db kv.KV
}

// open opens a fresh store; the caller has called prof.Watch first, so the
// Badger it opens is closed before the watch checks.
func open(t *testing.T) store {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	return openOver(t, db)
}

// openOver initialises and opens the store over db.
func openOver(t *testing.T, db kv.KV) store {
	t.Helper()

	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return store{
		cp: s.ControlPlane(),
		db: db,
	}
}

// check runs table's Check for caller on w inside one Write, as the API's
// route does, and returns its error.
func (s store) check(tb settings.Table, caller entity.APIRole, w settings.Write) error {
	return s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tb.Check(tx, caller, w, nil)
	})
}

// put stores one setting through the store's own write, with no check.
func (s store) put(t *testing.T, scope entity.SettingScope, key, value string) {
	t.Helper()

	err := s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		cur, _, err := tx.Setting(scope, key)
		if err != nil {
			return err
		}

		_, err = tx.PutSetting(setting(scope, key, value), cur.Generation)

		return err
	})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// raw writes one setting row straight to the Badger, as a newer binary
// would, with no check and no settings generation raised.
func (s store) raw(t *testing.T, scope entity.SettingScope, key string, value []byte) {
	t.Helper()

	err := s.db.Update(ctx, func(tx kv.Txn) error {
		return tx.Set(rowKey(scope, key), value)
	})
	if err != nil {
		t.Fatalf("raw row %s: %v", key, err)
	}
}

// rowKey is the store's key of a setting row: its kind, the scope's kind,
// the scope's ID below installation, then the key.
func rowKey(scope entity.SettingScope, key string) []byte {
	k := []byte{keys.KindCFG, byte(scope.Kind)}
	if scope.Kind != entity.ScopeInstallation {
		k = keys.ID(k, scope.ID[:])
	}

	return keys.String(k, []byte(key))
}

// rowValue is the store's encoding of a setting row's value, generation,
// schema and position.
func rowValue(value string, generation uint64, schema uint32) []byte {
	return codec.NewWriter(1).
		Bytes([]byte(value)).
		Uint64(generation).
		Uint32(schema).
		Uint64(generation).
		Encode()
}

// setting is a row of value at the current schema.
func setting(scope entity.SettingScope, key, value string) entity.Setting {
	return entity.Setting{
		Scope:  scope,
		Key:    key,
		Value:  []byte(value),
		Schema: settings.Versions()[entity.FormatSettings].Max,
	}
}

// refused fails the test unless err matches want and prints line whole.
func refused(t *testing.T, what string, err, want error, line string) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)

		return
	}

	if line != "" && err.Error() != line {
		t.Errorf("%s: line = %q, want %q", what, err.Error(), line)
	}
}

// passed fails the test on err.
func passed(t *testing.T, what string, err error) {
	t.Helper()

	if err != nil {
		t.Errorf("%s: err = %v, want nil", what, err)
	}
}

// RFC 13 §9's "§6 unknown field": one field of each section misspelled, by a
// letter, by case, or as the prefix itself, is refused naming the closest
// declared key, and the correct spelling passes beside it.
func TestCheckUnknownField(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	cases := []struct {
		scope   entity.SettingScope
		bad     string
		good    string
		value   string
		closest string
	}{
		{installation, "identity.mod", "identity.mode", `"x"`, "identity.mode"},
		{
			installation,
			"identity.oidc.Issuer",
			"identity.oidc.issuer",
			`"x"`,
			"identity.oidc.issuer",
		},
		{
			installation, "api.password_sign_in.enable", "api.password_sign_in.enabled", "true",
			"api.password_sign_in.enabled",
		},
		{installation, "test.port.prot", "test.port.port", "1", "test.port.port"},
		{installation, "test.port.Port", "test.port.port", "1", "test.port.port"},
		{installation, "adapters.smb.prot", "adapters.smb.port", "1", "adapters.smb.port"},
		{
			nodeA,
			"journal." + journalID + ".spare_targt",
			"journal." + journalID + ".spare_target",
			"1",
			"journal." + journalID + ".spare_target",
		},
		{
			installation, "test.override.max_file", "test.override.max_files", "1",
			"test.override.max_files",
		},
		{installation, "rehome.rat", "rehome.rate", "1", "rehome.rate"},
	}

	for _, c := range cases {
		err := s.check(tb, entity.RoleAdmin, settings.Put(setting(c.scope, c.bad, c.value)))
		line := fmt.Sprintf("setting (%s): %q: unknown key; did you mean %s?",
			scopeName(c.scope), c.bad, c.closest)
		refused(t, c.bad, err, settings.ErrUnknownKey, line)

		passed(
			t,
			c.good,
			s.check(tb, entity.RoleAdmin, settings.Put(setting(c.scope, c.good, c.value))),
		)
	}

	err := s.check(tb, entity.RoleAdmin, settings.Put(setting(installation, "test.port", "1")))
	refused(t, "the prefix as a key", err, settings.ErrUnknownKey,
		`setting (installation): "test.port": unknown key; did you mean test.port.port?`)
}

// scopeName is a scope as a refusal line prints it.
func scopeName(s entity.SettingScope) string {
	if s.Kind == entity.ScopeInstallation {
		return "installation"
	}

	return fmt.Sprintf("%s %x", s.Kind, s.ID)
}

// A key under no section's prefix is refused with no hint, a put and a
// delete alike; a key the section does not declare is refused for a put
// whatever the caller's role.
func TestCheckUnknownKeyNoSection(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	for _, w := range []settings.Write{
		settings.Put(setting(installation, "nosuch.key", "1")),
		settings.Delete(installation, "nosuch.key"),
		settings.Put(setting(installation, "identityx.mode", "1")),
	} {
		refused(t, w.Key, s.check(tb, entity.RoleAdmin, w), settings.ErrUnknownKey,
			fmt.Sprintf("setting (installation): %q: unknown key", w.Key))
	}

	for _, role := range []entity.APIRole{entity.RoleOperator, entity.RoleAdmin} {
		err := s.check(tb, role, settings.Put(setting(installation, "identity.newer", "1")))
		refused(t, "undeclared put by "+role.String(), err, settings.ErrUnknownKey, "")
	}
}

// A hand-built Write, made by neither Put nor Delete, is refused naming its
// key, and reads nothing.
func TestCheckHandBuiltWrite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	w := settings.Write{
		Scope: installation,
		Key:   "identity.mode",
	}

	err := s.check(table(), entity.RoleAdmin, w)
	if !errors.Is(err, settings.ErrInvalid) || !strings.Contains(err.Error(), `"identity.mode"`) {
		t.Errorf("hand-built write: err = %v, want ErrInvalid naming the key", err)
	}
}

// A key holding a line break is printed escaped, so it cannot forge a line
// of an API answer or a log.
func TestCheckKeyEscaped(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	key := "identity.mode\nsetting (installation): identity.mode: forged"
	err := s.check(table(), entity.RoleAdmin, settings.Put(setting(installation, key, `"x"`)))

	refused(t, "key with a line break", err, settings.ErrUnknownKey, "")

	if err != nil && strings.ContainsAny(err.Error(), "\r\n") {
		t.Errorf("refusal holds a raw line break: %q", err.Error())
	}

	if err != nil && !strings.Contains(err.Error(), strings.ReplaceAll(key, "\n", `\n`)) {
		t.Errorf("refusal %q does not hold the key escaped", err.Error())
	}
}

// A write at a scope kind its section does not have is refused, and the
// scope it has passes: an installation setting, a per-journal setting, both
// sides of an installation default with a node override, and both sides of
// an installation default with a share override.
func TestCheckScopes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	journalKey := "journal." + journalID + ".spare_target"

	err := s.check(tb, entity.RoleAdmin,
		settings.Put(setting(nodeA, "api.password_sign_in.enabled", "true")))
	refused(t, "installation key per node", err, settings.ErrWrongScope,
		"setting ("+scopeName(nodeA)+"): api.password_sign_in.enabled: "+
			"set at installation, not per node")
	passed(t, "installation key at installation", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "api.password_sign_in.enabled", "true"))))

	passed(t, "journal key per node", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, journalKey, "1"))))
	refused(t, "journal key at installation", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, journalKey, "1"))), settings.ErrWrongScope,
		"setting (installation): "+journalKey+": set per node, not at installation")

	refused(t, "journal key with no journal", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "journal.spare_target", "1"))), settings.ErrUnknownKey,
		"setting ("+scopeName(nodeA)+`): "journal.spare_target": `+
			"unknown key; did you mean journal.<journal>.spare_target?")
	refused(t, "journal key with no field", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "journal."+journalID+".nosuch", "1"))),
		settings.ErrUnknownKey, "")

	for _, scope := range []entity.SettingScope{installation, nodeA} {
		passed(t, "test.override at "+scope.Kind.String(), s.check(tb, entity.RoleOperator,
			settings.Put(setting(scope, "test.override.max_files", "1"))))
	}

	refused(t, "test.override per share", s.check(tb, entity.RoleOperator,
		settings.Put(setting(shareA, "test.override.max_files", "1"))), settings.ErrWrongScope,
		"setting ("+scopeName(shareA)+"): test.override.max_files: "+
			"set at installation or per node, not per share")

	for _, scope := range []entity.SettingScope{installation, shareA} {
		passed(t, "rehome.rate at "+scope.Kind.String(), s.check(tb, entity.RoleOperator,
			settings.Put(setting(scope, "rehome.rate", "1"))))
	}

	refused(t, "rehome.rate per node", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "rehome.rate", "1"))), settings.ErrWrongScope, "")
}

// A node may override only the fields OverrideOnly names; the installation
// writes every field.
func TestCheckOverrideOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	passed(t, "node port", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.port", "1"))))
	refused(t, "node host", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.host", `"h"`))), settings.ErrWrongScope,
		"setting ("+scopeName(nodeA)+"): test.port.host: "+
			"set at installation, not per node; per node sets only test.port.port")

	for key, value := range map[string]string{"test.port.port": "1", "test.port.host": `"h"`} {
		passed(t, "installation "+key, s.check(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, value))))
	}

	passed(t, "node SMB port", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "adapters.smb.port", "1"))))
	refused(t, "node SMB pending timeout", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "adapters.smb.pending_timeout", `"1s"`))),
		settings.ErrWrongScope, "")
}

// A delete at a scope its key does not have is refused by the scope gate,
// before the role gate.
func TestCheckDeleteWrongScope(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	err := s.check(table(), entity.RoleOperator,
		settings.Delete(nodeA, "api.password_sign_in.enabled"))
	refused(t, "node delete", err, settings.ErrWrongScope, "")
}

// An admin-only section refuses every role below admin, its first value
// included, by an explicit rank that reads neither the role's name nor its
// number.
func TestCheckRoleOnAdminSections(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()
	w := settings.Put(setting(installation, "identity.oidc.issuer", `"https://idp"`))

	refused(t, "operator's first put", s.check(tb, entity.RoleOperator, w), settings.ErrForbidden,
		`setting (installation): identity.oidc.issuer: written by admin only`)

	s.put(t, installation, "identity.oidc.issuer", `"https://old"`)

	for _, role := range []entity.APIRole{
		entity.RoleOperator,
		entity.RoleViewer,
		entity.RoleNone,
		entity.APIRole(200),
		entity.RoleAdmin + 1,
	} {
		refused(t, fmt.Sprintf("role %d", role), s.check(tb, role, w), settings.ErrForbidden, "")
	}

	passed(t, "admin", s.check(tb, entity.RoleAdmin, w))
}

// A default section is written by an operator and an admin, never by a
// viewer or an empty role.
func TestCheckRoleOnDefaultSections(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()
	w := settings.Put(setting(installation, "identity.mode", `"x"`))

	for _, role := range []entity.APIRole{entity.RoleOperator, entity.RoleAdmin} {
		passed(t, role.String(), s.check(tb, role, w))
	}

	for _, role := range []entity.APIRole{entity.RoleViewer, entity.RoleNone} {
		refused(t, role.String(), s.check(tb, role, w), settings.ErrForbidden,
			`setting (installation): identity.mode: written by operator or admin`)
	}
}

// A delete passes the role gate as a put does: removing the installation's
// row of an admin-only setting is an admin's write.
func TestCheckRoleDelete(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()
	key := "api.password_sign_in.enabled"

	s.put(t, installation, key, "false")

	refused(t, "operator", s.check(tb, entity.RoleOperator, settings.Delete(installation, key)),
		settings.ErrForbidden, "")
	passed(t, "admin", s.check(tb, entity.RoleAdmin, settings.Delete(installation, key)))
}

// A key is judged by the section of the longest prefix on a dot boundary: an
// operator may write identity, never identity.oidc.
func TestCheckNoLeakAcrossSections(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	refused(t, "identity.oidc.audience", s.check(
		tb,
		entity.RoleOperator,
		settings.Put(
			setting(installation, "identity.oidc.audience", `"a"`),
		),
	), settings.ErrForbidden, "")
	passed(t, "identity.mode", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "identity.mode", `"a"`))))
}

// A key a newer binary stored under a declared prefix is deleted by an admin
// only, whatever the section's write role, and is never written by a put.
func TestCheckRepairDelete(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()
	key := "identity.newer"
	newer := settings.Versions()[entity.FormatSettings].Max + 1

	s.raw(t, installation, key, rowValue(`"x"`, 1, 1))

	passed(t, "admin", s.check(tb, entity.RoleAdmin, settings.Delete(installation, key)))

	for _, role := range []entity.APIRole{
		entity.RoleOperator,
		entity.RoleViewer,
		entity.RoleNone,
		entity.APIRole(200),
	} {
		refused(
			t,
			fmt.Sprintf("role %d", role),
			s.check(tb, role, settings.Delete(installation, key)),
			settings.ErrForbidden,
			`setting (installation): "identity.newer": unknown key, deleted by admin only`,
		)
	}

	s.raw(t, installation, key, rowValue(`"x"`, 2, newer))
	passed(t, "admin, newer row", s.check(tb, entity.RoleAdmin, settings.Delete(installation, key)))

	s.raw(t, nodeA, "test.port.newer", rowValue("1", 1, 1))
	passed(t, "admin, node row of an OverrideOnly section", s.check(tb, entity.RoleAdmin,
		settings.Delete(nodeA, "test.port.newer")))

	refused(
		t,
		"no prefix",
		s.check(tb, entity.RoleAdmin, settings.Delete(installation, "nosuch.key")),
		settings.ErrUnknownKey,
		"",
	)
	refused(
		t,
		"admin's put",
		s.check(tb, entity.RoleAdmin, settings.Put(setting(installation, key, `"x"`))),
		settings.ErrUnknownKey,
		"",
	)
}
