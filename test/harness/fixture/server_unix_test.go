//go:build unix

package fixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// buildDfs builds dfs into a directory of the test's and returns it. A host
// whose mount the journal's platform check refuses skips the test: dfs init
// opens its journal under that check, which a test cannot hand a probe, so
// it runs here first, on the same mount.
func buildDfs(t *testing.T) string {
	t.Helper()
	if _, err := journal.Init(t.Context(), t.TempDir(), [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the temporary directory's mount (%v); set TMPDIR to a directory on a suitable mount", err)
	} else if err != nil {
		t.Fatalf("journal init on a scratch directory: %v", err)
	}
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "dfs"), "github.com/cubbit/dittofs/cmd/dfs").CombinedOutput(); err != nil {
		t.Fatalf("go build dfs: %v\n%s", err, out)
	}
	return bin
}

// start starts a memory fixture from bin, failing the test with dt's lines
// and dfs.log when it doesn't start.
func start(t *testing.T, ctx context.Context, bin string) *Server {
	t.Helper()
	out := t.TempDir()
	var w strings.Builder
	s, err := Start(ctx, registry.Fixture{Profile: "memory"}, bin, out, &w)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
		t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
	}
	t.Cleanup(func() { _ = s.Stop() })
	if strings.Contains(w.String(), s.password) {
		t.Errorf("dt's lines on the setup hold the administrator's password:\n%s", w.String())
	}
	return s
}

// The one real start: dfs built, initialised with dfs init and started with
// dfs start under t.TempDir() on 127.0.0.1:0, holds its users, its share,
// applied on the node, and exactly the grants RFC 29 gives, read back
// through the client; a session the server ends is signed in again; Reset
// recreates the share; a second dfs init over the same installation is
// refused and changes nothing; after Kill and Restart the share is still
// there; Stop ends it with exit 0, and Restart then refuses. No child reads
// or writes the HOME or the configuration directory dt runs with.
func TestServer(t *testing.T) {
	bin := buildDfs(t)
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR") // would hide the API's address from dt
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := start(t, ctx, bin)
	if got, want := s.Placeholders()["share"], strings.TrimPrefix(Share, "/"); got != want {
		t.Errorf("{share} is %q, want %q", got, want)
	}
	if err := s.Restart(ctx); err == nil {
		t.Errorf("Restart of a running dfs: accepted, want it refused")
	}

	first := configured(t, s, "after the start")
	if err := s.client.SignOut(ctx); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if n := len(state(t, s, "after the session ended")); n == 0 || s.session.signIns != 2 {
		t.Errorf("after the session ended: %d records read after %d sign-ins, want them read after signing in again, 2 sign-ins", n, s.session.signIns)
	}
	if err := s.Reset(ctx, strings.TrimPrefix(Share, "/")); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reset := configured(t, s, "after Reset")
	if reset == first {
		t.Errorf("after Reset the share is still %s, want it recreated under a new ID", first)
	}

	before := state(t, s, "before the second dfs init")
	if err := s.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := s.initialise(ctx); err == nil {
		t.Errorf("a second dfs init over the installation: accepted, want it refused")
	}
	if log, _ := os.ReadFile(s.log.Name()); !strings.Contains(string(log), installedText) {
		t.Errorf("a second dfs init over the installation: dfs.log doesn't say %q:\n%s", installedText, log)
	}
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := configured(t, s, "after Kill and Restart"); got != reset {
		t.Errorf("after Kill and Restart the share is %s, want %s, the one before", got, reset)
	}
	if after := state(t, s, "after the second dfs init"); !slices.Equal(after, before) {
		t.Errorf("the second dfs init changed what the server holds:\nbefore %v\nafter  %v", before, after)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v, want dfs stopped with exit 0", err)
	}
	if err := s.Restart(ctx); err == nil {
		t.Errorf("Restart after Stop: accepted, want it refused")
	}
	for _, dir := range []string{home, xdg} {
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s holds %s after the fixture ran, want it untouched", dir, ents[0].Name())
		}
	}
}

