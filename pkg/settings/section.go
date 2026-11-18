package settings

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// Scope is one of RFC 13 §3's three forms of a setting's scope: At, PerThing
// or Override.
type Scope struct{}

// At is one value across the scope: "installation".
func At(scope entity.ScopeKind) Scope {
	panic(unbuilt("At"))
}

// PerThing is one value for each instance of a thing inside the scope, such
// as "node, per journal"; the thing is never itself a scope.
func PerThing(scope entity.ScopeKind, thing string) Scope {
	panic(unbuilt("PerThing"))
}

// Override is a value at the wide scope that one record of the narrow scope
// may replace, such as "installation default, node override".
func Override(wide, narrow entity.ScopeKind) Scope {
	panic(unbuilt("Override"))
}

// Section is one component's settings, declared by Of under a key prefix.
type Section struct{}

// Option is one of Of's options.
type Option struct{}

// Of declares the section of a component's typed settings T under prefix, with
// its scope, its defaults def and its options. Each field of T that json reads
// is a key, prefix.<json name>, and carries its Class in a class tag; an
// embedded struct is read as its own fields, and a field tagged json:"-" is not
// a setting. Once built, Of panics on a field with no class, no explicit json
// name or a class that is not one of the four, on two fields that share a
// name, and on an option naming a field T does not have; an option's field is
// a Go field name, found through an embedded struct as json flattens it. Until
// then it panics as a stub.
func Of[T interface{ Validate() error }](
	prefix string,
	scope Scope,
	def T,
	opts ...Option,
) Section {
	panic(unbuilt("Of"))
}

// WriteRole is the lowest role that may write the section; operator when the
// option is absent.
func WriteRole(role entity.APIRole) Option {
	panic(unbuilt("WriteRole"))
}

// Required names the fields with no default, which a snapshot missing one
// refuses. Each is a Go field name of the section's type, not a json key.
func Required(fields ...string) Option {
	panic(unbuilt("Required"))
}

// OverrideOnly names the fields an Override scope's narrow record may
// replace; every other field holds one value at the wide scope. Each is a Go
// field name of the section's type, not a json key. Without it, every field
// may be overridden.
func OverrideOnly(fields ...string) Option {
	panic(unbuilt("OverrideOnly"))
}

// CheckTx is a check the section's component hands Of, run inside the write's
// transaction on the effective value the write leaves, which reads store
// state beyond settings through tx's tracked reads. It is handed the write so
// it can return at once for a key it does not guard.
func CheckTx[T any](check func(tx entity.ControlPlaneTx, w Write, effective T) error) Option {
	panic(unbuilt("CheckTx"))
}

// SecretRef declares that field holds a reference to a secret of kind, never
// a secret value (RFC 13 §7). The field is a Go field name of the section's
// type, not a json key.
func SecretRef(field string, kind entity.SecretKind) Option {
	panic(unbuilt("SecretRef"))
}

// Table is every section the node declares. Once built, two sections that
// declare one key make the table panic, naming both, when it is first used.
type Table []Section
