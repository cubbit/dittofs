package settings

import (
	"errors"
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

// Refuse is a section's CheckTx refusing w by rule, printed as RFC 13 §6's
// line naming w's scope and key, and matching ErrInvalid.
func (w Write) Refuse(rule string) error {
	return refuse(w.Scope, w.Key, rule, ErrInvalid)
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

	if !sec.declares(declared) && w.put != nil {
		return refuse(w.Scope, strconv.Quote(w.Key),
			"unknown key; did you mean "+sec.nearest(w.Key, thing)+"?", ErrUnknownKey)
	}

	if err := sec.checkScope(w, declared); err != nil {
		return err
	}

	if !sec.declares(declared) {
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

	if w.put != nil && w.put.Schema > maxSchema() {
		return newer(w.Scope, w.Key, w.put.Schema)
	}

	if rank(caller) < rank(sec.writeRole) {
		return refuse(w.Scope, w.Key, "written by "+writers(sec.writeRole), ErrForbidden)
	}

	// A key KeyRole names carries a higher bar than its section's, so an
	// operator keeps the section's other keys while this one stays an
	// admin's, first value included: the gate runs before anything decodes or
	// judges the value.
	if role, guarded := sec.keyRoles[declared]; guarded && rank(caller) < rank(role) {
		return refuse(w.Scope, w.Key, "written by "+writers(role), ErrForbidden)
	}

	// decision: a delete of a per-thing key with no thing names no row of any
	// thing, so no required key or Validate judges it; refusing it would leave
	// such a row unremovable. Withdraw once a per-thing section stores a key
	// with no thing.
	if sec.scope.form == formPerThing && thing == "" {
		return nil
	}

	effective, holders, err := sec.after(tx, w, declared, thing)
	if err != nil {
		return err
	}

	// A plaintext typed into a reference field never reaches a Validate that
	// prints it: the reference gate runs before Validate, and its refusals
	// name the field, never the value (RFC 13 §6).
	if err := sec.checkSecretRefs(tx, effective, thing, holders); err != nil {
		return err
	}

	if err := effective.(interface{ Validate() error }).Validate(); err != nil {
		key := sec.prefix
		if thing != "" {
			key += "." + thing
		}

		return refuse(w.Scope, key, err.Error(), ErrInvalid)
	}

	if sec.checkTx != nil {
		if err := sec.checkTx(tx, w, effective); err != nil {
			return err
		}
	}

	// ponytail: a rule across components, a value one component's setting
	// must keep against another's, is not built: each section checks only
	// its own settings, and a section's CheckTx reads what else it needs.
	// Upgrade to a check that judges both sections in one write once a rule
	// across components is declared.

	// decision: the settings format's schema, one version across every
	// section, is written only once the installation's active version allows
	// it, so a rollback before that raise meets no row it cannot decode (RFC 13
	// §5.5). One write is not covered yet: a field a newer binary added is a new
	// key under a prefix this binary declares, and setting it before the raise
	// makes a rollback refuse that section. Withdraw this note once a write that
	// sets such a field is refused while that binary is newer than the active
	// version.
	return sec.gates(tx, w, declared, thing, holds, effective)
}

// maxSchema is the newest settings schema this binary reads.
func maxSchema() uint32 {
	return Versions()[entity.FormatSettings].Max
}

// newer refuses a write over a row of schema, newer than this binary reads,
// at scope and key.
func newer(scope entity.SettingScope, key string, schema uint32) error {
	return refuse(scope, key, fmt.Sprintf("schema %d is newer than this node reads, up to %d",
		schema, maxSchema()), ErrNewerSchema)
}

// after is the section's effective value once w is applied, for thing in a
// per-thing section, declared being w's key as the section declares it: the
// defaults, then each scope of w's chain overlaid, widest first, then the
// required keys. It reads each declared field at each scope of the chain
// through tx, one tracked point read each, so a concurrent write of any row
// it judged makes the write retry; a put's row takes the place of the stored
// one, and a delete drops it; any other stored row of a newer schema refuses
// the write. The returned holders map names, for every field a SecretRef
// option declares, the narrowest row of the chain that sets it, with its
// true Scope and Key, the row the reference gate judges the owner against.
// A delete of a key the section does not declare reads that row too, and
// never judges its schema, so a newer binary's row stays removable.
func (sec *Section) after(
	tx entity.ControlPlaneTx,
	w Write,
	declared, thing string,
) (any, map[string]entity.Setting, error) {
	value := sec.def
	set := make(map[string]bool, len(sec.fields))

	var holders map[string]entity.Setting
	if len(sec.secretRefs) > 0 {
		holders = make(map[string]entity.Setting, len(sec.secretRefs))
	}

	for _, scope := range sec.chain(w.Scope) {
		var rows []entity.Setting

		for _, f := range sec.fields {
			key := sec.written(f.key, thing)

			row, found, err := tx.Setting(scope, key)
			if err != nil {
				return nil, nil, fmt.Errorf("settings: read %s at %s: %w", key, scope.Kind, err)
			}

			// The write's own row is replaced or removed, never read for
			// its fields, so its schema is not judged (RFC 13 §2.3); every
			// other row of the chain is read, and a newer one refuses.
			if scope == w.Scope && key == w.Key {
				found = w.put != nil
				if found {
					row = *w.put
				}
			} else if found && row.Schema > maxSchema() {
				return nil, nil, newer(scope, key, row.Schema)
			}

			if !found {
				continue
			}

			// decision: a narrower row of a field OverrideOnly does not name
			// is skipped, as the node's read skips it, never refused: Check
			// refuses writing one, so only a store written around Check
			// holds one, and it changes no value.
			if scope.Kind == sec.scope.narrow && len(sec.overrideOnly) > 0 &&
				!slices.Contains(sec.overrideOnly, f.key) {
				continue
			}

			// The holder is recorded while the row still holds its written
			// key, before the decode relabels it to the declared one, so
			// the reference gate compares the owner the sealing routes name.
			// The two keys differ only in a per-thing section, whose written
			// key carries the thing, and no section of that form declares a
			// reference today, so no test tells them apart yet.
			if holders != nil {
				if _, named := sec.secretRefs[f.key]; named {
					holders[f.key] = row
				}
			}

			row.Key = f.key
			rows = append(rows, row)
			set[f.key] = true
		}

		decoded, err := sec.decodeAny(sec, value, scope, rows)
		if err != nil {
			return nil, nil, sec.inThing(err, thing)
		}

		value = decoded
	}

	if w.deletes && !sec.declares(declared) {
		if _, _, err := tx.Setting(w.Scope, w.Key); err != nil {
			return nil, nil, fmt.Errorf("settings: read %q at %s: %w", w.Key, w.Scope.Kind, err)
		}
	}

	var missing []error

	for _, key := range sec.required {
		if !set[key] {
			missing = append(missing, refuse(w.Scope, sec.written(key, thing),
				"required and has no value", ErrRequired))
		}
	}

	if len(missing) > 0 {
		return nil, nil, errors.Join(missing...)
	}

	return value, holders, nil
}

// chain is the scopes whose rows make the effective value at scope, widest
// first: the wide scope, then scope itself when it is the narrow one of an
// Override. The wide scope of every Override RFC 13 declares is the
// installation, which has no ID.
func (sec *Section) chain(scope entity.SettingScope) []entity.SettingScope {
	if sec.scope.form == formOverride && scope.Kind == sec.scope.narrow {
		return []entity.SettingScope{
			{Kind: sec.scope.wide},
			scope,
		}
	}

	return []entity.SettingScope{scope}
}

// inThing writes a decode refusal's key for thing, as the write names it.
func (sec *Section) inThing(err error, thing string) error {
	var r *refusal
	if errors.As(err, &r) && thing != "" {
		r.key = sec.written(r.key, thing)
	}

	return err
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
