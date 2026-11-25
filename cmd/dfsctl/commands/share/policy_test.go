package share

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// export-policy update with no field flag is refused rather than writing
// nothing and printing the policy as though it had changed.
func TestPolicyUpdateNeedsAField(t *testing.T) {
	cmd := New(cmdutil.Env{ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	cmd.SetArgs([]string{"export-policy", "update", "archive"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "at least one of the flags") {
		t.Errorf("export-policy update with no field flag: got %v, want a refusal naming the field flags", err)
	}
}
