package quota_test

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/commands/quota"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
)

// The shared suite's quota rows, each through a root built in this process,
// over a node the harness starts.
func TestQuotaRows(t *testing.T) {
	clisuite.Run(t, func(t *testing.T) clisuite.Runner {
		return clisuite.InProcess(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json")})
	}, clisuite.Start(t), "quota")
}

// quota set refuses a negative limit or grace, and a grace that is not whole
// seconds, before it reaches any server.
func TestQuotaSetRefusesLimits(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"--hard-bytes=-1", "--hard-bytes -1"},
		{"--soft-files=-5", "--soft-files -5"},
		{"--grace=-1s", "--grace -1s"},
		{"--grace=500ms", "--grace 500ms"},
	} {
		cmd := quota.New(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "config.json")})
		cmd.SetArgs([]string{"set", "archive", "--user", "alice", tc.flag})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("quota set %s: got %v, want a refusal naming %q", tc.flag, err, tc.want)
		}
	}
}
