package bootstrap

import "strings"

// renamedConfigKeys maps the dotted prefix of a config section that moved to
// the prefix that replaced it. Entries stay until a release nobody upgrades
// across still carried the old name.
var renamedConfigKeys = map[string]string{
	"blockstore.local": "blockstore.journal",
}

// renamedKey returns the key that replaced key, when key is under a section
// that moved. A renamed key is refused naming its replacement rather than as
// an unknown key: an equivalent still exists under the new name, and the
// operator believes the old one is in effect.
//
// The match ignores case, so a key written in another case still names its
// replacement.
func renamedKey(key string) (string, bool) {
	lower := strings.ToLower(key)

	for old, replacement := range renamedConfigKeys {
		if lower == old || strings.HasPrefix(lower, old+".") {
			return replacement + strings.TrimPrefix(lower, old), true
		}
	}

	return "", false
}
