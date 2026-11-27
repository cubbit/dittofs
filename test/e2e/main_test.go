//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/cubbit/dittofs/test/e2e/helpers"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// TestMain builds dfs and starts the one server the package's tests run
// against, serving NFS without mounting it: each test mounts the shares it
// makes.
func TestMain(m *testing.M) {
	os.Exit(helpers.Run(m, registry.Fixture{Server: "dfs", Profile: "memory", NFS: registry.NFS{Version: "export"}}))
}
