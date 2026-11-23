package store

import "github.com/cubbit/dittofs/pkg/keyprovider"

// Key writes a weak key file outside a test: the rule names it.
func Key() ([]byte, error) { return keyprovider.GenerateWeakKeyFile("p") }

// Strong writes a default one, which the rule allows.
func Strong() ([]byte, error) { return keyprovider.GenerateKeyFile("p") }
