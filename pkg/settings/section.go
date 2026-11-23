package settings

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Scope is one of RFC 13 §3's three forms of a setting's scope: At, PerThing
// or Override. The zero Scope is none of them, and Of panics on it.
type Scope struct {
	form   scopeForm
	wide   entity.ScopeKind
	narrow entity.ScopeKind
	thing  string
}

// scopeForm is which of RFC 13 §3's three forms a Scope is.
type scopeForm uint8

const (
	formAt scopeForm = iota + 1
	formPerThing
	formOverride
)

// At is one value across the scope: "installation". It panics on a scope
// kind that is none of RFC 13 §3's five scopes.
func At(scope entity.ScopeKind) Scope {
	return Scope{
		form: formAt,
		wide: knownScope(scope),
	}
}

// PerThing is one value for each instance of a thing inside the scope, such
// as "node, per journal"; the thing is never itself a scope. It panics on a
// scope kind that is none of the five, and on an empty thing.
func PerThing(scope entity.ScopeKind, thing string) Scope {
	if thing == "" {
		panic("settings: PerThing with an empty thing")
	}

	return Scope{
		form:  formPerThing,
		wide:  knownScope(scope),
		thing: thing,
	}
}

// Override is a value at the wide scope that one record of the narrow scope
// may replace, such as "installation default, node override". It panics on a
// scope kind that is none of the five.
func Override(wide, narrow entity.ScopeKind) Scope {
	return Scope{
		form:   formOverride,
		wide:   knownScope(wide),
		narrow: knownScope(narrow),
	}
}

// knownScope returns kind, or panics when it is none of the five scopes.
func knownScope(kind entity.ScopeKind) entity.ScopeKind {
	if kind.String() == "" {
		panic(fmt.Sprintf("settings: scope kind %d is none of the five scopes", kind))
	}

	return kind
}

// Section is one component's settings, declared by Of under a key prefix.
type Section struct {
	prefix       string
	typ          reflect.Type
	scope        Scope
	def          any
	fields       []field
	writeRole    entity.APIRole
	keyRoles     map[string]entity.APIRole
	required     []string
	secretRefs   map[string]entity.SecretKind
	overrideOnly []string
	checkTx      func(entity.ControlPlaneTx, Write, any) error
	// decodeAny is decode for T, so Check and Read build a value of a type
	// they only know at run time.
	decodeAny func(*Section, any, entity.SettingScope, []entity.Setting) (any, error)
	// formatVersion is the declared key that holds the block format version,
	// named by the FormatVersion option; "" names none.
	formatVersion string
	// series are the refusal counters WithMetrics gave this section's table:
	// nil on a table no registry was given, whose refusals count nothing.
	series *refusalSeries
}

// field is one key of a section: its binding class, whether it names how to
// reach a remote store's content, and the field's path in the section's type.
type field struct {
	key   string
	class Class
	store bool
	index []int
}

// Option is one of Of's options.
type Option struct {
	kind      optionKind
	role      entity.APIRole
	fields    []string
	secret    entity.SecretKind
	check     func(entity.ControlPlaneTx, Write, any) error
	checkType reflect.Type
}

// optionKind is which of Of's options an Option is.
type optionKind uint8

const (
	optWriteRole optionKind = iota + 1
	optRequired
	optOverrideOnly
	optCheckTx
	optSecretRef
	optFormatVersion
	optKeyRole
)

// classes are the class tag's spellings, RFC 13 §5's words as written.
var classes = map[string]Class{
	"live":       Live,
	"restart":    Restart,
	"next write": NextWrite,
	"bound":      Bound,
}

