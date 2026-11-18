package settings

import (
	"fmt"
	"strings"
)

// index is a table's sections by prefix, built once per Read or Check so that
// the table is checked once per call and lookup costs one map read per dot
// of the key.
type index struct {
	sections map[string]*Section
}

// index checks t and returns its sections by prefix. It panics on two
// sections that declare one prefix, and on a key one section declares that
// lookup would give to another, as an identity section's field oidc beside an
// identity.oidc section, which no write or read could reach.
func (t Table) index() index {
	x := index{sections: make(map[string]*Section, len(t))}

	for i := range t {
		sec := &t[i]
		if first, dup := x.sections[sec.prefix]; dup {
			panic(fmt.Sprintf("settings: sections %s (%v) and %s (%v) declare one prefix",
				first.prefix, first.typ, sec.prefix, sec.typ))
		}

		x.sections[sec.prefix] = sec
	}

	for i := range t {
		sec := &t[i]
		for _, f := range sec.fields {
			if owner, _ := x.lookup(f.key); owner != sec {
				panic(fmt.Sprintf("settings: section %s (%v) declares key %s, "+
					"which section %s (%v) takes",
					sec.prefix, sec.typ, f.key, owner.prefix, owner.typ))
			}
		}
	}

	return x
}

// lookup returns the section key belongs to: the one with the longest prefix
// equal to key or followed in it by a dot.
func (x index) lookup(key string) (*Section, bool) {
	for k := key; ; {
		if sec, ok := x.sections[k]; ok {
			return sec, true
		}

		dot := strings.LastIndexByte(k, '.')
		if dot < 0 {
			return nil, false
		}

		k = k[:dot]
	}
}
