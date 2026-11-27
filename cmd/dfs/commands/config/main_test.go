package config

import (
	"os"
	"strings"
	"testing"
)

// TestMain drops the DITTOFS_ variables the developer's shell exports before
// any test runs: the validate command loads the bootstrap with the process
// environment, which refuses a name it does not know, so a stray one would
// fail a test for a reason the test does not name. A test that needs one sets
// it itself.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "DITTOFS_") {
			_ = os.Unsetenv(name)
		}
	}

	os.Exit(m.Run())
}
