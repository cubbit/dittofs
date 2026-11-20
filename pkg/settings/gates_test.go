package settings_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The settings structs the gates tests declare sections over: a bound key, a
// remote store's endpoint and buckets, a block format version.
type (
	boundKey struct {
		valid
		Target string `json:"target" class:"bound"`
	}
	boundLive struct {
		valid
		Bound string `json:"bound" class:"bound"`
		Live  string `json:"live"  class:"live"`
	}
	boundDuration struct {
		valid
		Retention time.Duration `json:"retention" class:"bound"`
	}
	boundOverride struct {
		valid
		Mode string `json:"mode" class:"bound"`
	}
	shape struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	boundShape struct {
		valid
		Shape shape `json:"shape" class:"bound"`
	}
	remoteEndpoint struct {
		valid
		Endpoint string `json:"endpoint" class:"restart" location:"store"`
	}
	remoteBucket struct {
		valid
		Bucket string `json:"bucket" class:"bound" location:"store"`
	}
	remoteBucketOnly struct {
		valid
		Bucket string `json:"bucket" class:"live" location:"store"`
	}
	remoteCredential struct {
		valid
		Credential entity.SecretRef `json:"credential" class:"restart" location:"store"`
	}
	blockFormat struct {
		valid
		Version uint32 `json:"version" class:"next write"`
		Other   uint32 `json:"other"   class:"live"`
	}
	blockFormatLong struct {
		valid
		Version int64 `json:"version" class:"next write"`
	}
	orderPort struct {
		Port uint32 `json:"port" class:"bound"`
	}
)

// Validate refuses a port past 65535, printing it, as RFC 13 §6's example
// does. It runs before the gates, so a write it refuses never reaches them.
func (o orderPort) Validate() error {
	if o.Port > 65535 {
		return fmt.Errorf("port must be at most 65535, got %d", o.Port)
	}

	return nil
}

// errHolds is the store error the failing holds builder returns: a read a
// failing store would make, which a refusal must never hide.
var errHolds = errors.New("content read: the store failed")

// contentName is the netgroup a test's holds closure point-reads, named by
// the scope whose content it stands for.
func contentName(scope entity.SettingScope) string {
	return hex.EncodeToString(scope.ID[:])
}

// holdsContent builds the closure Check's caller builds at its route: a point
// read of the netgroup named by the received scope, through the write's
// transaction.
func holdsContent(tx entity.ControlPlaneTx) settings.HoldsContent {
	return func(scope entity.SettingScope) (bool, error) {
		_, found, err := tx.Netgroup(contentName(scope))
		if err != nil {
			return false, err
		}

		return found, nil
	}
}

// failingHolds builds a holds whose read always fails, so any write that
// reaches it returns that error and no other.
func failingHolds(entity.ControlPlaneTx) settings.HoldsContent {
	return func(entity.SettingScope) (bool, error) {
		return false, errHolds
	}
}

// checkBuilt runs Check for caller on w inside one Write, holds built over
// the write's transaction.
func (s store) checkBuilt(
	tb settings.Table,
	caller entity.APIRole,
	w settings.Write,
	build func(entity.ControlPlaneTx) settings.HoldsContent,
) error {
	return s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		return tb.Check(tx, caller, w, build(tx))
	})
}

// attemptPut stores row as the API's route does: Check first, holds built
// over the write's transaction, then the put, and it runs hook between the
// two on the first attempt only. It returns how many attempts ran and the
// write's error.
func (s store) attemptPut(
	tb settings.Table,
	caller entity.APIRole,
	row entity.Setting,
	build func(entity.ControlPlaneTx) settings.HoldsContent,
	hook func(),
) (int, error) {
	attempts := 0

	err := s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		attempts++

		holds := build(tx)

		if err := tb.Check(tx, caller, settings.Put(row), holds); err != nil {
			return err
		}

		if attempts == 1 && hook != nil {
			hook()
		}

		cur, _, err := tx.Setting(row.Scope, row.Key)
		if err != nil {
			return err
		}

		_, err = tx.PutSetting(row, cur.Generation)

		return err
	})

	return attempts, err
}

