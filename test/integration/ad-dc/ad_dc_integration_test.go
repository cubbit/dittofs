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
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// adExecTimeout bounds every `docker exec` the fixture runs against the DC.
	// A bare exec.Command has no deadline, so a call issued while the container is
	// going away blocks until the Docker API call itself gives up — turning a
	// fixture teardown into a hung test run instead of a diagnosable failure.
	adExecTimeout = 2 * time.Minute

	adRealm  = "DITTOFS.AD"
	adDomain = "DITTOFS"

	// adDCHostname is the DC's fixed host name, given to its container, and
	// adDCHost its DNS name, which the DC's TLS certificate names in a SAN.
	adDCHostname = "dc1"
	adDCHost     = adDCHostname + ".dittofs.ad"

	adUserAlice = "alice"
	adUserPass  = "TestPassword01!"

	// The SMB and NFS service principals, in the form gokrb5 requests a ticket
	// for (no realm). The fixture puts both on an AES-only service account and
	// exports their keys into dittofs.keytab.
	adSMBSPNShort = "cifs/dittofs.dittofs.ad"
	adNFSSPNShort = "nfs/dittofs.dittofs.ad"

	// The domain administrator, whose bind the online join uses.
	adAdminPassword = "Passw0rd!2024"
	adBindDN        = "CN=Administrator,CN=Users,DC=dittofs,DC=ad"
	adBaseDN        = "DC=dittofs,DC=ad"
)

// adContainerName is the container the current test's fixture is running in. It
// is set by setupADDC to a name derived from the test, so two
// tests never address the same container and one test's teardown cannot reach
// another's DC.
//
// decision: this is package-level mutable state, which is safe only because the
// tests in this package run sequentially (none calls t.Parallel). It is set once
// per test before any helper reads it. It would need to become a field threaded
// through the helpers if these tests ever run in parallel.
var adContainerName string

// adNamePrefix starts the name of every image, container and network the
// fixture makes, and is the key of the label they carry. Setting
// DITTOFS_AD_DC_NAME_PREFIX replaces it on a host whose Docker names are shared
// between users. It is lowercased because Docker image names must be.
var adNamePrefix = strings.ToLower(cmp.Or(os.Getenv("DITTOFS_AD_DC_NAME_PREFIX"), "dittofs-ad-dc-test"))

// adRunID tells this run's containers and networks apart from those of any
// other run on the same Docker host, so two runs never remove each other's DC.
var adRunID = strings.ToLower(rand.Text()[:8])

// adImage is the DC image, built once per run by the first setupADDC and
// removed by TestMain when the run ends. Its tag is this run's own, so a
// concurrent run's build cannot retag it between this run's build and its
// docker run.
var adImage = adNamePrefix + ":" + adRunID

var (
	buildADImageOnce sync.Once
	adImageErr       error
	adImageBuilt     bool
)

// TestMain removes the run's DC image once every test is done with it. Removing
// it per test would also drop the legacy builder's cache, its untagged parent
// images, so every test would rebuild the image from scratch.
func TestMain(m *testing.M) {
	code := m.Run()
	if adImageBuilt {
		if out, err := adDockerCombined("rmi", adImage); err != nil {
			fmt.Fprintf(os.Stderr, "rm AD-DC image %s: %v\n%s", adImage, err, out)
		}
	}
	os.Exit(code)
}

// adStaleAge is how old a container or labelled network must be before
// a later run removes it as left behind by a run that was killed before its
// t.Cleanup ran. It is well above the suite's 20-minute timeout, so a run still
// in flight on the same host keeps its DC.
const adStaleAge = time.Hour

var sweepADLeftoversOnce sync.Once

