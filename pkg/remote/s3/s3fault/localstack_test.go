package s3fault

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// LocalStack, with its endpoint unset, fails under CI and skips outside it,
// naming the variable either way. Each case runs this test's own binary, so
// the verdict read is the one go test prints for the real testing.T.
func TestLocalStackUnset(t *testing.T) {
	if os.Getenv("S3FAULT_CHILD") == "1" {
		LocalStack(t)
		t.Fatal("LocalStack returned with LOCALSTACK_HTTPS_ENDPOINT unset")
	}
	for _, c := range []struct{ ci, want string }{
		{"true", "--- FAIL: TestLocalStackUnset"},
		{"", "--- SKIP: TestLocalStackUnset"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLocalStackUnset$", "-test.v")
		cmd.Env = append(os.Environ(), "S3FAULT_CHILD=1", "LOCALSTACK_HTTPS_ENDPOINT=", "CI="+c.ci)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), c.want) || !strings.Contains(string(out), "LOCALSTACK_HTTPS_ENDPOINT isn't set") {
			t.Errorf("CI=%q: output %q, want %q naming LOCALSTACK_HTTPS_ENDPOINT", c.ci, out, c.want)
		}
	}
}