// storedDelete removes the row as the API's route does: Check first, holds
// built over the write's transaction, then the delete, which runs only when
// Check passes.
func (s store) storedDelete(
	tb settings.Table,
	caller entity.APIRole,
	scope entity.SettingScope,
	key string,
	build func(entity.ControlPlaneTx) settings.HoldsContent,
) error {
	return s.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tb.Check(tx, caller, settings.Delete(scope, key), build(tx)); err != nil {
			return err
		}

		cur, _, err := tx.Setting(scope, key)
		if err != nil {
			return err
		}

		return tx.DeleteSetting(scope, key, cur.Generation)
	})
}

// rowsStored is every setting row stored at scope, its key to its value
// bytes, as the operator's write left them.
func rowsStored(t *testing.T, s store, scope entity.SettingScope) map[string]string {
	t.Helper()

	got := make(map[string]string)

	for row, err := range s.cp.Settings(ctx, scope) {
		if err != nil {
			t.Fatalf("read %s: %v", row.Key, err)
		}

		got[row.Key] = string(row.Value)
	}

	return got
}

// A change to a bound setting is refused while its scope holds content and
// stored while it holds none; the refused write leaves the row as it was, so
// an operator's edit cannot orphan what a namespace or store already holds
// (RFC 13 §5.1).
func TestBoundPutNeedsNoContent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	tb := settings.Table{
		settings.Of("test.block", settings.At(entity.ScopeInstallation), boundKey{Target: "d"}),
	}

	key := "test.block.target"

	s := open(t)
	s.netgroup(t, contentName(installation))

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"t2"`),
		holdsContent,
		nil,
	)
	refused(t, "a change under content", err, settings.ErrBound,
		`setting (installation): test.block.target: `+
			`bound setting changed while its scope holds content, got "t2" (RFC 13 §5.1)`)

	if got := rowsStored(t, s, installation); len(got) != 0 {
		t.Errorf("a refused change stored rows: %v", got)
	}

	none := open(t)

	_, err = none.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"t2"`),
		holdsContent,
		nil,
	)
	passed(t, "the same change with no content", err)

	if want := map[string]string{key: `"t2"`}; !maps.Equal(
		rowsStored(t, none, installation),
		want,
	) {
		t.Errorf("the accepted change: rows = %v, want %v", rowsStored(t, none, installation), want)
	}
}

// A delete is checked as the change it is: removing a bound key's row so the
// effective value changes is refused while content exists and leaves the row
// stored, and it passes while none exists, which is also how it works for a
// delete under no content whatever it would leave.
func TestBoundDeleteNeedsNoContent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.block", settings.At(entity.ScopeInstallation), boundKey{Target: "d"}),
	}

	key := "test.block.target"

	s.put(t, installation, key, `"v"`)
	passed(t, "the delete with no content",
		s.storedDelete(tb, entity.RoleOperator, installation, key, holdsContent))

	if got := rowsStored(t, s, installation); len(got) != 0 {
		t.Errorf("the accepted delete left rows: %v", got)
	}

	s.put(t, installation, key, `"v"`)
	s.netgroup(t, contentName(installation))

	refused(t, "the same delete under content",
		s.storedDelete(tb, entity.RoleOperator, installation, key, holdsContent),
		settings.ErrBound,
		`setting (installation): test.block.target: `+
			`bound setting changed while its scope holds content (RFC 13 §5.1)`)

	got := rowsStored(t, s, installation)
	if want := map[string]string{key: `"v"`}; !maps.Equal(got, want) {
		t.Errorf("the refused delete: rows = %v, want %v", got, want)
	}
}

