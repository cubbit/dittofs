package settings

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Change is one key whose effective value differs between two snapshots, at
// the scope instance whose value changed: the node's, a share's or another
// instance's. An Override a node may replace lists its value at the node's
// scope; an Override at share, namespace or remote store scope lists its wide
// value at the wide scope, the installation's, once; the reconcile applies
// such a change to every instance of the section, with or without a row of
// its own, since an instance whose own row moved to the same value is listed
// only there.
type Change struct {
	Section string
	Key     string
	Class   Class
	Scope   entity.SettingScope
}

// Diff lists the keys whose effective value differs from prev to cur, each
// with its section, its field's class and its scope instance, sections by
// prefix, then the section's own value, its instances by ID and its things by
// scope ID and thing, each in field declaration order. A nil prev, or a
// section or value prev holds no value of, makes every key of it a change; a
// value cur holds none of lists nothing, since there is no new value to
// apply. The base of a section with one value per instance or per thing is
// never listed, since it names no instance.
//
// An instance or a thing a snapshot holds no value of runs the
// section's base, so it is compared against the base, and an instance of an
// Override that runs the wide value on both sides, for a key, lists nothing
// at its own scope: the wide value's change is listed once, at the wide
// scope. Two snapshots cannot tell an instance whose last row was deleted
// from one that was removed, so a removed share, namespace, remote store or
// thing lists the keys that went back to the base; the reconcile, which
// knows what exists, discards those.
func Diff(prev, cur *Snapshot) []Change {
	if cur == nil {
		return nil
	}

	var out []Change

	for _, prefix := range slices.Sorted(maps.Keys(cur.sections)) {
		var was *entry
		if prev != nil {
			was = prev.sections[prefix]
		}

		out = append(out, diffEntry(was, cur.sections[prefix], cur.node)...)
	}

	return out
}

// diffEntry is the changes of one section from was, nil when the previous
// snapshot does not hold it, to e.
func diffEntry(was, e *entry, node entity.NodeID) []Change {
	sec := e.section

	var wasBase *outcome
	if was != nil {
		wasBase = &was.base
	}

	var out []Change

	// The base of a section with one value per instance or per thing is the
	// defaults, which no row changes, and it names no instance.
	if !perInstance(sec.scope.wide) && sec.scope.form != formPerThing {
		out = changes(sec, baseScope(sec, node), "", values{
			prev: wasBase,
			cur:  e.base,
		})
	}

	kind := sec.scope.wide
	if sec.scope.form == formOverride {
		kind = sec.scope.narrow
	}

	var wasInstances map[[16]byte]outcome
	if was != nil {
		wasInstances = was.instances
	}

	ids := slices.Collect(maps.Keys(e.instances))
	for id := range wasInstances {
		if _, ok := e.instances[id]; !ok {
			ids = append(ids, id)
		}
	}

	slices.SortFunc(ids, func(a, b [16]byte) int {
		return bytes.Compare(a[:], b[:])
	})

	for _, id := range ids {
		scope := entity.SettingScope{
			Kind: kind,
			ID:   id,
		}
		out = append(out, changes(sec, scope, "", instanceValues(wasBase, e.base,
			wasInstances, e.instances, id))...)
	}

	var wasThings map[thingAt]outcome
	if was != nil {
		wasThings = was.things
	}

	things := slices.Collect(maps.Keys(e.things))
	for k := range wasThings {
		if _, ok := e.things[k]; !ok {
			things = append(things, k)
		}
	}

	slices.SortFunc(things, func(a, b thingAt) int {
		if c := bytes.Compare(a.id[:], b.id[:]); c != 0 {
			return c
		}

		return strings.Compare(a.thing, b.thing)
	})

	for _, k := range things {
		scope := entity.SettingScope{
			Kind: sec.scope.wide,
			ID:   k.id,
		}
		out = append(out, changes(sec, scope, k.thing, instanceValues(wasBase, e.base,
			wasThings, e.things, k))...)
	}

	return out
}

// values is one value of a section on both sides of a diff, with the base
// each side runs where it holds no value of its own. A nil prev is no value.
type values struct {
	prev     *outcome
	cur      outcome
	prevBase *outcome
	curBase  *outcome
}

// instanceValues is the value of instance k on both sides, the side's base
// where that side holds no value of k.
func instanceValues[K comparable](
	wasBase *outcome,
	base outcome,
	was, cur map[K]outcome,
	k K,
) values {
	v := values{
		prev:     wasBase,
		cur:      base,
		prevBase: wasBase,
		curBase:  &base,
	}

	if got, ok := was[k]; ok {
		v.prev = &got
	}

	if got, ok := cur[k]; ok {
		v.cur = got
	}

	return v
}

// baseScope is the scope instance a section's own value is for: the node's
// for a section at node scope or one a node may override, the wide scope's
// otherwise.
func baseScope(sec *Section, node entity.NodeID) entity.SettingScope {
	scope := entity.SettingScope{
		Kind: sec.scope.wide,
	}
	if sec.scope.form == formOverride && sec.scope.narrow == entity.ScopeNode {
		scope.Kind = entity.ScopeNode
	}

	if scope.Kind == entity.ScopeNode {
		scope.ID = node
	}

	return scope
}

