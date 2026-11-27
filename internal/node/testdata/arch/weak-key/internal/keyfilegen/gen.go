package keyfilegen

import "github.com/cubbit/dittofs/internal/keyfile"

// gen writes a key file through keyfile.Generate rather than keyprovider's
// generators, which may ask for a weak one: the rule names it.
func gen() ([]byte, error) { return keyfile.Generate("p", false) }
