// Package storetest is a package of test helpers, which may write a weak key file.
package storetest

import "github.com/cubbit/dittofs/pkg/keyprovider"

func Key() ([]byte, error) { return keyprovider.GenerateWeakKeyFile("p") }
