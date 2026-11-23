package install

import "github.com/cubbit/dittofs/pkg/keyprovider"

// gen takes GenerateWeakKeyFile as a value, not in a call: the rule names it.
var gen = keyprovider.GenerateWeakKeyFile

func weak() ([]byte, error) { return gen("p") }
