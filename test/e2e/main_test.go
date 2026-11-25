//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/cubbit/dittofs/test/e2e/helpers"
)

// TestMain builds dfs and starts the one server the package's tests run
// against.
func TestMain(m *testing.M) { os.Exit(helpers.Run(m)) }
