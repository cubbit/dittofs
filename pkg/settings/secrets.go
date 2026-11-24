package settings

import (
	"bytes"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// SecretUse is one reference to a secret a snapshot holds: the section whose
// value carries it, the setting key the section declares the reference
// under, the scope instance whose value holds it, and the kind of secret the
// section declares that key names.
type SecretUse struct {
	Section string
	Key     string
	Scope   entity.SettingScope
	Kind    entity.SecretKind
	Ref     entity.SecretRef
}

// Secrets is every reference the snapshot holds, the base values and each
// instance's and thing's alike, one use per scope instance that holds a
// non-zero reference, sections by prefix and each section's keys in
// declaration order. A value a section refused names no reference: it runs
// nothing until the walk reads it whole again.
//
// decision: the scope is the one the value's instance runs at, as Diff also
// gives it, not the row that set the field, which is the owner the write
// check sealed: today every declared reference sits in a section with one
// value for the whole scope, so the two are the same owner. Read the row that
// set the field once a reference lands in an Override's chain, where a wider
// row can set a field of the value a narrower instance runs.
func (s *Snapshot) Secrets() []SecretUse {
	if s == nil {
		return nil
	}

	var out []SecretUse

	for _, prefix := range slices.Sorted(maps.Keys(s.sections)) {
		out = append(out, s.sections[prefix].secrets(s.node)...)
	}

	return out
}

// secrets is one section's references, the base value it holds for the whole
// scope first, then each instance's and thing's, each key in declaration
// order.
func (e *entry) secrets(node entity.NodeID) []SecretUse {
	sec := e.section

	var out []SecretUse

	if !perInstance(sec.scope.wide) && sec.scope.form != formPerThing {
		out = append(out, usesOf(sec, baseScope(sec, node), "", e.base.value, e.base.err)...)
	}

	ids := make([][16]byte, 0, len(e.instances))
	for id := range e.instances {
		ids = append(ids, id)
	}

	slices.SortFunc(ids, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })

	for _, id := range ids {
		at := e.instances[id]

		scope := entity.SettingScope{
			Kind: sec.scope.wide,
			ID:   id,
		}
		if sec.scope.form == formOverride {
			scope.Kind = sec.scope.narrow
		}

		out = append(out, usesOf(sec, scope, "", at.value, at.err)...)
	}

	out = append(out, thingsOf(sec, e.things)...)

	return out
}

// thingsOf is a per-thing section's references, each thing's rows in scope
// and thing order.
func thingsOf(sec *Section, things map[thingAt]outcome) []SecretUse {
	keys := make([]thingAt, 0, len(things))
	for k := range things {
		keys = append(keys, k)
	}

	slices.SortFunc(keys, func(a, b thingAt) int {
		if c := bytes.Compare(a.id[:], b.id[:]); c != 0 {
			return c
		}

		return strings.Compare(a.thing, b.thing)
	})

	var out []SecretUse

	for _, k := range keys {
		scope := entity.SettingScope{
			Kind: sec.scope.wide,
			ID:   k.id,
		}
		got := things[k]
		out = append(out, usesOf(sec, scope, k.thing, got.value, got.err)...)
	}

	return out
}

// usesOf is each reference value holds, one use per declared key naming a
// non-zero one; a refused value names none.
func usesOf(
	sec *Section,
	scope entity.SettingScope,
	thing string,
	value any,
	err error,
) []SecretUse {
	if err != nil {
		return nil
	}

	var out []SecretUse

	for _, f := range sec.fields {
		kind, named := sec.secretRefs[f.key]
		if !named {
			continue
		}

		ref, ok := refOf(value, f.index)
		if !ok || ref == (entity.SecretRef{}) {
			continue
		}

		key := f.key
		if thing != "" {
			key = thingKey(sec.prefix, f.key, thing)
		}

		out = append(out, SecretUse{
			Section: sec.prefix,
			Key:     key,
			Scope:   scope,
			Kind:    kind,
			Ref:     ref,
		})
	}

	return out
}