// Of declares the section of a component's typed settings T under prefix, with
// its scope, its defaults def and its options. Each field of T that json reads
// is a key, prefix.<json name>, and carries its Class in a class tag, spelled
// live, restart, next write or bound; an embedded struct with no json name is
// read as its own fields, and a field tagged json:"-" is not a setting. Of
// panics on a field with no class, no explicit json name or a class that is
// not one of the four, on two fields that share a key, on an option naming a
// field T does not have, on OverrideOnly outside an Override scope, on a write
// role other than operator and admin, on a KeyRole that does not rank above
// the section's final write role, on a CheckTx of another type than T, on
// the zero Scope, and on an empty prefix or one ending in a dot. An option's
// field is a Go field name, resolved as Go's selector resolves it, the
// shallowest field of that name, so a typo fails the table's first test
// instead of leaving a key unchecked.
//
// Each field that names how to reach a remote store's content (an endpoint,
// bucket, prefix or credential reference) carries, beside its class tag, a
// location:"store" tag, and Check refuses its change until the store's
// capability check can prove the claim at the new location (RFC 13 §5.2). Of
// panics naming the field and the value on a location tag of any other
// spelling, since json would read it as if it were absent and its gate would
// stay off. FormatVersion(field) names the field whose key holds the block
// format version; the field must be an integer, an int or a uint of any width
// and never a pointer, or Of panics naming the field and its kind. Of panics
// on a FormatVersion name that is no setting field and on two FormatVersion
// options in one section: either would leave its gate silent, and a
// declaration error must fail the table's first test.
//
// T's Validate may print the value it refuses, as RFC 13 §6's "got 70000"
// does, but never the value of a field declared with SecretRef, which may
// hold a plaintext typed in place of a reference (RFC 13 §6 and §7): Read
// prints Validate's refusals and runs no reference check of its own.
//
// A refusal from Validate prints under the section's key, the prefix or the
// prefix and the thing, so its text names the field and not the section, or
// the section shows twice.
//
// Only an embedded struct value is flattened. An embedded pointer
// is read as a field, so it needs its own json name and class, where json
// would flatten it: a nil default has no fields to read defaults from, and
// failing at Of is safer than a guess. Flatten it if a section ever needs to
// embed one.
func Of[T interface{ Validate() error }](
	prefix string,
	scope Scope,
	def T,
	opts ...Option,
) Section {
	if prefix == "" {
		panic(`settings: section "": empty prefix`)
	}

	if strings.HasSuffix(prefix, ".") {
		panic(fmt.Sprintf("settings: section %q: prefix ends in a dot", prefix))
	}

	refuse := func(format string, args ...any) {
		panic(fmt.Sprintf("settings: section %s: ", prefix) + fmt.Sprintf(format, args...))
	}

	if scope.form == 0 {
		refuse("no scope: declare it with At, PerThing or Override")
	}

	typ := reflect.TypeFor[T]()
	if typ.Kind() != reflect.Struct {
		refuse("%v is not a struct", typ)
	}

	sec := Section{
		prefix:    prefix,
		typ:       typ,
		scope:     scope,
		def:       clone(def),
		writeRole: entity.RoleOperator,
		decodeAny: func(
			sec *Section,
			base any,
			scope entity.SettingScope,
			rows []entity.Setting,
		) (any, error) {
			return decode(sec, base.(T), scope, rows)
		},
	}

	byPath := make(map[string]string) // a setting field's index path to its key
	byKey := make(map[string]string)  // key to Go field name

	var walk func(t reflect.Type, path []int)

	walk = func(t reflect.Type, path []int) {
		for sf := range t.Fields() {
			at := append(slices.Clone(path), sf.Index...)

			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}

			name, _, _ := strings.Cut(tag, ",")
			if sf.Anonymous && name == "" && sf.Type.Kind() == reflect.Struct {
				walk(sf.Type, at)

				continue
			}

			// json skips an unexported field unless it embeds a struct.
			if !sf.IsExported() && (!sf.Anonymous || sf.Type.Kind() != reflect.Struct) {
				continue
			}

			if name == "" {
				refuse("field %s has no json name", sf.Name)
			}

			spelling, tagged := sf.Tag.Lookup("class")
			if !tagged {
				refuse("field %s has no class tag", sf.Name)
			}

			class, ok := classes[spelling]
			if !ok {
				refuse("field %s has class %q, not one of live, restart, next write, bound",
					sf.Name, spelling)
			}

			located, locatedTag := sf.Tag.Lookup("location")
			if locatedTag && located != "store" {
				refuse("field %s has location %q, not store", sf.Name, located)
			}

			key := prefix + "." + name
			if first, dup := byKey[key]; dup {
				refuse("fields %s and %s share the key %s", first, sf.Name, key)
			}

			byKey[key] = sf.Name
			byPath[fmt.Sprint(at)] = key
			sec.fields = append(sec.fields, field{
				key:   key,
				class: class,
				store: locatedTag,
				index: at,
			})
		}
	}
	walk(typ, nil)

	keyOf := func(option, goName string) (string, reflect.Type) {
		sf, found := typ.FieldByName(goName)

		key, ok := byPath[fmt.Sprint(sf.Index)]
		if !found || !ok {
			refuse("%s names %s, which is no setting field", option, goName)
		}

		return key, sf.Type
	}

	for _, o := range opts {
		switch o.kind {
		case optWriteRole:
			if o.role != entity.RoleOperator && o.role != entity.RoleAdmin {
				refuse("write role %d is neither operator nor admin", o.role)
			}

			sec.writeRole = o.role
		case optRequired:
			for _, f := range o.fields {
				key, _ := keyOf("Required", f)
				sec.required = append(sec.required, key)
			}
		case optOverrideOnly:
			if scope.form != formOverride {
				refuse("OverrideOnly outside an Override scope")
			}

			for _, f := range o.fields {
				key, _ := keyOf("OverrideOnly", f)
				sec.overrideOnly = append(sec.overrideOnly, key)
			}
		case optCheckTx:
			if o.checkType != typ {
				refuse("CheckTx checks %v, not %v", o.checkType, typ)
			}

			sec.checkTx = o.check
		case optSecretRef:
			if sec.secretRefs == nil {
				sec.secretRefs = make(map[string]entity.SecretKind)
			}

			key, _ := keyOf("SecretRef", o.fields[0])
			sec.secretRefs[key] = o.secret
		case optFormatVersion:
			key, ftype := keyOf("FormatVersion", o.fields[0])

			switch ftype.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			default:
				refuse("FormatVersion field %s is %v, not an integer", o.fields[0], ftype)
			}

			if sec.formatVersion != "" {
				refuse("FormatVersion names both %s and %s",
					byKey[sec.formatVersion], o.fields[0])
			}

			sec.formatVersion = key
		case optKeyRole:
			key, _ := keyOf("KeyRole", o.fields[0])

			if sec.keyRoles == nil {
				sec.keyRoles = make(map[string]entity.APIRole)
			}

			sec.keyRoles[key] = o.role
		}
	}

	// A key role only raises the section's write role, and WriteRole may
	// name it after KeyRole, so the two are compared once both are in:
	// a key never ends below the role its section declares. The fields'
	// declared order decides which refusal a section with several to give
	// prints, so the message is always the same one.
	for _, f := range sec.fields {
		role, guarded := sec.keyRoles[f.key]

		if guarded && rank(role) <= rank(sec.writeRole) {
			refuse("KeyRole %s names %d, at or below the section's write role %d",
				byKey[f.key], role, sec.writeRole)
		}
	}

	return sec
}

