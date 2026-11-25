package store

import "github.com/cubbit/dittofs/pkg/keyprovider"

// Key writes a weak key file outside a test, and again names it as a value:
// the rule names the file once.
func Key() ([]byte, error) { return keyprovider.GenerateWeakKeyFile("p") }

var again = keyprovider.GenerateWeakKeyFile

// Strong writes a default one, which the rule allows.
func Strong() ([]byte, error) { return keyprovider.GenerateKeyFile("p") }
