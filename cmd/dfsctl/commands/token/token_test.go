package token_test

import (
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
)

// The shared suite's token rows, each through a root built in this process,
// over a node the harness starts, reading the time from the node's clock as
// a client on a server's host would.
func TestTokenRows(t *testing.T) {
	s := clisuite.Start(t)
	clisuite.Run(t, func(t *testing.T) clisuite.Runner {
		return clisuite.InProcess(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json"), Now: s.Harness.Clock().Now})
	}, s, "token")
}