// sweepADLeftovers removes what earlier runs under the same name prefix left
// behind: a go test timeout panic, Ctrl-C or SIGKILL skips t.Cleanup, and each
// leaked network holds one of Docker's few default bridge subnets. Containers
// are matched by name rather than label, so those of a fixture that labelled
// none go too, and go first, since Docker refuses to remove a network still in
// use. Failures are only logged: a leftover that stays costs a subnet, not this
// run.
//
// decision: images are not pruned. A fully cached build keeps the Created time
// of its layers, so no age filter tells a live run's freshly tagged image from a
// leftover, and pruning one between its build and its docker run fails that
// run. A killed run therefore leaves its image tag behind; a cached build points
// it at the image the next run builds anyway, so it costs a tag rather than
// disk until the Dockerfile changes.
func sweepADLeftovers(t *testing.T) {
	t.Helper()
	// Docker's name filter is an unanchored pattern; staleADContainers checks
	// the prefix itself.
	out, err := adDockerOutput("ps", "-a", "--filter", "name="+adNamePrefix+"-",
		"--format", "{{.ID}}\t{{.Names}}\t{{.CreatedAt}}")
	if err != nil {
		t.Logf("list leftover AD-DC containers: %v", err)
		return
	}
	if ids := staleADContainers(string(out), adNamePrefix+"-", time.Now(), adStaleAge); len(ids) > 0 {
		t.Logf("Removing %d AD-DC container(s) left by killed runs", len(ids))
		if out, err := adDockerCombined("rm", append([]string{"-f"}, ids...)...); err != nil {
			t.Logf("remove leftover AD-DC containers: %v\n%s", err, out)
		}
	}
	until := "until=" + adStaleAge.String()
	if out, err := adDockerCombined("network", "prune", "-f", "--filter", "label="+adNamePrefix, "--filter", until); err != nil {
		t.Logf("prune leftover AD-DC networks: %v\n%s", err, out)
	}
}

// staleADContainers returns the IDs in `docker ps --format
// '{{.ID}}\t{{.Names}}\t{{.CreatedAt}}'` output whose container's name starts
// with namePrefix and which was created more than maxAge before now. A line
// whose fields or time do not parse is left out, so an unexpected format never
// removes a DC.
func staleADContainers(psOut, namePrefix string, now time.Time, maxAge time.Duration) []string {
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(psOut), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !strings.HasPrefix(fields[1], namePrefix) {
			continue
		}
		id, created := fields[0], fields[2]
		at, err := time.Parse("2006-01-02 15:04:05 -0700 MST", created)
		if err == nil && now.Sub(at) > maxAge {
			ids = append(ids, id)
		}
	}
	return ids
}

// uniqueContainerName derives a Docker container name unique to t and to this
// run, so a fixture is never shared between tests or between concurrent runs.
// Docker names allow only [a-zA-Z0-9][a-zA-Z0-9_.-]*, so the test name is
// lowercased and every other byte is replaced with '-'.
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
	return adNamePrefix + "-" + adRunID + "-" + safe
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

// setupADDC runs the run's Samba AD-DC image, building it on first use, on a
// Docker network of its own, letting its entrypoint provision the domain, and
// returns the DC's IP on that network. t.Cleanup removes the container and
// network.
//
// The DC is reached on its container IP, with no port published on the host,
// so a run never takes a host port another run or user holds, and NETLOGON's
// endpoint mapper (port 135) can send clients to dynamic RPC ports without
// NAT. That needs a container IP the host can route to: true of a Linux Docker
// host, not of Docker Desktop, where setupADDC fails rather than skips.
func setupADDC(t *testing.T) (dcIP string) {
	t.Helper()

	sweepADLeftoversOnce.Do(func() { sweepADLeftovers(t) })

	// Address a container private to this test so no other test's teardown can
	// remove the DC out from under an in-flight docker exec.
	container := uniqueContainerName(t)
	network := container + "-net"
	label := adNamePrefix + "=" + adRunID
	adContainerName = container

	// Register the teardown before the container exists, so a t.Fatalf anywhere
	// below cannot orphan the container or its network. The network goes last:
	// Docker refuses to remove it while a container uses it.
	t.Cleanup(func() {
		t.Log("Cleaning up AD-DC container...")
		if out, err := adDockerCombined("rm", "-f", container); err != nil {
			t.Logf("rm AD-DC container: %v\n%s", err, out)
		}
		if out, err := adDockerCombined("network", "rm", network); err != nil {
			t.Logf("rm AD-DC network: %v\n%s", err, out)
		}
	})

	// Build the image once per run, cached by Docker across runs. Force
	// linux/amd64: samba-ad-dc has no native arm64 path in our base and runs
	// fine under emulation.
	buildADImageOnce.Do(func() {
		t.Log("Building Samba AD-DC image (slow on first run)...")
		buildCmd := exec.Command("docker", "build",
			"--platform", "linux/amd64",
			"--label", label,
			"-t", adImage, findADDockerfileDir(t),
		)
		buildCmd.Stdout = os.Stdout
		buildCmd.Stderr = os.Stderr
		adImageErr = buildCmd.Run()
		adImageBuilt = adImageErr == nil
	})
	if adImageErr != nil {
		t.Fatalf("docker build failed: %v", adImageErr)
	}

	if out, err := adDockerCombined("network", "create", "--label", label, network); err != nil {
		t.Fatalf("docker network create %s: %v\n%s", network, err, out)
	}

	// Run the container with the AD-DC entrypoint (it provisions the domain,
	// creates users/groups, and exports the keytab to /keytabs on its own).
	t.Log("Starting AD-DC container...")
	// --privileged is required for `samba-tool domain provision`: setting the NT
	// ACL on the sysvol share (smbd.set_nt_acl) is denied on an overlayfs-backed
	// container filesystem (e.g. Docker Desktop on macOS), aborting provisioning.
	// CI's overlay2-on-ext4 happens to allow it unprivileged, but requiring it
	// keeps the fixture portable across developer machines (issue #1252).
	if out, err := adDockerCombined("run", "-d",
		"--platform", "linux/amd64",
		"--privileged",
		"--name", container,
		"--label", label,
		"--network", network,
		"--hostname", adDCHostname,
		"-e", fmt.Sprintf("RPC_PORT_RANGE=%d-%d", adRPCPortLow, adRPCPortHigh),
		adImage,
	); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, out)
	}

	// The ready marker is written once the KDC, LDAP, DNS and the RPC endpoint
	// mapper accept connections, so no separate wait on a port is needed. Provisioning a
	// fresh domain under emulation is slow.
	t.Log("Waiting for the AD-DC setup to finish...")
	waitForADReady(t, 10*time.Minute)

	dcIP = getContainerIP(t)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(dcIP, "389"), 5*time.Second)
	if err != nil {
		t.Fatalf("DC at %s is not reachable from this host (%v); the fixture needs a Linux Docker host that routes to container IPs, not Docker Desktop", dcIP, err)
	}
	_ = conn.Close()
	t.Logf("AD-DC reachable at %s on network %s", dcIP, network)
	return dcIP
}