// changes is the keys of sec whose value differs in v, at scope, each written
// for thing when thing is not empty.
func changes(sec *Section, scope entity.SettingScope, thing string, v values) []Change {
	if v.cur.err != nil {
		return nil
	}

	cur, ok := fieldsOf(v.cur.value)
	every := !ok || v.prev == nil || v.prev.err != nil

	var prev, prevBase, curBase map[string]json.RawMessage
	if !every {
		prev, ok = fieldsOf(v.prev.value)
		every = !ok
	}

	tracks := !every && v.prevBase != nil && v.prevBase.err == nil && v.curBase.err == nil
	if tracks {
		prevBase, _ = fieldsOf(v.prevBase.value)
		curBase, _ = fieldsOf(v.curBase.value)
	}

	var out []Change

	for _, f := range sec.fields {
		// The field's json name is what the value's object holds it under;
		// a selfkey section's key is the prefix, which has no suffix to cut.
		name := f.name

		if !every && bytes.Equal(prev[name], cur[name]) {
			continue
		}

		if tracks && bytes.Equal(prev[name], prevBase[name]) &&
			bytes.Equal(cur[name], curBase[name]) {
			continue
		}

		key := f.key
		if thing != "" {
			key = thingKey(sec.prefix, f.key, thing)
		}

		out = append(out, Change{
			Section: sec.prefix,
			Key:     key,
			Class:   f.class,
			Scope:   scope,
		})
	}

	return out
}

// fieldsOf is v's fields by json name, each as json encodes it, the names
// Of reads a section's keys from, embedded structs flattened as json does.
//
// Two values of a field are equal when json encodes them to the same bytes,
// which a value decoded from a stored row always can; a value json cannot
// encode compares as changed in every field, since listing a key that did not
// change costs a reconfigure and missing one leaves a component running a
// value the operator replaced. A field type whose MarshalJSON drops
// information would hide a change of that information. json encodes a nil
// slice or map as null and an empty one as [] or {}, so the two compare as a
// change.
func fieldsOf(v any) (map[string]json.RawMessage, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}

	return fields, true
}

// Keep is the receiver with section's values taken from prev, so a refused
// section keeps what it last ran, and the rest from the receiver; a nil
// receiver holds nothing else. With a nil prev, or one that holds no value
// of section, the result holds none either, so Get on it is an error and
// never the defaults (RFC 13 §6). Neither snapshot is changed: prev is what
// the running components still read. In a section with one value per instance
// or per thing, only an instance or thing the receiver refuses takes prev's
// value, so the others keep their new one; the whole section takes prev's
// only when the section's own value is refused, since then every instance is.
// One prev does not hold keeps the receiver's outcome, a refusal staying a
// refusal, since prev's base would hand it the defaults.
func (s *Snapshot) Keep(section string, prev *Snapshot) *Snapshot {
	out := &Snapshot{
		sections: make(map[string]*entry),
	}

	var cur *entry

	switch {
	case s != nil:
		out.node = s.node
		maps.Copy(out.sections, s.sections)
		cur = s.sections[section]
	case prev != nil:
		out.node = prev.node
	}

	delete(out.sections, section)

	if prev != nil {
		if e, ok := prev.sections[section]; ok {
			out.sections[section] = keepEntry(e, cur)
		}
	}

	return out
}

// keepEntry is the entry Keep leaves for a refused section. A section with one
// value, or one whose own value is refused (then every instance is), is prev's
// entry with every instance and thing of cur that prev does not hold added as
// cur has it. In a section with one value per instance or per thing whose own
// value is valid, it is cur's entry with prev's value put back only where cur's
// is refused. prev's own entry is not changed.
func keepEntry(prev, cur *entry) *entry {
	if cur == nil {
		return prev
	}

	if cur.instances == nil && cur.things == nil || cur.stored && cur.base.err != nil {
		e := *prev
		e.instances = merged(prev.instances, cur.instances)
		e.things = merged(prev.things, cur.things)

		return &e
	}

	e := *cur
	e.instances = keptRefused(prev.instances, cur.instances)
	e.things = keptRefused(prev.things, cur.things)

	return &e
}

// keptRefused is cur with each refused value prev holds replaced by prev's.
func keptRefused[K comparable](prev, cur map[K]outcome) map[K]outcome {
	out := maps.Clone(cur)

	for k, got := range cur {
		if was, ok := prev[k]; ok && got.err != nil {
			out[k] = was
		}
	}

	return out
}

// merged is a copy of prev with every key of cur that prev lacks; prev as it
// is when cur adds none.
func merged[K comparable](prev, cur map[K]outcome) map[K]outcome {
	var out map[K]outcome

	for k, got := range cur {
		if _, ok := prev[k]; ok {
			continue
		}

		if out == nil {
			out = maps.Clone(prev)
			if out == nil {
				out = make(map[K]outcome, len(cur))
			}
		}

		out[k] = got
	}

	if out == nil {
		return prev
	}

	return out
}