// A put of the value already stored is no change: it passes while content
// exists without asking holds, which a failing read underneath would show, and
// a different value beside it is refused.
func TestBoundSameValueIsNoChange(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.block", settings.At(entity.ScopeInstallation), boundKey{Target: "d"}),
	}

	key := "test.block.target"

	s.put(t, installation, key, `"v"`)
	s.netgroup(t, contentName(installation))

	passed(t, "the put of the stored value",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, `"v"`)), failingHolds))

	refused(t, "another value beside it",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, `"other"`)), holdsContent),
		settings.ErrBound, "")
}

// Values are compared as the decode reads them, not by bytes, since a row
// keeps what the operator wrote: "60m" put over a stored "1h" with content is
// no change, and "2h" beside them is refused.
func TestBoundSameValueDecodedNotBytes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.gc", settings.At(entity.ScopeInstallation), boundDuration{}),
	}

	key := "test.gc.retention"

	s.put(t, installation, key, `"1h"`)
	s.netgroup(t, contentName(installation))

	passed(t, "an hour put over its stored hour",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, `"60m"`)), holdsContent))

	refused(t, "two hours beside it",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, `"2h"`)), holdsContent),
		settings.ErrBound, "")
}

// On a bound section an override declares, a delete is judged by the value it
// leaves, not by the row it removes: while content exists, deleting a node
// row equal to the installation's row passes, and deleting one the
// installation's row would replace is refused (RFC 13 §5.1).
func TestBoundOverrideDelete(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.override",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			boundOverride{},
		),
	}

	key := "test.override.mode"

	s.netgroup(t, contentName(nodeA))

	s.put(t, installation, key, `"a"`)
	s.put(t, nodeA, key, `"a"`)

	passed(t, "the delete that leaves the same value",
		s.storedDelete(tb, entity.RoleOperator, nodeA, key, holdsContent))

	if got := rowsStored(t, s, nodeA); len(got) != 0 {
		t.Errorf("the accepted delete left the node's row: %v", got)
	}

	s.put(t, nodeA, key, `"a"`)
	s.put(t, installation, key, `"b"`)

	refused(t, "the delete that leaves another value",
		s.storedDelete(tb, entity.RoleOperator, nodeA, key, holdsContent),
		settings.ErrBound, "")

	got := rowsStored(t, s, nodeA)
	if want := map[string]string{key: `"a"`}; !maps.Equal(got, want) {
		t.Errorf("the refused delete: rows = %v, want %v", got, want)
	}
}

// In an override section a node put with no node row is judged against the
// installation's row: the put of its value is no change and passes without
// asking holds, while another value is refused under content (RFC 13 §5.1).
func TestBoundOverridePutOverTheWiderRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.override",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			boundOverride{},
		),
	}

	key := "test.override.mode"

	s.netgroup(t, contentName(nodeA))
	s.put(t, installation, key, `"a"`)

	passed(t, "the put of the wider row's value",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `"a"`)), failingHolds))

	refused(t, "the put of another value",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `""`)), holdsContent),
		settings.ErrBound, "")
}

// A struct bound field in an override section is judged against the value
// the scope reads before the write: the wider row overlaid by the node's own,
// member by member, as the after side is built. A put that changes no member
// of that value passes without asking holds, and one that changes a member
// the wider row set is refused under content (RFC 13 §5.1).
func TestBoundStructOverrideOverlay(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.shape",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			boundShape{},
		),
	}

	key := "test.shape.shape"

	s.netgroup(t, contentName(nodeA))
	s.put(t, installation, key, `{"b":"1"}`)
	s.put(t, nodeA, key, `{"a":"1"}`)

	refused(t, "the put that changes the member the wider row set",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `{"a":"1","b":""}`)), holdsContent),
		settings.ErrBound, "")

	passed(t, "the put that changes no member",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `{"a":"1"}`)), failingHolds))
}

