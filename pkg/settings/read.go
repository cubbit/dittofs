package settings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Snapshot is every section's decoded and validated value, as one node runs
// them.
type Snapshot struct {
	node     entity.NodeID
	sections map[string]*entry
}

// entry is one section in a snapshot. A section with one value holds it in
// base; a section with one value per scope instance holds each instance's in
// instances, and in base the value an instance with no rows of its own runs.
// stored is whether any row went into base.
type entry struct {
	section   *Section
	base      outcome
	stored    bool
	instances map[[16]byte]outcome
	things    map[thingAt]outcome
}

// thingAt is one thing of a per-thing section: the scope instance it sits
// in and its ID as its key holds it.
type thingAt struct {
	id    [16]byte
	thing string
}

// outcome is a decoded and validated value, or the refusal that took its
// place.
type outcome struct {
	value any
	err   error
}

// level is the rows of one scope instance a section reads, and the rows of it
// refused before any decode: a row that does not decode or holds a newer
// schema.
type level struct {
	scope   entity.SettingScope
	rows    []entity.Setting
	refused []error
}

// levelKey is one level of a section: a scope instance and, in a per-thing
// section, one thing inside it; the thing is empty in any other section.
type levelKey struct {
	scope entity.SettingScope
	thing string
}

// Read decodes every section of table for node from the settings r lists
// across every scope, and returns the snapshot and the sections it refused,
// each with its error; a refused section does not stop the others.
//
// The walk is not one transaction: AllSettings reads each page in its own,
// so a write between two pages can leave a snapshot of two states. The caller
// reads the settings generation before the call and after it, and discards
// the snapshot if it moved (RFC 23 §2.3).
//
// decision: a row no section of table claims, a row whose key does not
// parse, a row at a scope kind its section does not have, another node's row
// and, at an OverrideOnly section's narrow scope, a row of a field the option
// does not name are skipped, neither refused nor counted. Such rows exist
// when another binary wrote them, a newer one that added or widened a
// section, or an older one whose section this binary dropped, and refusing
// them would stop a node of a mixed-version install, or of a rollback, over
// a setting it does not run. A corrupt narrow row whose key does not parse
// therefore falls back to the wider value. Withdraw both skips if a count
// of skipped rows is wanted for an alert, or if a store ever holds rows no
// binary of the install wrote.
func Read(
	ctx context.Context,
	r entity.SettingsReader,
	table Table,
	node entity.NodeID,
) (*Snapshot, map[string]error, error) {
	x := table.index()
	newest := Versions()[entity.FormatSettings].Max
	levels := make(map[*Section]map[levelKey]*level, len(table))

	// ponytail: one walk of every setting row per call, so each reconcile
	// costs a scan of the whole settings range. Read only the records above
	// the reconciled position, through an index by position, once the scan
	// takes more than 100 ms, a tenth of the one-second poll.
	for s, err := range r.AllSettings(ctx, entity.SettingKey{}) {
		if err != nil && !errors.Is(err, entity.ErrBadRow) {
			return nil, nil, err
		}

		sec, ok := x.lookup(s.Key)
		if !ok || !reads(sec, s) {
			continue
		}

		key := levelKey{
			scope: s.Scope,
		}

		// A per-thing row is kept under its thing and its key as the section
		// declares it; one whose key carries no thing is skipped, as above.
		if sec.scope.form == formPerThing {
			key.thing, s.Key, ok = splitThing(sec.prefix, s.Key)
			if !ok {
				continue
			}
		}

		at := levelOf(levels, sec, key)

		switch {
		case err != nil:
			at.refused = append(at.refused, &refusal{
				scope: s.Scope,
				key:   s.Key,
				fault: fault{
					rule: "the stored row does not decode",
				},
				kind: ErrInvalid,
			})
		case s.Schema > newest:
			at.refused = append(at.refused, &refusal{
				scope: s.Scope,
				key:   s.Key,
				fault: fault{
					rule: fmt.Sprintf(
						"schema %d is newer than this node reads, up to %d",
						s.Schema,
						newest,
					),
				},
				kind: ErrNewerSchema,
			})
		default:
			at.rows = append(at.rows, s)
		}
	}

	snap := &Snapshot{
		node:     node,
		sections: make(map[string]*entry, len(table)),
	}
	refused := make(map[string]error)

	for i := range table {
		sec := &table[i]

		e := build(sec, levels[sec], node)
		snap.sections[sec.prefix] = e

		if err := e.err(); err != nil {
			refused[sec.prefix] = err
		}
	}

	return snap, refused, nil
}

// reads reports whether sec keeps row s: s's scope kind is one of sec's and,
// at an OverrideOnly section's narrow scope, its key is a field the option
// names. Which node's rows enter the value is build's choice.
func reads(sec *Section, s entity.Setting) bool {
	kind := s.Scope.Kind

	switch {
	case kind == sec.scope.wide:
	case sec.scope.form == formOverride && kind == sec.scope.narrow:
		if sec.overrideOnly != nil && !slices.Contains(sec.overrideOnly, s.Key) {
			return false
		}
	default:
		return false
	}

	return true
}

