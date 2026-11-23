//go:build ad_dc

package ad_dc_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// alice's ticket through the composed node, against this fixture's domain
// controller. The test itself, internal/node's TestSambaTicketThroughTheNode,
// lives in internal/node, since only that package's export_test.go reaches
// the node's filesystem service, which signs a ticket in; this one starts the
// controller and runs it there, handing over the controller's address, CA
// and service keytab.
//
// decision: a go test run of another package rather than a call, since a
// test cannot import another package's test files; the run compiles
// internal/node's tests, about a minute per run. Overturned once the
// node serves a ticket over an adapter a client here can reach.
func TestNodeSignIn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping AD-DC integration test")
	}
	dcIP := setupADDC(t)
	const test = "TestSambaTicketThroughTheNode"
	// go test runs a test in its package's directory, three below the module.
	cmd := exec.CommandContext(t.Context(), "go", "test", "-tags=ad_dc", "-count=1", "-v", "-run=^"+test+"$", "./internal/node/")
	cmd.Dir = filepath.Join("..", "..", "..")
	cmd.Env = append(os.Environ(), "DITTOFS_AD_DC_IP="+dcIP, "DITTOFS_AD_DC_CA="+copyADCA(t), "DITTOFS_AD_DC_KEYTAB="+copyADKeytab(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in internal/node: %v\n%s", test, err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+test) {
		t.Fatalf("%s in internal/node did not pass, want it run and passed:\n%s", test, out)
	}
	t.Logf("%s", out)
}
