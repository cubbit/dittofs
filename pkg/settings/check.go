package settings

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Write is one change Check judges: a put of a setting or the delete of one
// scope's value, which leaves the wider row or the default effective.
type Write struct {
	Scope entity.SettingScope
	Key   string

	put     *entity.Setting // the row a put writes; nil for a delete
	deletes bool
}

// Put is the write of s.
func Put(s entity.Setting) Write {
	return Write{
		Scope: s.Scope,
		Key:   s.Key,
		put:   &s,
	}
}

// Delete is the removal of key's value at scope.
func Delete(scope entity.SettingScope, key string) Write {
	return Write{
		Scope:   scope,
		Key:     key,
		deletes: true,
	}
}

// HoldsContent reports whether scope holds content, read by Check's caller in
// the write's transaction. Check calls it only for a change to a bound
// setting, and refuses the write when it returns an error, so a bound setting
// never fails open.
type HoldsContent func(scope entity.SettingScope) (bool, error)

// Check judges w for caller inside the write's transaction tx, reading only
// through tx, and returns the refusal or nil.
//
// decision: a secret's rotation never reaches Check, since a setting's
// reference names the secret, not one of its versions: a new version changes
// no setting. Check judges only a reference repointed at another secret.
func (t Table) Check(
	tx entity.ControlPlaneTx,
	caller entity.APIRole,
	w Write,
	holds HoldsContent,
) error {
	if w.put == nil && !w.deletes {
		return fmt.Errorf(
			"settings: write of %q is neither a put nor a delete: %w",
			w.Key,
			ErrInvalid,
		)
	}

	sec, declared, thing, found := t.index().resolve(w.Key)
	if !found {
		return refuse(w.Scope, strconv.Quote(w.Key), "unknown key", ErrUnknownKey)
	}

	if err := sec.checkScope(w, declared); err != nil {
		return err
	}

	if !sec.declares(declared) {
		if w.put != nil {
			return refuse(w.Scope, strconv.Quote(w.Key),
				"unknown key; did you mean "+sec.nearest(w.Key, thing)+"?", ErrUnknownKey)
		}

		// decision: a delete of a key the section does not declare, under
		// its prefix, passes for an admin only, whatever the section's write
		// role, so a section a newer binary's key left refused can be
		// repaired. An undeclared key has no class, so the class and bound
		// gates never run on it: during a rollback an admin can delete a
		// newer binary's bound key under existing content, and the key reads
		// as the wider value or its default when the newer binary returns,
		// the accepted cost. Withdraw the delete itself once a write that
		// sets a field a newer binary added is refused while that binary is
		// newer than the active version; a newer binary's section nested
		// under a declared prefix still needs it then.
		if rank(caller) < rank(entity.RoleAdmin) {
			return refuse(w.Scope, strconv.Quote(w.Key),
				"unknown key, deleted by admin only", ErrForbidden)
		}
	}

	if rank(caller) < rank(sec.writeRole) {
		return refuse(w.Scope, w.Key, "written by "+writers(sec.writeRole), ErrForbidden)
	}

	return nil
}

// refuse is a refusal of the write at scope, printed as RFC 13 §6's line.
func refuse(scope entity.SettingScope, key, rule string, kind error) error {
	return &refusal{
		scope: scope,
		key:   key,
		fault: fault{
			rule: rule,
		},
		kind: kind,
	}
}

// checkScope refuses w at a scope kind the section does not have, and a
// narrower record of a field OverrideOnly does not name.
func (sec *Section) checkScope(w Write, declared string) error {
	key := w.Key
	if !sec.declares(declared) {
		key = strconv.Quote(w.Key)
	}

	s := sec.scope
	kind := w.Scope.Kind

	switch {
	case s.form == formOverride && kind == s.narrow:
		if len(sec.overrideOnly) == 0 || slices.Contains(sec.overrideOnly, declared) ||
			!sec.declares(declared) {
			return nil
		}

		return refuse(w.Scope, key, "set "+where(s.wide)+", not "+where(kind)+"; "+
			where(kind)+" sets only "+strings.Join(sec.overrideOnly, ", "), ErrWrongScope)
	case kind == s.wide:
		return nil
	case s.form == formOverride:
		return refuse(w.Scope, key,
			"set "+where(s.wide)+" or "+where(s.narrow)+", not "+where(kind), ErrWrongScope)
	default:
		return refuse(w.Scope, key, "set "+where(s.wide)+", not "+where(kind), ErrWrongScope)
	}
}

// where names a scope kind as a refusal prints it: at installation, per node.
func where(kind entity.ScopeKind) string {
	if kind == entity.ScopeInstallation {
		return "at installation"
	}

	if kind.String() == "" {
		return "at an unknown scope"
	}

	return "per " + kind.String()
}

// writers names the roles that may write a section of write role role.
func writers(role entity.APIRole) string {
	if role == entity.RoleAdmin {
		return "admin only"
	}

	return "operator or admin"
}

// declares reports whether key is one of the section's fields, as declared.
func (sec *Section) declares(key string) bool {
	return slices.ContainsFunc(sec.fields, func(f field) bool {
		return f.key == key
	})
}

// nearest is the section's key closest to key, written for thing in a
// per-thing section; one with no thing names it by the scope's thing.
func (sec *Section) nearest(key, thing string) string {
	if sec.scope.form == formPerThing && thing == "" {
		thing = "<" + sec.scope.thing + ">"
	}

	keys := make([]string, 0, len(sec.fields))
	for _, f := range sec.fields {
		keys = append(keys, sec.written(f.key, thing))
	}

	return nearest(key, keys)
}

// written is a declared key as a write names it: with its thing in a
// per-thing section, as declared in any other.
func (sec *Section) written(declared, thing string) string {
	if sec.scope.form != formPerThing {
		return declared
	}

	return thingKey(sec.prefix, declared, thing)
}