// levelOf is sec's level at scope, made on first use.
func levelOf(levels map[*Section]map[levelKey]*level, sec *Section, key levelKey) *level {
	byScope, ok := levels[sec]
	if !ok {
		byScope = make(map[levelKey]*level)
		levels[sec] = byScope
	}

	at, ok := byScope[key]
	if !ok {
		at = &level{
			scope: key.scope,
		}
		byScope[key] = at
	}

	return at
}

// perInstance reports whether a scope kind holds one value per instance, a
// share, a namespace or a remote store, rather than one value for the node.
func perInstance(kind entity.ScopeKind) bool {
	return kind != entity.ScopeInstallation && kind != entity.ScopeNode
}

// build is sec's entry from its levels: the defaults, then the wide scope's
// rows, then the narrow scope's, then the required keys, then Validate.
func build(sec *Section, byScope map[levelKey]*level, node entity.NodeID) *entry {
	e := &entry{
		section: sec,
	}

	if sec.scope.form == formPerThing {
		return buildThings(sec, byScope, node)
	}

	narrow := sec.scope.narrow
	if sec.scope.form != formOverride {
		narrow = 0
	}

	if perInstance(sec.scope.wide) {
		e.base = finish(sec, outcome{value: sec.def}, nil, entity.SettingScope{
			Kind: sec.scope.wide,
		})
		e.instances = make(map[[16]byte]outcome)

		for key, at := range byScope {
			got := overlay(sec, outcome{value: sec.def}, at)
			e.instances[key.scope.ID] = finish(sec, got, at.rows, key.scope)
		}

		return e
	}

	wideScope := entity.SettingScope{
		Kind: sec.scope.wide,
	}
	if wideScope.Kind == entity.ScopeNode {
		wideScope.ID = node
	}

	wide := byScope[levelKey{scope: wideScope}]
	base := overlay(sec, outcome{value: sec.def}, wide)
	e.stored = wide != nil

	var wideRows []entity.Setting
	if wide != nil {
		wideRows = wide.rows
	}

	switch {
	case narrow == 0:
		e.base = finish(sec, base, wideRows, wideScope)
	case perInstance(narrow):
		e.base = finish(sec, base, wideRows, wideScope)
		e.instances = make(map[[16]byte]outcome)

		for key, at := range byScope {
			scope := key.scope
			if scope.Kind != narrow {
				continue
			}

			got := overlay(sec, base, at)
			e.instances[scope.ID] = finish(
				sec,
				got,
				append(slices.Clone(wideRows), at.rows...),
				scope,
			)
		}
	default:
		narrowScope := entity.SettingScope{
			Kind: narrow,
			ID:   node,
		}
		at := byScope[levelKey{scope: narrowScope}]
		got := overlay(sec, base, at)

		rows := slices.Clone(wideRows)
		if at != nil {
			rows = append(rows, at.rows...)
		}

		scope := wideScope
		if at != nil {
			scope = narrowScope
		}

		e.base = finish(sec, got, rows, scope)
	}

	return e
}

// buildThings is a per-thing section's entry: one value for each thing a row
// names, in each scope instance the section reads (node's own at node scope),
// each decoded over the defaults on its own, so a bad row stops only its
// thing. The base is the defaults, what a thing with no rows runs.
func buildThings(sec *Section, byScope map[levelKey]*level, node entity.NodeID) *entry {
	scope := entity.SettingScope{
		Kind: sec.scope.wide,
	}
	if scope.Kind == entity.ScopeNode {
		scope.ID = node
	}

	e := &entry{
		section: sec,
		base:    finish(sec, outcome{value: sec.def}, nil, scope),
		things:  make(map[thingAt]outcome),
	}

	for key, at := range byScope {
		if key.scope.Kind == entity.ScopeNode && key.scope.ID != node {
			continue
		}

		got := finish(sec, overlay(sec, outcome{value: sec.def}, at), at.rows, key.scope)
		nameThing(got.err, sec.prefix, key.thing)
		e.things[thingAt{
			id:    key.scope.ID,
			thing: key.thing,
		}] = got
	}

	return e
}

// nameThing rewrites the key of every refusal in err, made under the key a
// per-thing section declares, as the row stored it, with its thing, and
// rewrites an unknown key's hint the same way, so the line names the key the
// operator wrote.
func nameThing(err error, prefix, thing string) {
	switch err := err.(type) {
	case *refusal:
		key, quoted := err.key, false
		if unquoted, uerr := strconv.Unquote(key); uerr == nil {
			key, quoted = unquoted, true
		}

		if key == prefix {
			key += "." + thing
		} else {
			key = thingKey(prefix, key, thing)
		}

		if quoted {
			key = strconv.Quote(key)
		}

		err.key = key

		// An unknown key's hint names the nearest declared key; written for
		// the thing, it is a key the operator can write.
		const hint = "unknown key; did you mean "
		if near, ok := strings.CutPrefix(err.fault.rule, hint); ok {
			if near, ok = strings.CutSuffix(near, "?"); ok && strings.HasPrefix(near, prefix+".") {
				err.fault.rule = hint + thingKey(prefix, near, thing) + "?"
			}
		}
	case interface{ Unwrap() []error }:
		for _, e := range err.Unwrap() {
			nameThing(e, prefix, thing)
		}
	}
}

