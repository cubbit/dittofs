//go:build ad_dc

// Package ad_dc_test provides integration tests against a real Samba Active
// Directory Domain Controller in Docker: NETLOGON pass-through, online join and
// LDAP lookups. This file holds the DC fixture the tests share.
//
// Run with: go test -tags=ad_dc -v -timeout 20m ./test/integration/ad-dc/
// Requires: Docker (the samba-ad-dc image builds + provisions under linux/amd64
// emulation on Apple Silicon, which is slow — hence the generous timeout).
package ad_dc_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	adImageName = "dittofs-ad-dc-test"

	// adExecTimeout bounds every `docker exec` the fixture runs against the DC.
	// A bare exec.Command has no deadline, so a call issued while the container is
	// going away blocks until the Docker API call itself gives up — turning a
	// fixture teardown into a hung test run instead of a diagnosable failure.
	adExecTimeout = 2 * time.Minute

	adRealm  = "DITTOFS.AD"
	adDomain = "DITTOFS"

	adUserAlice = "alice"
	adUserPass  = "TestPassword01!"
)

// adContainerName is the container the current test's fixture is running in. It
// is set by setupADDC/setupADDCForLDAP to a name derived from the test, so two
// tests never address the same container and one test's teardown cannot reach
// another's DC.
//
// decision: this is package-level mutable state, which is safe only because the
// tests in this package run sequentially (none calls t.Parallel). It is set once
// per test before any helper reads it. It would need to become a field threaded
// through the helpers if these tests ever run in parallel.
var adContainerName = "dittofs-ad-dc-test"

// uniqueContainerName derives a Docker container name unique to t, so a fixture
// is never shared between tests. Docker names allow only [a-zA-Z0-9][a-zA-Z0-9_.-]*,
// so the test name is lowercased and every other byte is replaced with '-'.
func uniqueContainerName(t *testing.T) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, t.Name())
	return adImageName + "-" + safe
}

// adDockerOutput runs `docker <sub> args...` bounded by adExecTimeout and returns
// its stdout. Used for the fixture's introspection calls (inspect, port) whose
// output is parsed, so Docker's stderr must not be folded into it.
func adDockerOutput(sub string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adExecTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", append([]string{sub}, args...)...).Output()
}

// adDockerCombined runs `docker <sub> args...` bounded by adExecTimeout and
// returns stdout+stderr together, so a failure message keeps Docker's diagnostic
// rather than only the exit status.
func adDockerCombined(sub string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adExecTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", append([]string{sub}, args...)...).CombinedOutput()
}

// adDockerExec runs `docker exec <container> args...` with a bounded context and
// returns its combined output. Every exec the fixture issues goes through here so
// none can block past adExecTimeout, and a failure keeps Docker's stderr.
func adDockerExec(args ...string) ([]byte, error) {
	return adDockerCombined("exec", append([]string{adContainerName}, args...)...)
}

// AD-DC Docker Setup

