package operation_test

import (
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
)

// The shared suite's operation rows, each through a root built in this
// process, over a node the harness starts.
func TestOperationRows(t *testing.T) {
	clisuite.Run(t, func(t *testing.T) clisuite.Runner {
		return clisuite.InProcess(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json")})
	}, clisuite.Start(t), "operation")
}