// getContainerIP returns the IP of adContainerName on its fixture network.
func getContainerIP(t *testing.T) string {
	t.Helper()
	// Newer Docker engines drop the legacy top-level .NetworkSettings.IPAddress
	// field, so use the per-network Networks map, populated on both old and new
	// engines. Emit one IP per line so multiple attached networks don't
	// concatenate into a single invalid address; the AD-DC fixture attaches only
	// its own network, so the first non-empty line is the IP we want.
	out, err := adDockerOutput("inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}\n{{end}}",
		adContainerName,
	)
	if err != nil {
		t.Fatalf("docker inspect network IP: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if ip := strings.TrimSpace(line); ip != "" {
			return ip
		}
	}
	t.Fatal("could not determine container IP address from docker inspect")
	return ""
}

// copyADCA copies the fixture CA, which issued the DC's TLS certificate, out
// of the running DC and returns its path.
func copyADCA(t *testing.T) string {
	t.Helper()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if out, err := adDockerCombined("cp", adContainerName+":/keytabs/ca.pem", caPath); err != nil {
		t.Fatalf("copy the fixture CA: %v\n%s", err, out)
	}
	return caPath
}

// copyADKeytab copies the service keytab the entrypoint exports out of the
// running DC and returns its path. docker cp reads as root inside the
// container, so the keytab's 0400 mode and dittofs owner do not block it.
func copyADKeytab(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dittofs.keytab")
	if out, err := adDockerCombined("cp", adContainerName+":/keytabs/dittofs.keytab", path); err != nil {
		t.Fatalf("copy the service keytab: %v\n%s", err, out)
	}
	return path
}

// writeADKrb5Conf writes a host-side krb5.conf whose realm KDC is the DC at
// dcIP, over TCP, and returns its path. The krb5.conf Samba generates names
// the DC by a host name the host cannot resolve.
func writeADKrb5Conf(t *testing.T, dcIP string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "krb5.conf")
	conf := fmt.Sprintf(`[libdefaults]
    default_realm = %s
    dns_lookup_realm = false
    dns_lookup_kdc = false
    udp_preference_limit = 1
    rdns = false

[realms]
    %s = {
        kdc = %s:88
    }
`, adRealm, adRealm, dcIP)
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}
	return path
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

func dumpADLogs(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), adExecTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker",
		"logs", "--tail", "100", adContainerName).CombinedOutput()
	t.Logf("---- AD-DC container logs (tail) ----\n%s\n-------------------------------------", out)
}
