package store

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

// A test may write a weak key file.
func TestKey(t *testing.T) { _, _ = keyprovider.GenerateWeakKeyFile("p") }
