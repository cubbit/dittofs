package settings

import (
	"strings"
	"unicode"
)

// splitThing reads key in a per-thing section's form, <prefix>.<thing>.<rest>,
// and returns the thing's ID and the key as the section declares it,
// <prefix>.<rest>. The rest is never split again, so a name that already
// carries the thing, as adapters.<protocol>.<field> carries the adapter, is
// read once: its protocol is the thing. A key with no thing after the
// prefix, a thing with nothing after it, or a thing holding a control
// character is not split.
//
// decision: only a one-part prefix is supported: the thing's ID goes after
// the whole prefix, which for a one-part prefix is RFC 13 §3's "after its
// first part". A dotted per-thing prefix would need that rule, the ID after
// the name's first part, and no per-thing section declares one. Withdraw
// when a section declares a dotted per-thing prefix.
func splitThing(prefix, key string) (thing, declared string, ok bool) {
	rest, found := strings.CutPrefix(key, prefix+".")
	if !found {
		return "", "", false
	}

	thing, field, found := strings.Cut(rest, ".")
	if !found || thing == "" || field == "" || strings.ContainsFunc(thing, unicode.IsControl) {
		return "", "", false
	}

	return thing, prefix + "." + field, true
}

// thingKey is declared, a key a per-thing section declares, written for one
// thing: the form splitThing reads back.
func thingKey(prefix, declared, thing string) string {
	return prefix + "." + thing + declared[len(prefix):]
}
