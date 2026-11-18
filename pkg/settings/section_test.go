package settings

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// valid gives the test structs that embed it a Validate that passes; it
// declares no field, so it adds no key.
type valid struct{}

func (valid) Validate() error {
	return nil
}

// The small settings structs the tests declare sections over.
type (
	plain struct {
		valid
		Lo         uint32           `json:"lo"         class:"live"`
		Credential entity.SecretRef `json:"credential" class:"live"`
	}
	other struct {
		valid
		Lo uint32 `json:"lo" class:"live"`
	}
	fourClasses struct {
		valid
		L uint32 `json:"l" class:"live"`
		R uint32 `json:"r" class:"restart"`
		N uint32 `json:"n" class:"next write"`
		B uint32 `json:"b" class:"bound"`
	}
	smbExtra struct {
		adaptersettings.SMB
		Extra int `json:"extra"`
	}
	noClass struct {
		valid
		A int `json:"a"`
	}
	restar struct {
		valid
		A int `json:"a" class:"restar"`
	}
	capitalRestart struct {
		valid
		A int `json:"a" class:"Restart"`
	}
	underscoreNextWrite struct {
		valid
		A int `json:"a" class:"next_write"`
	}
	dashNextWrite struct {
		valid
		A int `json:"a" class:"next-write"`
	}
	noJSON struct {
		valid
		A int `class:"live"`
	}
	optionsOnlyJSON struct {
		valid
		A int `json:",omitempty" class:"live"`
	}
	Inner struct {
		B int `json:"a" class:"live"`
	}
	shadowed struct {
		valid
		Inner
		A int `json:"a" class:"live"`
	}
	embeddedPointer struct {
		valid
		*Inner
	}
	namedEmbedded struct {
		valid
		Inner `json:"inner" class:"live"`
	}
	withUnexported struct {
		valid
		Lo   uint32 `json:"lo" class:"live"`
		note string
	}
	twoRefs struct {
		valid
		Credential entity.SecretRef `json:"credential" class:"live"`
		Bind       entity.SecretRef `json:"bind"       class:"live"`
	}
	PortInner struct {
		Port uint16 `json:"inner_port" class:"live"`
	}
	portTwice struct {
		valid
		Port uint16 `json:"port" class:"live"`
		PortInner
	}
	notStruct int
)

func (notStruct) Validate() error {
	return nil
}

// panicText runs f and returns what it panicked with, or "" if it returned.
func panicText(t *testing.T, f func()) (text string) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			s, ok := r.(string)
			if !ok {
				t.Fatalf("panicked with %T %v, want a string", r, r)
			}

			text = s
		}
	}()

	f()

	return ""
}

// declared returns what declare returns, and fails the test, naming the
// panic, when it panics.
func declared(t *testing.T, declare func() Section) Section {
	t.Helper()

	var sec Section

	if text := panicText(t, func() {
		sec = declare()
	}); text != "" {
		t.Fatalf("declaration panicked: %s", text)
	}

	return sec
}

// classesOf is every key of sec with its class.
func classesOf(sec Section) map[string]Class {
	got := make(map[string]Class, len(sec.fields))
	for _, f := range sec.fields {
		got[f.key] = f.class
	}

	return got
}

var installation = At(entity.ScopeInstallation)

func TestOfSMBSection(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := declared(t, func() Section {
		return Of(
			"adapters.smb",
			Override(entity.ScopeInstallation, entity.ScopeNode),
			adaptersettings.DefaultSMB(),
			OverrideOnly("Port"),
		)
	})

	want := map[string]Class{
		"adapters.smb.port":                Restart,
		"adapters.smb.drain_timeout":       Live,
		"adapters.smb.idle_timeout":        Live,
		"adapters.smb.write_timeout":       Live,
		"adapters.smb.handshake_timeout":   Live,
		"adapters.smb.read_timeout":        Live,
		"adapters.smb.disabled_operations": Live,
		"adapters.smb.dialects":            Live,
		"adapters.smb.pending_timeout":     Live,
		"adapters.smb.recall_timeout":      Live,
	}
	if got := classesOf(sec); !maps.Equal(got, want) {
		t.Errorf("keys and classes = %v, want %v", got, want)
	}

	if !slices.Equal(sec.overrideOnly, []string{"adapters.smb.port"}) {
		t.Errorf("overridable keys = %q, want [adapters.smb.port]", sec.overrideOnly)
	}
}