// setupADDC builds the Samba AD-DC image, runs it (letting its own entrypoint
// provision the domain), waits for the KDC to accept connections, and copies
// the exported keytab + a host-side krb5.conf pointing at the mapped KDC port.
func setupADDC(t *testing.T) (kdcHostPort int, keytabPath, krb5ConfPath string, cleanup func()) {
	t.Helper()

	tmpDir := t.TempDir()

	// Address a container private to this test so no other test's teardown can
	// remove the DC out from under an in-flight docker exec.
	container := uniqueContainerName(t)
	adContainerName = container

	// Register the teardown before the container exists, capturing this test's
	// name, so a t.Fatalf anywhere below (port discovery, readiness, keytab copy)
	// cannot orphan the container — sibling tests use different names and their
	// cleanup cannot reclaim it.
	t.Cleanup(func() {
		t.Log("Cleaning up AD-DC container...")
		_ = exec.Command("docker", "rm", "-f", container).Run()
	})

	// Clean up any previous container left by an interrupted run of this test.
	_ = exec.Command("docker", "rm", "-f", adContainerName).Run()

	dockerfileDir := findADDockerfileDir(t)

	// Build the image (cached after first run). Force linux/amd64: samba-ad-dc
	// has no native arm64 path in our base and runs fine under emulation.
	t.Log("Building Samba AD-DC image (slow on first run)...")
	buildCmd := exec.Command("docker", "build",
		"--platform", "linux/amd64",
		"-t", adImageName, dockerfileDir,
	)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("docker build failed: %v", err)
	}

	// Run the container with the AD-DC entrypoint (it provisions the domain,
	// creates users/groups, and exports the keytab to /keytabs on its own).
	t.Log("Starting AD-DC container...")
	// --privileged is required for `samba-tool domain provision`: setting the NT
	// ACL on the sysvol share (smbd.set_nt_acl) is denied on an overlayfs-backed
	// container filesystem (e.g. Docker Desktop on macOS), aborting provisioning.
	// CI's overlay2-on-ext4 happens to allow it unprivileged, but requiring it
	// keeps the fixture portable across developer machines (issue #1252).
	runOut, err := exec.Command("docker", "run", "-d",
		"--platform", "linux/amd64",
		"--privileged",
		"--name", adContainerName,
		"-p", "0:88/tcp",
		adImageName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, runOut)
	}

	// Discover the randomly assigned host port for the KDC.
	portOut, err := adDockerOutput("port", adContainerName, "88/tcp")
	if err != nil {
		dumpADLogs(t)
		t.Fatalf("docker port: %v", err)
	}
	portStr := strings.TrimSpace(string(portOut))
	parts := strings.Split(portStr, ":")
	kdcHostPort, err = strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	t.Logf("AD-DC KDC mapped to host port %d", kdcHostPort)

	// Wait for the KDC TCP port to come up. Provisioning a fresh AD domain
	// under emulation is slow, so allow several minutes.
	t.Log("Waiting for KDC TCP port...")
	waitForADPort(t, kdcHostPort, 6*time.Minute)

	// The samba that served the setup keeps running, so the DC stays up from here.
	t.Log("Waiting for the AD-DC setup to finish...")
	waitForADReady(t, 4*time.Minute)

	// Copy the keytab out of the container. docker cp reads as root inside the
	// container, so the fixture's chmod 0400 / chown to the dittofs uid does
	// not block us (it would block a host volume mount).
	keytabPath = filepath.Join(tmpDir, "dittofs.keytab")
	if out, err := adDockerCombined("cp",
		adContainerName+":/keytabs/dittofs.keytab", keytabPath); err != nil {
		dumpADLogs(t)
		t.Fatalf("copy keytab: %v\n%s", err, out)
	}

	// Write a host-side krb5.conf that points the realm KDC at the mapped host
	// port (force TCP, the mapped port is TCP only). The Samba-generated
	// krb5.conf in the container references the DC hostname, which the host
	// cannot resolve — so we synthesise a minimal one.
	krb5ConfPath = filepath.Join(tmpDir, "krb5.conf")
	hostKrb5Conf := fmt.Sprintf(`[libdefaults]
    default_realm = %s
    dns_lookup_realm = false
    dns_lookup_kdc = false
    udp_preference_limit = 1
    rdns = false

[realms]
    %s = {
        kdc = 127.0.0.1:%d
    }
`, adRealm, adRealm, kdcHostPort)
	if err := os.WriteFile(krb5ConfPath, []byte(hostKrb5Conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}

	cleanup = func() {
		t.Log("Cleaning up AD-DC container...")
		_ = exec.Command("docker", "rm", "-f", container).Run()
	}
	return kdcHostPort, keytabPath, krb5ConfPath, cleanup
}

func findADDockerfileDir(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("Dockerfile"); err == nil {
		return "."
	}
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "test", "integration", "ad-dc")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find project root (go.mod)")
		}
		dir = parent
	}
}

// waitForADReady polls until the entrypoint writes its ready marker, the last
// step of its setup: the users, groups and complete keytab exist and the KDC,
// LDAP and DNS accept connections. A failed setup stops the container, which
// fails the test at once instead of at the timeout.
func waitForADReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := adDockerExec("test", "-f", "/keytabs/.ready"); err == nil {
			return
		}
		if out, _ := adDockerOutput("inspect", "-f", "{{.State.Running}}", adContainerName); strings.TrimSpace(string(out)) == "false" {
			dumpADLogs(t)
			t.Fatal("AD-DC container stopped before its setup finished")
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("AD-DC setup not finished within %s", timeout)
}

func waitForADPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 1*time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
	dumpADLogs(t)
	t.Fatalf("KDC port %d not ready within %s", port, timeout)
}

// lookupGroupSID queries the running DC for a group's object SID via samba-tool.
func lookupGroupSID(t *testing.T, group string) string {
	t.Helper()
	out, err := adDockerExec("samba-tool", "group", "show", group, "--attributes=objectSid")
	if err != nil {
		t.Fatalf("samba-tool group show %s: %v\n%s", group, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "objectsid:") {
			sid := strings.TrimSpace(line[len("objectsid:"):])
			if sid != "" {
				return sid
			}
		}
	}
	t.Fatalf("could not parse objectSid for group %s from:\n%s", group, out)
	return ""
}

func dumpADLogs(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adExecTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker",
		"logs", "--tail", "100", adContainerName).CombinedOutput()
	t.Logf("---- AD-DC container logs (tail) ----\n%s\n-------------------------------------", out)
}