// WriteRole is the lowest role that may write the section; operator when the
// option is absent. Of accepts operator and admin only, the roles RFC 23 §5.2
// lets write settings; given twice, the last wins.
func WriteRole(role entity.APIRole) Option {
	return Option{
		kind: optWriteRole,
		role: role,
	}
}

// KeyRole names the one field whose key the section's own write role does
// not cover, with the least role that may write it. A key role only raises
// the section's write role, never lowers it: Of panics on a role that does
// not rank above the section's, so an admin-only key keeps the section's
// other keys an operator writes. The field is a Go field name of the
// section's type, not a json key; given twice for one field, the last wins.
func KeyRole(field string, role entity.APIRole) Option {
	return Option{
		kind:   optKeyRole,
		fields: []string{field},
		role:   role,
	}
}

// Required names the fields with no default, which a snapshot missing one
// refuses. Each is a Go field name of the section's type, not a json key.
func Required(fields ...string) Option {
	return Option{
		kind:   optRequired,
		fields: fields,
	}
}

// OverrideOnly names the fields an Override scope's narrow record may
// replace; every other field holds one value at the wide scope. Each is a Go
// field name of the section's type, not a json key. Without it, every field
// may be overridden.
func OverrideOnly(fields ...string) Option {
	return Option{
		kind:   optOverrideOnly,
		fields: fields,
	}
}

// CheckTx is a check the section's component hands Of, run inside the write's
// transaction on the effective value the write leaves, which reads store
// state beyond settings through tx's tracked reads. It is handed the write so
// it can return at once for a key it does not guard. Its error is returned as
// it is, so its author words the refusal: a refusal is the Write's Refuse, and a
// store error met while reading wraps none of the errors Check returns, so a
// failing store is never counted as a refused value.
func CheckTx[T any](check func(tx entity.ControlPlaneTx, w Write, effective T) error) Option {
	return Option{
		kind: optCheckTx,
		check: func(tx entity.ControlPlaneTx, w Write, effective any) error {
			return check(tx, w, effective.(T))
		},
		checkType: reflect.TypeFor[T](),
	}
}

// SecretRef declares that field holds a reference to a secret of kind, never
// a secret value (RFC 13 §7). The field is a Go field name of the section's
// type, not a json key.
func SecretRef(field string, kind entity.SecretKind) Option {
	return Option{
		kind:   optSecretRef,
		fields: []string{field},
		secret: kind,
	}
}

// FormatVersion names the field whose key holds the block format version,
// which Check refuses past this node's range (RFC 13 §5.4). The field is a Go
// field name of the section's type, not a json key, an integer kind: an int
// or a uint of any width, never a pointer.
func FormatVersion(field string) Option {
	return Option{
		kind:   optFormatVersion,
		fields: []string{field},
	}
}

// rank orders the write roles by an explicit table, never by the role's
// number or name, so a role outside the table, empty or unknown, ranks lowest
// and passes no write role.
func rank(r entity.APIRole) int {
	switch r {
	case entity.RoleViewer:
		return 1
	case entity.RoleOperator:
		return 2
	case entity.RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Table is every section the node declares. Read and Check build its index
// once per call, which checks it: two sections that declare one prefix, or a
// key one section declares that another's prefix takes, make it panic,
// naming both.
type Table []Section
