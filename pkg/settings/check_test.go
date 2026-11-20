package settings_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/bytesize"
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

// The sections the value tests declare, each in a table of its own.
type (
	refSettings struct {
		valid
		Lo         uint32           `json:"lo"         class:"live"`
		Credential entity.SecretRef `json:"credential" class:"live"`
	}
	tokenSettings struct {
		Token string `json:"token" class:"live"`
	}
	cacheSettings struct {
		valid
		Size bytesize.ByteSize `json:"size" class:"live"`
	}
)

// Validate prints the token it refuses, as a Validate breaking Of's contract
// would.
func (s tokenSettings) Validate() error {
	if s.Token != "" {
		return fmt.Errorf("bad token %q", s.Token)
	}

	return nil
}

// newerSchema is one past the newest settings schema this binary reads.
var newerSchema = settings.Versions()[entity.FormatSettings].Max + 1

// A put of a schema newer than this binary reads is refused whole, the
// current one passing beside it; a stored row of a newer schema in the
// section, at the installation or at the node scope, refuses a put and a
// delete the same, while a current one passes; the put's own schema is
// judged before the role, a stored row's after it; a schema 0 put passes.
func TestCheckNewerSchema(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	newerPut := setting(installation, "test.port.port", "1")
	newerPut.Schema = newerSchema

	refused(t, "newer put", s.check(tb, entity.RoleOperator, settings.Put(newerPut)),
		settings.ErrNewerSchema,
		"setting (installation): test.port.port: schema 2 is newer than this node reads, up to 1")
	refused(t, "viewer's newer put", s.check(tb, entity.RoleViewer, settings.Put(newerPut)),
		settings.ErrNewerSchema,
		"setting (installation): test.port.port: schema 2 is newer than this node reads, up to 1")
	passed(t, "current put", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "1"))))

	zero := setting(installation, "test.port.port", "1")
	zero.Schema = 0
	passed(t, "schema 0 put", s.check(tb, entity.RoleOperator, settings.Put(zero)))

	s.raw(t, installation, "test.port.host",
		rowValue(`"h"`, 1, settings.Versions()[entity.FormatSettings].Max))
	passed(t, "put beside a current row", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "1"))))

	s.raw(t, installation, "test.port.host", rowValue(`"h"`, 1, newerSchema))

	const line = "setting (installation): test.port.host: " +
		"schema 2 is newer than this node reads, up to 1"

	refused(t, "put beside a newer row", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "1"))), settings.ErrNewerSchema, line)
	refused(t, "node put beside a newer installation row", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.port", "1"))), settings.ErrNewerSchema, line)
	refused(t, "delete beside a newer row", s.check(tb, entity.RoleOperator,
		settings.Delete(installation, "test.port.port")), settings.ErrNewerSchema, line)
	refused(t, "viewer beside a newer row", s.check(tb, entity.RoleViewer,
		settings.Put(setting(installation, "test.port.port", "1"))), settings.ErrForbidden, "")

	n := open(t)
	n.raw(t, nodeA, "test.port.host", rowValue(`"h"`, 1, newerSchema))

	refused(t, "node put beside a newer node row", n.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.port", "1"))), settings.ErrNewerSchema,
		"setting ("+scopeName(nodeA)+"): test.port.host: "+
			"schema 2 is newer than this node reads, up to 1")
	passed(t, "installation put, the node row outside its chain", n.check(tb,
		entity.RoleOperator, settings.Put(setting(installation, "test.port.port", "1"))))
}

// A write may replace or delete its own row of a newer schema, since that
// reads none of the row's fields (RFC 13 §2.3); the same key's row at a
// wider scope of the chain, newer, still refuses it.
func TestCheckNewerSchemaOwnRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	s.raw(t, installation, "test.port.port", rowValue("1", 1, newerSchema))

	passed(t, "own row put", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "2"))))
	passed(t, "own row delete", s.check(tb, entity.RoleOperator,
		settings.Delete(installation, "test.port.port")))
	refused(t, "same key at the wider scope", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.port", "2"))), settings.ErrNewerSchema,
		"setting (installation): test.port.port: schema 2 is newer than this node reads, up to 1")

	s.raw(t, installation, "test.port.host", rowValue(`"h"`, 1, newerSchema))

	refused(t, "sibling", s.check(tb, entity.RoleOperator,
		settings.Delete(installation, "test.port.port")), settings.ErrNewerSchema,
		"setting (installation): test.port.host: schema 2 is newer than this node reads, up to 1")
}