// A refusal prints the put as the decode's refusals do, a string quoted and
// any other value compact, so a put that spans lines is still one line
// (RFC 13 §6).
func TestGateRefusalPrintsThePutOnOneLine(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.shape", settings.At(entity.ScopeInstallation), boundShape{}),
	}

	s.netgroup(t, contentName(installation))

	refused(t, "the put spanning lines",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, "test.shape.shape",
				"{\n  \"a\": \"1\",\n  \"b\": \"2\"\n}\n")), holdsContent),
		settings.ErrBound,
		"setting (installation): test.shape.shape: "+
			`bound setting changed while its scope holds content, `+
			`got {"a":"1","b":"2"} (RFC 13 §5.1)`)
}

// The store location analogue of the wider row: a node put with no node row
// is judged against the installation's row, so it is refused rather than taken
// for the store's first value (RFC 13 §5.2).
func TestStoreLocationOverrideWiderRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.remote",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			remoteEndpoint{},
			settings.Required("Endpoint"),
		),
	}

	key := "test.remote.endpoint"

	s.put(t, installation, key, `"https://s3"`)

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(nodeA, key, `"https://e2"`),
		holdsContent,
		nil,
	)
	refused(t, "the node put over the wider row", err, settings.ErrUnproven,
		"setting ("+scopeName(nodeA)+"): "+key+": "+
			`store location change not provable yet, got "https://e2" (RFC 13 §5.2)`)
}

// A per thing bound key is judged against the thing's own row, read as a
// stored row and decoded with the key as the section declares it: the put of
// the value the row holds passes with content, another value is refused, and
// a row before the write nothing decodes refuses the write naming the key as
// written (RFC 13 §5.1).
func TestBoundPerThingKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.journal",
			settings.PerThing(entity.ScopeNode, "journal"),
			boundOverride{},
		),
	}

	key := "test.journal." + journalID + ".mode"

	s.netgroup(t, contentName(nodeA))
	s.put(t, nodeA, key, `"v"`)

	passed(t, "the put of the thing's stored value",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `"v"`)), holdsContent))

	refused(t, "the put of another value",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `""`)), holdsContent),
		settings.ErrBound, "")

	s.raw(t, nodeA, key, rowValue(`v`, 1,
		settings.Versions()[entity.FormatSettings].Max))
	refused(t, "a row before the write nothing decodes",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, key, `"v"`)), holdsContent),
		settings.ErrInvalid,
		"setting ("+scopeName(nodeA)+"): "+key+": must hold one JSON value")
}

// A bound key's own row of a newer settings schema is read by the gate before
// the write, and the write is refused whole rather than judged against a row
// this binary cannot read (RFC 13 §2.3).
func TestBoundOwnNewerRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.block", settings.At(entity.ScopeInstallation), boundKey{Target: "d"}),
	}

	key := "test.block.target"
	schema := settings.Versions()[entity.FormatSettings].Max + 1

	s.raw(t, installation, key, rowValue(`"v"`, 1, schema))

	refused(t, "a put over the own newer row",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, `"t2"`)), holdsContent),
		settings.ErrNewerSchema,
		fmt.Sprintf("setting (installation): %s: "+
			"schema %d is newer than this node reads, up to %d",
			key, schema, schema-1))
}

// An error the content read returns is the write's error, never counted or
// wrapped as a bound refusal, so a failing store is never reported as an
// operator's refusal: the same holds when no holds is given at all, which a
// bound change refuses without reading anything rather than failing open.
func TestBoundStoreErrorIsNotARefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.block", settings.At(entity.ScopeInstallation), boundKey{Target: "d"}),
		settings.Of("test.plain", settings.At(entity.ScopeInstallation), boundLive{}),
	}

	key := "test.block.target"

	s.put(t, installation, key, `"v"`)

	for _, c := range []struct {
		name string
		w    settings.Write
	}{
		{"the put", settings.Put(setting(installation, key, `"t2"`))},
		{"the delete", settings.Delete(installation, key)},
	} {
		err := s.checkBuilt(tb, entity.RoleOperator, c.w, failingHolds)
		if !errors.Is(err, errHolds) || errors.Is(err, settings.ErrBound) {
			t.Errorf("%s: err = %v, want the store's error and not a bound refusal", c.name, err)
		}
	}

	err := s.check(tb, entity.RoleAdmin, settings.Put(setting(installation, key, `"t2"`)))
	if !errors.Is(err, settings.ErrNoHoldsForTest) || errors.Is(err, settings.ErrBound) {
		t.Errorf("a bound change with no holds: err = %v, want the missing check's error", err)
	}

	passed(
		t,
		"a non bound key",
		s.check(
			tb,
			entity.RoleAdmin,
			settings.Put(setting(installation, "test.plain.live", `"y"`)),
		),
	)

	passed(t, "the bound key's stored value",
		s.check(tb, entity.RoleAdmin, settings.Put(setting(installation, key, `"v"`))))
}

