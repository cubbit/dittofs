package settings

import (
	"iter"
	"strconv"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Refusals is each refusal err joins, as the scope instance its refused value
// belongs to and the key it names, in the order err joins them: err is one
// section's refusal, the value of Read's refused map, one refusal or several
// joined. A key held quoted to print comes back unquoted, so PrefixOf answers
// it; a key cut by the print's bound stays cut, since no unquote opens it.
// An error that wraps none of this package's refusals names nothing. The walk
// of a snapshot reports the rows it read with these, so a refusal names the
// instances it refused and no other.
func Refusals(err error) iter.Seq2[entity.SettingScope, string] {
	return func(yield func(entity.SettingScope, string) bool) {
		var down func(error) bool

		down = func(err error) bool {
			switch value := err.(type) {
			case *refusal:
				key := value.key
				if raw, uerr := strconv.Unquote(key); uerr == nil {
					key = raw
				}

				return yield(value.scope, key)
			case interface{ Unwrap() []error }:
				for _, leaf := range value.Unwrap() {
					if !down(leaf) {
						return false
					}
				}
			}

			return true
		}

		down(err)
	}
}

// PrefixOf answers the prefix of the section that declares key: the one
// with the longest prefix equal to key or followed in it by a dot, as the
// reads and checks give the key to. An empty string for a key no section of
// the table declares, which Read skips.
//
// A report that names a section's rows answers every other key's section
// through this, so no nested or look-alike section's row goes in it: the
// longest prefix is the one rule, reused, not copied.
//
// ponytail: the index is built per call, so a walk that asks once per row
// rebuilds it per row. Take one index per walk, as Read does, once a table
// and a walk large enough make the rebuilds show up in a profile.
func (t Table) PrefixOf(key string) string {
	sec, ok := t.index().lookup(key)
	if !ok {
		return ""
	}

	return sec.prefix
}