// A required key left unset, by a put while none is stored or by the delete
// of its only row, is refused naming the key.
func TestCheckRequiredKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.oidc",
			settings.At(entity.ScopeInstallation),
			oidcSettings{},
			settings.Required("Issuer"),
		),
	}

	const line = "setting (installation): test.oidc.issuer: required and has no value"

	audience := settings.Put(setting(installation, "test.oidc.audience", `"a"`))

	refused(t, "put with no issuer", s.check(tb, entity.RoleOperator, audience),
		settings.ErrRequired, line)

	s.put(t, installation, "test.oidc.issuer", `"https://idp"`)

	passed(t, "put with the issuer stored", s.check(tb, entity.RoleOperator, audience))
	refused(t, "delete of the issuer", s.check(tb, entity.RoleOperator,
		settings.Delete(installation, "test.oidc.issuer")), settings.ErrRequired, line)
}

// A value the section's Validate refuses comes back as Validate words it,
// with the value it refused, matchable as ErrInvalid; the value below it
// passes, and Check stores nothing.
func TestCheckValidateRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	refused(t, "70000", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "70000"))), settings.ErrInvalid,
		"setting (installation): test.port: port must be at most 65535, got 70000")
	passed(t, "65535", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "65535"))))

	for row, err := range s.cp.Settings(ctx, installation) {
		t.Errorf("Check stored %s (err %v)", row.Key, err)
	}
}

// A refused write of a key no section declares never prints its value; a
// refused value of a declared field that holds no secret prints it.
func TestCheckMarkerNotEchoed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()

	const marker = "MARKER-7f3c"

	err := s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "identity.secret", `"`+marker+`"`)))
	refused(t, "undeclared key", err, settings.ErrUnknownKey, "")

	if err != nil && strings.Contains(err.Error(), marker) {
		t.Errorf("refusal of an undeclared key prints its value: %q", err.Error())
	}

	err = s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "70000")))
	if err == nil || !strings.Contains(err.Error(), "got 70000") {
		t.Errorf("refused port: err = %v, want the value printed", err)
	}
}

// A put that sets a reference field is refused, printing its key and never
// its value; another field of the section, and the reference's delete, pass.
func TestCheckReferenceRefused(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.ref",
			settings.At(entity.ScopeInstallation),
			refSettings{},
			settings.SecretRef("Credential", entity.SecretPassword),
		),
	}

	ref := "[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]"

	refused(t, "reference put", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.ref.credential", ref))), settings.ErrSecretRef,
		"setting (installation): test.ref.credential: "+
			"a reference is set by the secrets API, not by value")
	passed(t, "other field", s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.ref.lo", "1"))))
	passed(t, "reference delete", s.check(tb, entity.RoleOperator,
		settings.Delete(installation, "test.ref.credential")))
}

// A plaintext typed into a reference field never reaches a Validate that
// prints it, and the decode's refusal of one prints it neither.
func TestCheckReferenceBeforeValidate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.token",
			settings.At(entity.ScopeInstallation),
			tokenSettings{},
			settings.SecretRef("Token", entity.SecretPassword),
		),
		settings.Of(
			"test.ref",
			settings.At(entity.ScopeInstallation),
			refSettings{},
			settings.SecretRef("Credential", entity.SecretPassword),
		),
	}

	err := s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.token.token", `"MARKER-1"`)))
	refused(t, "string reference", err, settings.ErrSecretRef, "")

	if err != nil && strings.Contains(err.Error(), "MARKER-1") {
		t.Errorf("string reference refusal prints the plaintext: %q", err.Error())
	}

	err = s.check(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.ref.credential", `"MARKER-2"`)))
	refused(t, "typed reference", err, settings.ErrInvalid, "")

	if err != nil && strings.Contains(err.Error(), "MARKER-2") {
		t.Errorf("typed reference refusal prints the plaintext: %q", err.Error())
	}
}