// The bound gate runs only on a bound key: a live sibling of the same section
// changes while content exists without a content read, and the bound one is
// judged.
func TestBoundOnlyBoundKeys(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.plain", settings.At(entity.ScopeInstallation), boundLive{}),
	}

	s.netgroup(t, contentName(installation))

	passed(t, "the live change",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, "test.plain.live", `"y"`)), failingHolds))

	err := s.checkBuilt(tb, entity.RoleOperator,
		settings.Put(setting(installation, "test.plain.bound", `"b"`)), failingHolds)
	if !errors.Is(err, errHolds) {
		t.Errorf("the bound change: err = %v, want the failing content read's", err)
	}
}

// The gates run after the gates before them: a write the section's own
// Validate refuses is refused by it even when it is also a bound change
// under content, and the bound refusal comes only for a change Validate
// takes.
func TestGatesRunAfterTheEarlierOnes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("test.port", settings.At(entity.ScopeInstallation), orderPort{}),
	}

	s.netgroup(t, contentName(installation))

	refused(t, "a write Validate refuses",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, "test.port.port", "70000")), holdsContent),
		settings.ErrInvalid,
		`setting (installation): test.port: port must be at most 65535, got 70000`)

	refused(t, "the bound change alone",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, "test.port.port", "8000")), holdsContent),
		settings.ErrBound, "")
}

// A remote store's endpoint, bucket, prefix or credential reference is created
// by its first write and never changed by a plain one after that: the first
// put passes with no row before it and no default, the same put over it
// passes, another value is refused naming RFC 13 §5.2, and a credential
// reference is set by the secrets API alone, never by value.
func TestStoreLocationFirstValuePasses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.remote",
			settings.At(entity.ScopeInstallation),
			remoteEndpoint{},
			settings.Required("Endpoint"),
		),
		settings.Of(
			"test.credential",
			settings.At(entity.ScopeInstallation),
			remoteCredential{},
			settings.Required("Credential"),
			settings.SecretRef("Credential", entity.SecretRemoteCredential),
		),
	}

	key := "test.remote.endpoint"

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"https://s3"`),
		holdsContent,
		nil,
	)
	passed(t, "the first put", err)

	if got, want := rowsStored(t, s, installation),
		map[string]string{key: `"https://s3"`}; !maps.Equal(got, want) {
		t.Errorf("the first put: rows = %v, want %v", got, want)
	}

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"https://s3"`),
		holdsContent,
		nil,
	)
	passed(t, "the same put again", err)

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, `"https://e2"`),
		holdsContent,
		nil,
	)
	refused(t, "another value", err, settings.ErrUnproven,
		`setting (installation): test.remote.endpoint: `+
			`store location change not provable yet, got "https://e2" (RFC 13 §5.2)`)

	if got, want := rowsStored(t, s, installation),
		map[string]string{key: `"https://s3"`}; !maps.Equal(got, want) {
		t.Errorf("the refused change: rows = %v, want %v", got, want)
	}

	err = s.check(tb, entity.RoleOperator, settings.Put(setting(installation,
		"test.credential.credential",
		"[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]")))
	refused(t, "the credential reference", err, settings.ErrSecretRef,
		"setting (installation): test.credential.credential: "+
			"a reference is set by the secrets API, not by value")
}