func TestOfClasses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sec := declared(t, func() Section {
		return Of("p", installation, fourClasses{})
	})

	want := map[string]Class{"p.l": Live, "p.r": Restart, "p.n": NextWrite, "p.b": Bound}
	if got := classesOf(sec); !maps.Equal(got, want) {
		t.Errorf("keys and classes = %v, want %v", got, want)
	}
}

func TestOfUntaggedSMBFieldPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := panicText(t, func() {
		Of("adapters.smb", installation, smbExtra{SMB: adaptersettings.DefaultSMB()})
	})

	const want = "settings: section adapters.smb: field Extra has no class tag"
	if got != want {
		t.Errorf("panic = %q, want %q", got, want)
	}
}

func TestOfPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	smb := adaptersettings.DefaultSMB()
	node := entity.ScopeNode
	override := Override(entity.ScopeInstallation, node)
	checkOther := CheckTx(func(entity.ControlPlaneTx, Write, other) error {
		return nil
	})

	cases := []struct {
		name string
		of   func()
		want string
	}{
		{
			"no class tag",
			func() {
				Of("p", installation, noClass{})
			},
			"settings: section p: field A has no class tag",
		},
		{
			"misspelled class",
			func() {
				Of("p", installation, restar{})
			},
			`settings: section p: field A has class "restar", ` +
				"not one of live, restart, next write, bound",
		},
		{
			"class in another case",
			func() {
				Of("p", installation, capitalRestart{})
			},
			`settings: section p: field A has class "Restart", ` +
				"not one of live, restart, next write, bound",
		},
		{
			"next write with an underscore",
			func() {
				Of("p", installation, underscoreNextWrite{})
			},
			`settings: section p: field A has class "next_write", ` +
				"not one of live, restart, next write, bound",
		},
		{
			"next write with a dash",
			func() {
				Of("p", installation, dashNextWrite{})
			},
			`settings: section p: field A has class "next-write", ` +
				"not one of live, restart, next write, bound",
		},
		{
			"no json tag",
			func() {
				Of("p", installation, noJSON{})
			},
			"settings: section p: field A has no json name",
		},
		{
			"json options without a name",
			func() {
				Of("p", installation, optionsOnlyJSON{})
			},
			"settings: section p: field A has no json name",
		},
		{
			"outer field shadowing an embedded one",
			func() {
				Of("p", installation, shadowed{})
			},
			"settings: section p: fields B and A share the key p.a",
		},
		{
			"embedded pointer read as a field",
			func() {
				Of("p", installation, embeddedPointer{})
			},
			"settings: section p: field Inner has no json name",
		},
		{
			"not a struct",
			func() {
				Of("p", installation, notStruct(0))
			},
			"settings: section p: settings.notStruct is not a struct",
		},
		{
			"Required naming no field",
			func() {
				Of("p", installation, plain{}, Required("Missing"))
			},
			"settings: section p: Required names Missing, which is no setting field",
		},
		{
			"SecretRef naming no field",
			func() {
				Of("p", installation, plain{}, SecretRef("Missing", entity.SecretRemoteCredential))
			},
			"settings: section p: SecretRef names Missing, which is no setting field",
		},
		{
			"Required naming a json:\"-\" field",
			func() {
				Of("adapters.smb", installation, smb, Required("Addrs"))
			},
			"settings: section adapters.smb: Required names Addrs, which is no setting field",
		},
		{
			"OverrideOnly naming the json key",
			func() {
				Of("adapters.smb", override, smb, OverrideOnly("port"))
			},
			"settings: section adapters.smb: OverrideOnly names port, which is no setting field",
		},
		{
			"OverrideOnly under At",
			func() {
				Of("adapters.smb", installation, smb, OverrideOnly("Port"))
			},
			"settings: section adapters.smb: OverrideOnly outside an Override scope",
		},
		{
			"OverrideOnly under PerThing",
			func() {
				Of("adapters.smb", PerThing(node, "journal"), smb, OverrideOnly("Port"))
			},
			"settings: section adapters.smb: OverrideOnly outside an Override scope",
		},
		{
			"write role none",
			func() {
				Of("p", installation, plain{}, WriteRole(entity.RoleNone))
			},
			"settings: section p: write role 0 is neither operator nor admin",
		},
		{
			"write role viewer",
			func() {
				Of("p", installation, plain{}, WriteRole(entity.RoleViewer))
			},
			"settings: section p: write role 1 is neither operator nor admin",
		},
		{
			"write role unknown",
			func() {
				Of("p", installation, plain{}, WriteRole(entity.APIRole(9)))
			},
			"settings: section p: write role 9 is neither operator nor admin",
		},
		{
			"no scope",
			func() {
				Of("p", Scope{}, plain{})
			},
			"settings: section p: no scope: declare it with At, PerThing or Override",
		},
		{
			"CheckTx of another type",
			func() {
				Of("p", installation, plain{}, checkOther)
			},
			"settings: section p: CheckTx checks settings.other, not settings.plain",
		},
		{
			"empty prefix",
			func() {
				Of("", installation, plain{})
			},
			`settings: section "": empty prefix`,
		},
		{
			"prefix ending in a dot",
			func() {
				Of("adapters.", installation, plain{})
			},
			`settings: section "adapters.": prefix ends in a dot`,
		},
		{
			"At with no scope kind",
			func() {
				At(0)
			},
			"settings: scope kind 0 is none of the five scopes",
		},
		{
			"PerThing with no scope kind",
			func() {
				PerThing(0, "journal")
			},
			"settings: scope kind 0 is none of the five scopes",
		},
		{
			"PerThing with an empty thing",
			func() {
				PerThing(node, "")
			},
			"settings: PerThing with an empty thing",
		},
		{
			"Override with no wide scope kind",
			func() {
				Override(0, node)
			},
			"settings: scope kind 0 is none of the five scopes",
		},
		{
			"Override with an unknown narrow scope kind",
			func() {
				Override(entity.ScopeInstallation, 6)
			},
			"settings: scope kind 6 is none of the five scopes",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := panicText(t, c.of); got != c.want {
				t.Errorf("panic = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOfOptionsKept(t *testing.T) {
	prof.Watch(t, prof.Options{})

	if got := Of("p", installation, plain{}).writeRole; got != entity.RoleOperator {
		t.Errorf("default write role = %d, want operator (%d)", got, entity.RoleOperator)
	}

	admin := Of("p", installation, plain{}, WriteRole(entity.RoleAdmin))
	if admin.writeRole != entity.RoleAdmin {
		t.Errorf("write role = %d, want admin (%d)", admin.writeRole, entity.RoleAdmin)
	}

	sec := Of(
		"p",
		installation,
		plain{},
		Required("Lo"),
		SecretRef("Credential", entity.SecretRemoteCredential),
	)
	if !slices.Equal(sec.required, []string{"p.lo"}) {
		t.Errorf("required keys = %q, want [p.lo]", sec.required)
	}

	wantRefs := map[string]entity.SecretKind{"p.credential": entity.SecretRemoteCredential}
	if !maps.Equal(sec.secretRefs, wantRefs) {
		t.Errorf("secret references = %v, want %v", sec.secretRefs, wantRefs)
	}

	smb := Of("adapters.smb", installation, adaptersettings.DefaultSMB(), Required("Port"))
	if !slices.Equal(smb.required, []string{"adapters.smb.port"}) {
		t.Errorf("required keys = %q, want [adapters.smb.port]", smb.required)
	}

	both := []string{"p.lo", "p.credential"}

	oneOption := Of("p", installation, plain{}, Required("Lo", "Credential"))
	if !slices.Equal(oneOption.required, both) {
		t.Errorf("Required(Lo, Credential) keys = %q, want %q", oneOption.required, both)
	}

	twoOptions := Of("p", installation, plain{}, Required("Lo"), Required("Credential"))
	if !slices.Equal(twoOptions.required, both) {
		t.Errorf("Required(Lo), Required(Credential) keys = %q, want %q",
			twoOptions.required, both)
	}

	refs := Of(
		"p",
		installation,
		twoRefs{},
		SecretRef("Credential", entity.SecretRemoteCredential),
		SecretRef("Bind", entity.SecretPassword),
	)

	wantTwo := map[string]entity.SecretKind{
		"p.credential": entity.SecretRemoteCredential,
		"p.bind":       entity.SecretPassword,
	}
	if !maps.Equal(refs.secretRefs, wantTwo) {
		t.Errorf("two SecretRef options = %v, want %v", refs.secretRefs, wantTwo)
	}

	override := Override(entity.ScopeInstallation, entity.ScopeNode)
	wantOverride := []string{"adapters.smb.port", "adapters.smb.drain_timeout"}

	names := Of(
		"adapters.smb",
		override,
		adaptersettings.DefaultSMB(),
		OverrideOnly("Port", "DrainTimeout"),
	)
	if !slices.Equal(names.overrideOnly, wantOverride) {
		t.Errorf("OverrideOnly(Port, DrainTimeout) keys = %q, want %q",
			names.overrideOnly, wantOverride)
	}

	options := Of(
		"adapters.smb",
		override,
		adaptersettings.DefaultSMB(),
		OverrideOnly("Port"),
		OverrideOnly("DrainTimeout"),
	)
	if !slices.Equal(options.overrideOnly, wantOverride) {
		t.Errorf("OverrideOnly(Port), OverrideOnly(DrainTimeout) keys = %q, want %q",
			options.overrideOnly, wantOverride)
	}
}

func TestOfFieldRules(t *testing.T) {
	prof.Watch(t, prof.Options{})

	twice := Of(
		"p",
		installation,
		plain{},
		WriteRole(entity.RoleAdmin),
		WriteRole(entity.RoleOperator),
	)
	if twice.writeRole != entity.RoleOperator {
		t.Errorf("write role given twice = %d, want the last, operator (%d)",
			twice.writeRole, entity.RoleOperator)
	}

	shallow := Of("p", installation, portTwice{}, Required("Port"))
	if !slices.Equal(shallow.required, []string{"p.port"}) {
		t.Errorf("Required(Port) with an outer and an embedded Port = %q, want the outer, [p.port]",
			shallow.required)
	}

	named := Of("p", installation, namedEmbedded{})
	if got, want := classesOf(named), map[string]Class{"p.inner": Live}; !maps.Equal(got, want) {
		t.Errorf("named embedded struct keys = %v, want %v", got, want)
	}

	unexported := declared(t, func() Section {
		return Of("p", installation, withUnexported{note: "not a setting"})
	})
	if got, want := classesOf(unexported), map[string]Class{"p.lo": Live}; !maps.Equal(got, want) {
		t.Errorf("keys with an unexported field = %v, want %v", got, want)
	}
}

func TestOfScopes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	cases := []struct {
		name  string
		scope Scope
		want  Scope
	}{
		{
			"At",
			At(entity.ScopeInstallation),
			Scope{form: formAt, wide: entity.ScopeInstallation},
		},
		{
			"PerThing",
			PerThing(entity.ScopeNode, "journal"),
			Scope{form: formPerThing, wide: entity.ScopeNode, thing: "journal"},
		},
		{
			"Override",
			Override(entity.ScopeInstallation, entity.ScopeNode),
			Scope{form: formOverride, wide: entity.ScopeInstallation, narrow: entity.ScopeNode},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.scope != c.want {
				t.Errorf("scope = %+v, want %+v", c.scope, c.want)
			}

			if got := Of("p", c.scope, plain{}).scope; got != c.want {
				t.Errorf("section's scope = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestOfCheckTxKept(t *testing.T) {
	prof.Watch(t, prof.Options{})

	refused := errors.New("refused by the component")

	var (
		gotWrite Write
		gotValue plain
	)

	sec := Of("p", installation, plain{}, CheckTx(
		func(_ entity.ControlPlaneTx, w Write, v plain) error {
			gotWrite, gotValue = w, v

			return refused
		},
	))
	if sec.checkTx == nil {
		t.Fatal("section declared with CheckTx has no check")
	}

	w := Write{Key: "p.lo"}

	err := sec.checkTx(nil, w, plain{Lo: 7})
	if err != refused { //nolint:errorlint // the check's error must come back as itself, unwrapped
		t.Errorf("check returned %v, want the component's error as itself", err)
	}

	if gotWrite != w || gotValue.Lo != 7 {
		t.Errorf("check handed write %+v and value %+v, want %+v and Lo 7", gotWrite, gotValue, w)
	}

	if Of("p", installation, plain{}).checkTx != nil {
		t.Error("section declared without CheckTx has a check")
	}
}

func TestRank(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ordered := []entity.APIRole{
		entity.RoleNone,
		entity.RoleViewer,
		entity.RoleOperator,
		entity.RoleAdmin,
	}
	for i := 1; i < len(ordered); i++ {
		if rank(ordered[i-1]) >= rank(ordered[i]) {
			t.Errorf("rank(%d) = %d, not below rank(%d) = %d",
				ordered[i-1], rank(ordered[i-1]), ordered[i], rank(ordered[i]))
		}
	}

	for _, unknown := range []entity.APIRole{4, 255} {
		if got, want := rank(unknown), rank(entity.RoleNone); got != want {
			t.Errorf("rank(%d) = %d, want the lowest, %d", unknown, got, want)
		}
	}
}
