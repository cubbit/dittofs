//go:build e2e

// Package helpers drives the e2e tests' server: one DittoFS per package run,
// started through the harness's fixture on dfs init and dfs start, reached
// through the management API's client signed in as its administrator, with
// the accounts, groups, shares, grants and NFS mounts a test makes on it, each
// undone when the test ends.
package helpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/test/harness/fixture"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// served is the server Run started for the package's tests.
var served *fixture.Server

// Server is the running server as a test reaches it.
type Server struct {
	Addr    string         // the address an NFS client mounts from
	NFSPort int            // 0 while the node names no NFS port
	API     *client.Client // signed in as the bootstrap administrator
}

// Run is a package's TestMain: it builds dfs into a directory every user can
// read, starts one server from it serving NFS, runs m's tests, and
// stops the server. It returns the exit code.
func Run(m *testing.M) int {
	code, err := run(m)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	return code
}

func run(m *testing.M) (int, error) {
	// decision: the binary and the server's data go under /tmp, which every
	// user can traverse, rather than under TMPDIR, which a developer shell
	// may make private: a test acting as another uid must reach both.
	bin, err := os.MkdirTemp("/tmp", "dittofs-e2e-bin-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(bin) }()
	if err := os.Chmod(bin, 0o755); err != nil {
		return 0, err
	}
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "dfs"), "github.com/cubbit/dittofs/cmd/dfs").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("go build dfs: %w\n%s", err, out)
	}
	out, err := os.MkdirTemp("/tmp", "dittofs-e2e-server-")
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// The fixture serves NFS without mounting it; the tests make their own
	// mounts, each as the account it is about.
	f := registry.Fixture{Server: "dfs", Profile: "memory", NFS: registry.NFS{Version: "export"}}
	if served, err = fixture.Start(ctx, f, bin, out, os.Stderr); err != nil {
		return 0, fmt.Errorf("starting the server: %w; its log is %s", err, filepath.Join(out, "dfs.log"))
	}
	code := m.Run()
	if err := served.Stop(); err != nil {
		return 0, fmt.Errorf("stopping the server: %w; its log is %s", err, filepath.Join(out, "dfs.log"))
	}
	_ = os.RemoveAll(out)
	return code, nil
}

// Start returns the server Run started, failing t when there is none.
func Start(t *testing.T) Server {
	t.Helper()
	if served == nil {
		t.Fatal("no server: the package's TestMain must call helpers.Run")
	}
	p := served.Placeholders()
	nfs, _ := strconv.Atoi(p["nfs-port"])
	return Server{Addr: p["server"], NFSPort: nfs, API: served.Client()}
}

// api is the administrator's client, failing t when no server runs.
func api(t *testing.T) *client.Client {
	t.Helper()
	return Start(t).API
}

// call is a context for one API call, which a test's cleanup makes too,
// after the test's own context ended.
func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}