// A delete reaches a store location as its put does: removing a stored value
// the effective value would change to is refused, removing one equal to the
// value it leaves passes, and so does removing no row at all.
func TestStoreLocationDelete(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.bucket",
			settings.At(entity.ScopeInstallation),
			remoteBucketOnly{Bucket: "b0"},
		),
	}

	key := "test.bucket.bucket"

	s.put(t, installation, key, `"b1"`)

	refused(t, "the delete that leaves the default",
		s.storedDelete(tb, entity.RoleOperator, installation, key, holdsContent),
		settings.ErrUnproven,
		`setting (installation): test.bucket.bucket: `+
			`store location change not provable yet (RFC 13 §5.2)`)

	s.put(t, installation, key, `"b0"`)
	passed(t, "the delete that leaves the same value",
		s.storedDelete(tb, entity.RoleOperator, installation, key, holdsContent))

	passed(t, "the delete with no row stored",
		s.storedDelete(tb, entity.RoleOperator, installation, key, holdsContent))

	if got := rowsStored(t, s, installation); len(got) != 0 {
		t.Errorf("the accepted deletes left rows: %v", got)
	}
}

// The gates run in the plan's order, the bound one before the store location
// one: a bucket both bound and a store location is refused by the bound rule
// under content and by the store one without it, and a store location that is
// not bound is refused by the store rule even under content.
func TestStoreLocationAndBoundOrder(t *testing.T) {
	prof.Watch(t, prof.Options{})

	tb := settings.Table{
		settings.Of(
			"test.bucket",
			settings.At(entity.ScopeInstallation),
			remoteBucket{Bucket: "b0"},
		),
		settings.Of(
			"test.ostore",
			settings.At(entity.ScopeInstallation),
			remoteBucketOnly{Bucket: "b0"},
		),
	}

	under := open(t)
	under.netgroup(t, contentName(installation))

	_, err := under.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, "test.bucket.bucket", `"b2"`),
		holdsContent,
		nil,
	)
	refused(t, "a bound bucket under content", err, settings.ErrBound,
		`setting (installation): test.bucket.bucket: `+
			`bound setting changed while its scope holds content, got "b2" (RFC 13 §5.1)`)

	_, err = under.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, "test.ostore.bucket", `"b2"`),
		holdsContent,
		nil,
	)
	refused(t, "a store location under content", err, settings.ErrUnproven,
		`setting (installation): test.ostore.bucket: `+
			`store location change not provable yet, got "b2" (RFC 13 §5.2)`)

	none := open(t)

	_, err = none.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, "test.bucket.bucket", `"b2"`),
		holdsContent,
		nil,
	)
	refused(t, "the same bucket with no content", err, settings.ErrUnproven,
		`setting (installation): test.bucket.bucket: `+
			`store location change not provable yet, got "b2" (RFC 13 §5.2)`)
}

