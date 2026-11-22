package fixture

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// The one real start: dfs built, initialised with dfs init and started with
// dfs start under t.TempDir() on 127.0.0.1:0, holds its users, its share and
// both users' grants, read back through the client; Reset recreates the
// share with its grants; a second dfs init over the same installation is
// refused and changes nothing; and after Kill and Restart the share is still
// there. Stop ends it with exit 0.
func TestServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dfs is stopped by SIGTERM, which this system can't send")
	}
	// dfs init opens its journal under the platform's own check, which a
	// test cannot hand a probe, so the check runs here first, on the same
	// mount.
	if _, err := journal.Init(t.Context(), t.TempDir(), [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the temporary directory's mount (%v); set TMPDIR to a directory on a suitable mount", err)
	} else if err != nil {
		t.Fatalf("journal init on a scratch directory: %v", err)
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "dfs"), "github.com/cubbit/dittofs/cmd/dfs")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build dfs: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
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
	if got, want := s.Placeholders()["share"], strings.TrimPrefix(Share, "/"); got != want {
		t.Errorf("{share} is %q, want %q", got, want)
	}

	first := configured(t, s, "after the start")
	if err := s.Reset(strings.TrimPrefix(Share, "/")); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	reset := configured(t, s, "after Reset")
	if reset == first {
		t.Errorf("after Reset the share is still %s, want it recreated under a new ID", first)
	}

	s.Kill()
	if err := s.initialise(ctx); err == nil {
		t.Errorf("a second dfs init over the installation: accepted, want it refused")
	}
	if log, _ := os.ReadFile(s.log.Name()); !strings.Contains(string(log), "already initialised") {
		t.Errorf("a second dfs init over the installation: dfs.log names no refusal of an initialised installation:\n%s", log)
	}
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := configured(t, s, "after Kill and Restart"); got != reset {
		t.Errorf("after Kill and Restart the share is %s, want %s, the one before", got, reset)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v, want dfs stopped with exit 0", err)
	}
}

// configured checks, through the client, that the server holds User and
// User2 with their UIDs, the share Share alone, and both users' read-write
// grants on it, and returns the share's ID.
func configured(t *testing.T, s *Server, when string) string {
	t.Helper()
	ctx := t.Context()
	uids := map[string]uint32{}
	ids := map[string]string{}
	for u, err := range s.client.Users().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the users: %v", when, err)
		}
		if u.Spec.UID != nil {
			uids[u.Spec.Name] = *u.Spec.UID
		}
		ids[u.Spec.Name] = u.ID
	}
	for name, uid := range map[string]uint32{User: UserUID, User2: User2UID} {
		if got, ok := uids[name]; !ok || got != uid {
			t.Errorf("%s: the user %s has UID %d (held: %v), want %d", when, name, got, ok, uid)
		}
	}
	var shares []apiv1.Resource[apiv1.ShareSpec, apiv1.ShareStatus]
	for sh, err := range s.client.Shares().List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the shares: %v", when, err)
		}
		shares = append(shares, sh)
	}
	if len(shares) != 1 || shares[0].Spec.Path != Share {
		t.Fatalf("%s: the shares are %+v, want %s alone", when, shares, Share)
	}
	grants := map[string]string{}
	for g, err := range s.client.Grants(shares[0].ID).List(ctx, nil) {
		if err != nil {
			t.Fatalf("%s: listing the grants on %s: %v", when, Share, err)
		}
		grants[g.ID] = g.Spec.Access
	}
	for _, name := range []string{User, User2} {
		if got := grants[ids[name]]; got != "read-write" {
			t.Errorf("%s: %s's grant on %s is %q, want read-write (grants: %v)", when, name, Share, got, grants)
		}
	}
	return shares[0].ID
}

// A fixture the node can't serve yet is refused before anything starts,
// naming why.
func TestStartRefusesUnserved(t *testing.T) {
	for _, tc := range []struct {
		f    registry.Fixture
		want string
	}{
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}}, "serves no NFS or SMB"},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}}, "serves no NFS or SMB"},
		{registry.Fixture{Profile: "badger-s3"}, "no remote store"},
		{registry.Fixture{Profile: "memory", Shares: "shares.yaml"}, "no shares file"},
	} {
		out := t.TempDir()
		_, err := Start(t.Context(), tc.f, t.TempDir(), out, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: Start returned %v, want a refusal naming %q", tc.f, err, tc.want)
		}
		if ents, _ := os.ReadDir(out); len(ents) != 0 {
			t.Errorf("%+v: the refused start left %s in its output directory, want nothing", tc.f, ents[0].Name())
		}
	}
}