// overlay is at's rows decoded over base: base's refusal stands, and so do
// the rows of at refused before the decode, every one of them.
func overlay(sec *Section, base outcome, at *level) outcome {
	if base.err != nil || at == nil {
		return base
	}

	if len(at.refused) > 0 {
		return outcome{
			err: errors.Join(at.refused...),
		}
	}

	v, err := sec.decodeAny(sec, base.value, at.scope, at.rows)
	if err != nil {
		return outcome{
			err: err,
		}
	}

	return outcome{
		value: v,
	}
}

// finish refuses got for a required key no row of rows holds, each named in
// declaration order, and then for its Validate's refusal, printed under the
// section's prefix since Validate names no field. A refusal is never
// replaced by the defaults.
func finish(sec *Section, got outcome, rows []entity.Setting, scope entity.SettingScope) outcome {
	if got.err != nil {
		return got
	}

	var missing []error

	for _, key := range sec.required {
		held := slices.ContainsFunc(rows, func(s entity.Setting) bool {
			return s.Key == key
		})
		if !held {
			missing = append(missing, &refusal{
				scope: scope,
				key:   key,
				fault: fault{
					rule: "required and has no value",
				},
				kind: ErrRequired,
			})
		}
	}

	if len(missing) > 0 {
		return outcome{
			err: errors.Join(missing...),
		}
	}

	if err := got.value.(interface{ Validate() error }).Validate(); err != nil {
		return outcome{
			err: &refusal{
				scope: scope,
				key:   sec.prefix,
				fault: fault{
					rule: err.Error(),
				},
				kind: ErrInvalid,
			},
		}
	}

	return got
}

// err is e's one refusal as it is, or every refusal of e joined, the base's
// first and then each instance's in scope order, or nil.
//
// decision: a section with one value per scope instance, or per thing,
// reports only the refusals of values a stored row went into. Its base, the
// defaults when no row of the wide scope exists, is the value of an instance
// or a thing with no rows, and refusing it would report as refused a share
// or a journal no one configured, under an ID none has, while every
// configured one runs. The base's refusal still answers for such an
// instance or thing, so nothing runs on refused defaults.
func (e *entry) err() error {
	var errs []error
	if e.base.err != nil && (e.instances == nil && e.things == nil || e.stored) {
		errs = append(errs, e.base.err)
	}

	ids := make([][16]byte, 0, len(e.instances))
	for id := range e.instances {
		ids = append(ids, id)
	}

	slices.SortFunc(ids, func(a, b [16]byte) int {
		return bytes.Compare(a[:], b[:])
	})

	for _, id := range ids {
		// An instance with a refused base carries the base's own refusal,
		// already counted above.
		if err := e.instances[id].err; err != nil && err != e.base.err {
			errs = append(errs, err)
		}
	}

	things := slices.Collect(maps.Keys(e.things))
	slices.SortFunc(things, func(a, b thingAt) int {
		if c := bytes.Compare(a.id[:], b.id[:]); c != 0 {
			return c
		}

		return strings.Compare(a.thing, b.thing)
	})

	for _, k := range things {
		if err := e.things[k].err; err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) == 1 {
		return errs[0]
	}

	return errors.Join(errs...)
}

// Get is the section's value for node in snap, with no decode; an error for a
// section snap holds no value of, such as one held at start.
func Get[T any](snap *Snapshot, section string, node entity.NodeID) (T, error) {
	var zero T

	if snap == nil {
		return zero, fmt.Errorf("%w: section %s: no snapshot", ErrUnknownKey, section)
	}

	e, ok := snap.sections[section]
	if !ok {
		return zero, fmt.Errorf("%w: section %s is not in the snapshot", ErrUnknownKey, section)
	}

	scope := e.section.scope
	if scope.form == formPerThing || e.instances != nil || perInstance(scope.wide) {
		return zero, fmt.Errorf(
			"settings: section %s holds one value per instance, not one",
			section,
		)
	}

	if (scope.wide == entity.ScopeNode || scope.narrow == entity.ScopeNode) && node != snap.node {
		return zero, fmt.Errorf("settings: section %s: the snapshot is node %x's, not node %x's",
			section, snap.node, node)
	}

	if e.base.err != nil {
		return zero, e.base.err
	}

	v, ok := e.base.value.(T)
	if !ok {
		return zero, fmt.Errorf(
			"settings: section %s holds %T, not %T",
			section,
			e.base.value,
			zero,
		)
	}

	return clone(v), nil
}

// Stored reports whether a row of section's wide scope went into snap,
// refused or not: false for a section running on its defaults alone, or one
// snap does not hold.
func (s *Snapshot) Stored(section string) bool {
	if s == nil {
		return false
	}

	e, ok := s.sections[section]

	return ok && e.stored
}