// A dfs that crashes under the fixture, here killed from outside it, is
// reported: Kill says it had exited on its own, Restart refuses it, and Stop
// says it ended before its cell did.
func TestServerCrash(t *testing.T) {
	bin := buildDfs(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	s := start(t, ctx, bin)
	if err := syscall.Kill(s.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill dfs: %v", err)
	}
	<-s.exited
	if err := s.Kill(); err == nil || !strings.Contains(err.Error(), "exited on its own") {
		t.Errorf("Kill after dfs crashed: %v, want it reporting dfs exited on its own", err)
	}
	if err := s.Restart(ctx); err == nil {
		t.Errorf("Restart after dfs crashed: accepted, want it refused")
	}
	if err := s.Stop(); err == nil || !strings.Contains(err.Error(), "exited before its cell ended") {
		t.Errorf("Stop after dfs crashed: %v, want it reporting dfs exited before its cell ended", err)
	}
}

// configured checks, through the client, that the server holds User and
// User2 with their UIDs, the share Share alone, applied on the node, and
// exactly the grants grants gives on it, and returns the share's ID.
func configured(t *testing.T, s *Server, when string) string {
	t.Helper()
	ctx := t.Context()
	uids, ids := map[string]uint32{}, map[string]string{}
	for u, err := range s.client.Users().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the users: %v", when, err)
		}
		if u.Spec.UID != nil {
			uids[u.Spec.Name] = *u.Spec.UID
		}
		ids[u.ID] = u.Spec.Name
	}
	for name, uid := range map[string]uint32{User: UserUID, User2: User2UID} {
		if got, ok := uids[name]; !ok || got != uid {
			t.Errorf("%s: the user %s has UID %d (held: %v), want %d", when, name, got, ok, uid)
		}
	}
	var ours []string
	for sh, err := range s.client.Shares().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the shares: %v", when, err)
		}
		if sh.Status.State != "removing" {
			ours = append(ours, sh.ID)
		}
		if sh.Spec.Path != Share || sh.ID != s.shareIDs[strings.TrimPrefix(Share, "/")] {
			continue
		}
		if !applied(sh.Status) {
			t.Errorf("%s: the share %s's status is %+v, want it applied on its one node and refused by none", when, Share, sh.Status)
		}
	}
	id := s.shareIDs[strings.TrimPrefix(Share, "/")]
	if len(ours) != 1 || ours[0] != id {
		t.Fatalf("%s: the shares not being removed are %v, want %s (%s) alone", when, ours, Share, id)
	}
	got := map[string]string{}
	for g, err := range s.client.Grants(id).List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the grants on %s: %v", when, Share, err)
		}
		got[ids[g.ID]] = g.Spec.Access
	}
	if want := grants(s.f); !maps.Equal(got, want) {
		t.Errorf("%s: the grants on %s are %v, by user, want %v", when, Share, got, want)
	}
	return id
}

// state is every user, share and grant the server holds, one sorted line
// each, through the client.
func state(t *testing.T, s *Server, when string) []string {
	t.Helper()
	ctx := t.Context()
	var lines []string
	for u, err := range s.client.Users().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the users: %v", when, err)
		}
		uid := "none"
		if u.Spec.UID != nil {
			uid = fmt.Sprint(*u.Spec.UID)
		}
		lines = append(lines, fmt.Sprintf("user %s %s uid=%s disabled=%v kerberos=%q", u.ID, u.Spec.Name, uid, u.Spec.Disabled, u.Spec.Kerberos))
	}
	for sh, err := range s.client.Shares().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the shares: %v", when, err)
		}
		lines = append(lines, fmt.Sprintf("share %s %s %s %s", sh.ID, sh.Spec.Name, sh.Spec.Path, sh.Status.State))
		for g, err := range s.client.Grants(sh.ID).List(ctx, nil) {
			if err != nil {
				t.Fatalf("%s: listing the grants on %s: %v", when, sh.ID, err)
			}
			lines = append(lines, fmt.Sprintf("grant %s %s %s", sh.ID, g.ID, g.Spec.Access))
		}
	}
	slices.Sort(lines)
	return lines
}

// fakeDfs is a server whose dfs is script, a shell script standing in for
// dfs start, under the test's directories.
func fakeDfs(t *testing.T, script string) *Server {
	t.Helper()
	bin, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "dfs"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(data, "dfs.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{bin: bin, config: "c", home: data, data: filepath.Join(data, "d"), out: io.Discard, log: log}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// readyAPI answers every request "ready", and so refuses the sign-in.
func readyAPI(t *testing.T) string {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ready")) }))
	t.Cleanup(api.Close)
	return strings.TrimPrefix(api.URL, "http://")
}

// A log line longer than any scanner's buffer before the API's address still
// leaves the address read.
func TestRunReadsAddressPastLongLine(t *testing.T) {
	addr := readyAPI(t)
	s := fakeDfs(t, `trap 'exit 0' TERM
head -c 300000 /dev/zero | tr '\0' x; echo
echo "node: listening api=`+addr+`"
while :; do sleep 0.1; done
`)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err := s.run(ctx)
	if s.api != "http://"+addr || err == nil || !strings.Contains(err.Error(), "signing in as admin") {
		t.Errorf("run past a long line: API %q, %v; want %s read and the sign-in refused", s.api, err, addr)
	}
}

// Output after the API's address, long lines among it, is drained into
// dfs.log, so dfs never blocks writing it and stops on SIGTERM.
func TestRunDrainsOutput(t *testing.T) {
	addr := readyAPI(t)
	marker := filepath.Join(t.TempDir(), "marker")
	s := fakeDfs(t, `trap 'exit 0' TERM
echo "node: listening api=`+addr+`"
head -c 70000 /dev/zero | tr '\0' x; echo
head -c 300000 /dev/zero | tr '\0' y; echo
touch `+marker+`
while :; do sleep 0.1; done
`)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_ = s.run(ctx) // the sign-in is refused; dfs runs on
	// The bounded wait is the assertion: a dfs blocked on its log never
	// writes the marker.
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dfs never got past its long lines: nothing drains its output")
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v, want dfs stopped on SIGTERM with exit 0", err)
	}
	if log, _ := os.ReadFile(s.log.Name()); !strings.Contains(string(log), strings.Repeat("y", 300000)) {
		t.Errorf("dfs.log lacks the long line after the address")
	}
}