// A refused value of the wrong type, and a refused size, are printed as
// written, with no decoder's own text in the line.
func TestCheckRefusedValuePrinted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	tb := append(table(), settings.Of(
		"test.cache",
		settings.At(entity.ScopeInstallation),
		cacheSettings{},
	))

	for _, c := range []struct {
		key, value, line string
	}{
		{
			"test.port.port", `"abc"`,
			`setting (installation): test.port.port: must be a number, got "abc"`,
		},
		{
			"test.cache.size", `"1.5GB"`,
			`setting (installation): test.cache.size: must be a whole number, got "1.5GB"`,
		},
	} {
		err := s.check(tb, entity.RoleOperator, settings.Put(setting(installation, c.key, c.value)))
		refused(t, c.key, err, settings.ErrInvalid, c.line)

		for _, text := range []string{"cannot unmarshal", "json:", "strconv"} {
			if err != nil && strings.Contains(err.Error(), text) {
				t.Errorf("%s: refusal holds decoder text %q: %q", c.key, text, err.Error())
			}
		}
	}
}

// A stored row Check cannot decode fails the write, never passes it, and
// its bytes are never printed; the error is the store's, not a refusal, so
// it wraps none of Check's ten refusals.
func TestCheckUndecodableRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	s.raw(t, installation, "test.port.host", []byte("\xffMARKER"))

	err := s.check(table(), entity.RoleOperator,
		settings.Put(setting(installation, "test.port.port", "1")))
	if err == nil {
		t.Fatal("put beside an undecodable row passed")
	}

	if strings.Contains(err.Error(), "MARKER") || strings.Contains(err.Error(), "\xff") {
		t.Errorf("error prints the row's bytes: %q", err.Error())
	}

	for _, refusal := range []error{
		settings.ErrUnknownKey,
		settings.ErrWrongScope,
		settings.ErrNewerSchema,
		settings.ErrForbidden,
		settings.ErrRequired,
		settings.ErrInvalid,
		settings.ErrBound,
		settings.ErrSecretRef,
		settings.ErrFormatVersion,
		settings.ErrUnproven,
	} {
		if errors.Is(err, refusal) {
			t.Errorf("a store error reads as a refusal: %v wraps %v", err, refusal)
		}
	}
}

// issuerSettings is an override section with a required key.
type issuerSettings struct {
	valid
	Issuer string `json:"issuer" class:"live"`
}

// A node row of a field OverrideOnly does not name changes no value, so a
// bad one stored around Check never refuses the node's write, as the node's
// read skips it.
func TestCheckOverrideOnlyRowSkipped(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	s.raw(t, nodeA, "test.port.host", rowValue("1", 1, 1))

	passed(t, "node port beside a skipped node host", s.check(table(), entity.RoleOperator,
		settings.Put(setting(nodeA, "test.port.port", "1"))))
}

// A delete drops only its own scope's row: the installation's row of the
// same key stays in the value the delete leaves.
func TestCheckDeleteKeepsWiderRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.issuer",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			issuerSettings{},
			settings.Required("Issuer"),
		),
	}

	s.put(t, installation, "test.issuer.issuer", `"https://wide"`)
	s.put(t, nodeA, "test.issuer.issuer", `"https://node"`)

	passed(t, "node override delete", s.check(tb, entity.RoleOperator,
		settings.Delete(nodeA, "test.issuer.issuer")))
}

// A per-journal refusal names the key as written, the journal's ID in it:
// a value the decode refuses, and a required key a delete leaves unset.
func TestCheckThingInLines(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	key := "journal." + journalID + ".spare_target"
	tb := settings.Table{
		settings.Of(
			"journal",
			settings.PerThing(entity.ScopeNode, "journal"),
			journalSettings{},
			settings.Required("SpareTarget"),
		),
	}

	passed(t, "put of the required key", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, key, "1"))))
	refused(
		t,
		"decode",
		s.check(tb, entity.RoleOperator, settings.Put(setting(nodeA, key, `"abc"`))),
		settings.ErrInvalid,
		"setting ("+scopeName(nodeA)+"): "+key+`: must be a number, got "abc"`,
	)
	refused(t, "required", s.check(tb, entity.RoleOperator, settings.Delete(nodeA, key)),
		settings.ErrRequired, "setting ("+scopeName(nodeA)+"): "+key+": required and has no value")
}