// A block format version past this node's range is refused, so a rolling
// upgrade cannot leave blocks on nodes that cannot parse them, while the
// range's own ceiling and any version below its floor pass, as the section's
// own Validate judges those, and an integer field of the other width is
// guarded the same way (RFC 13 §5.4).
func TestFormatVersionGate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := settings.Versions()[entity.FormatBlock]
	above := strconv.FormatUint(uint64(r.Max)+1, 10)
	at := strconv.FormatUint(uint64(r.Max), 10)

	floor := r.Min
	if floor > 0 {
		floor--
	}

	below := strconv.FormatUint(uint64(floor), 10)

	key := "test.block.version"
	long := "test.block.long.version"
	line := func(k string) string {
		return fmt.Sprintf(
			"setting (installation): %s: "+
				"block format version %s is past this node's range, up to %d (RFC 13 §5.4)",
			k, above, r.Max,
		)
	}

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.block",
			settings.At(entity.ScopeInstallation),
			blockFormat{Version: r.Max},
			settings.FormatVersion("Version"),
		),
		settings.Of(
			"test.block.long",
			settings.At(entity.ScopeInstallation),
			blockFormatLong{Version: int64(r.Max)},
			settings.FormatVersion("Version"),
		),
	}

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, above),
		holdsContent,
		nil,
	)
	refused(t, "a version past the range", err, settings.ErrFormatVersion, line(key))

	if got := rowsStored(t, s, installation); len(got) != 0 {
		t.Errorf("the refused version: rows = %v, want none", got)
	}

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, at),
		holdsContent,
		nil,
	)
	passed(t, "the range's own ceiling", err)

	if got, want := rowsStored(t, s, installation),
		map[string]string{key: at}; !maps.Equal(got, want) {
		t.Errorf("the ceiling: rows = %v, want %v", got, want)
	}

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, key, below),
		holdsContent,
		nil,
	)
	passed(t, "a version below the range's floor", err)

	if got, want := rowsStored(t, s, installation),
		map[string]string{key: below}; !maps.Equal(got, want) {
		t.Errorf("the stored row: rows = %v, want the accepted version only %v", got, want)
	}

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, long, above),
		holdsContent,
		nil,
	)
	refused(t, "an int64 field's version past the range", err, settings.ErrFormatVersion,
		line(long))

	_, err = s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, long, at),
		holdsContent,
		nil,
	)
	passed(t, "the int64 field's own ceiling", err)
}

// The format version gate runs only on the key it marks and only on a put:
// the same number on another key of the section passes, and the version key's
// delete passes, since a delete sets nothing, even when a wider row at the
// installation holds a version above this node's range, written by a node
// with a wider one or around Check.
func TestFormatVersionOtherKeys(t *testing.T) {
	prof.Watch(t, prof.Options{})

	max := settings.Versions()[entity.FormatBlock].Max
	above := strconv.FormatUint(uint64(max)+1, 10)
	at := strconv.FormatUint(uint64(max), 10)

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.block",
			settings.At(entity.ScopeInstallation),
			blockFormat{Version: max},
			settings.FormatVersion("Version"),
		),
		settings.Of(
			"test.node",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			blockFormat{Version: max},
			settings.FormatVersion("Version"),
		),
	}

	_, err := s.attemptPut(
		tb,
		entity.RoleOperator,
		setting(installation, "test.block.other", above),
		holdsContent,
		nil,
	)
	passed(t, "another key with the same number", err)

	s.put(t, installation, "test.block.version", at)
	passed(t, "the version key's delete",
		s.storedDelete(tb, entity.RoleOperator, installation, "test.block.version", holdsContent))

	want := map[string]string{"test.block.other": above}
	if got := rowsStored(t, s, installation); !maps.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}

	node := "test.node.version"
	s.raw(t, installation, node, rowValue(above, 1,
		settings.Versions()[entity.FormatSettings].Max))
	s.put(t, nodeA, node, at)

	passed(t, "the node delete under a wider installation row",
		s.storedDelete(tb, entity.RoleOperator, nodeA, node, holdsContent))
}

// The format version key's own row is never read by the gate, so a put can
// rewrite it over what a newer binary left: a row of a newer schema, or bytes
// nothing decodes (check.go's own-row rule, RFC 13 §2.3), while the version
// the put writes is still judged against this node's range.
func TestFormatVersionOwnNewerRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	max := settings.Versions()[entity.FormatBlock].Max
	at := strconv.FormatUint(uint64(max), 10)
	current := settings.Versions()[entity.FormatSettings].Max

	s := open(t)
	tb := settings.Table{
		settings.Of(
			"test.block",
			settings.At(entity.ScopeInstallation),
			blockFormat{Version: max},
			settings.FormatVersion("Version"),
		),
	}

	key := "test.block.version"

	s.raw(t, installation, key, rowValue(at, 1, current+1))
	passed(t, "a put over the own newer row",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, at)), holdsContent))

	s.raw(t, installation, key, rowValue(`v`, 1, current))
	passed(t, "a put over the own undecodable row",
		s.checkBuilt(tb, entity.RoleOperator,
			settings.Put(setting(installation, key, at)), holdsContent))
}
