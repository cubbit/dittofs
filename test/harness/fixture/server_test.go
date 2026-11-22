package fixture

import (
	"errors"
	"maps"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// The grants follow RFC 29 §4.2: none on the default share, {user}'s
// read-write on it when the fixture mounts it over SMB, and {user}'s admin
// on a shares file's shares; {user2} holds none anywhere.
func TestGrants(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    registry.Fixture
		want map[string]string
	}{
		{"the default share", registry.Fixture{Profile: "memory"}, map[string]string{}},
		{"served over NFS", registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "4.1"}}, map[string]string{}},
		{"mounted over SMB", registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true, Mount: true}}, map[string]string{User: "read-write"}},
		{"a shares file", registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Shares: "s.yaml"}, map[string]string{User: "admin"}},
	} {
		if got := grants(tc.f); !maps.Equal(got, tc.want) {
			t.Errorf("%s: grants %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A share counts as applied only when every node reporting it applied it,
// at least one, and none refused it.
func TestApplied(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   apiv1.ShareStatus
		want bool
	}{
		{"no node reported", apiv1.ShareStatus{}, false},
		{"none of none", apiv1.ShareStatus{Applied: &apiv1.Applied{}}, false},
		{"none of one", apiv1.ShareStatus{Applied: &apiv1.Applied{Nodes: 0, Of: 1}}, false},
		{"refused", apiv1.ShareStatus{Applied: &apiv1.Applied{Nodes: 1, Of: 1}, Refused: 1}, false},
		{"one of one", apiv1.ShareStatus{Applied: &apiv1.Applied{Nodes: 1, Of: 1}}, true},
	} {
		if got := applied(tc.st); got != tc.want {
			t.Errorf("%s: applied %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Stop reports a dfs that exited before Stop as having ended before its
// cell, and one that didn't exit zero on SIGTERM as not stopped cleanly.
func TestStopResult(t *testing.T) {
	crash := errors.New("signal: killed")
	for _, tc := range []struct {
		name    string
		early   bool
		waitErr error
		want    string
	}{
		{"stopped cleanly", false, nil, ""},
		{"not zero on SIGTERM", false, crash, "did not stop cleanly on SIGTERM (signal: killed)"},
		{"exited first", true, crash, "exited before its cell ended (signal: killed)"},
		{"exited zero first", true, nil, "exited before its cell ended"},
	} {
		err := stopResult(tc.early, tc.waitErr, "dfs.log")
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: stopResult %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A fixture the node can't serve yet is refused before anything starts,
// naming why, and so is one dt itself doesn't set up.
func TestStartRefusesUnsupported(t *testing.T) {
	for _, tc := range []struct {
		f    registry.Fixture
		want string
	}{
		{registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: "export"}}, "serves no NFS or SMB"},
		{registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}}, "serves no NFS or SMB"},
		{registry.Fixture{Profile: "badger-s3"}, "no remote store"},
		{registry.Fixture{Profile: "memory", Shares: "shares.yaml"}, "no shares file"},
		{registry.Fixture{Profile: "memory", Users: []string{"1"}}, "creates no fixture users"},
		{registry.Fixture{Profile: "postgres-s3"}, `the "postgres-s3" profile`},
	} {
		out := t.TempDir()
		_, err := Start(t.Context(), tc.f, t.TempDir(), out, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: Start returned %v, want a refusal naming %q", tc.f, err, tc.want)
		}
		if why := Unsupported(&tc.f, runtime.GOOS); !strings.Contains(why, tc.want) {
			t.Errorf("%+v: Unsupported %q, want it naming %q, so dt run and dt list skip it", tc.f, why, tc.want)
		}
		if ents, _ := os.ReadDir(out); len(ents) != 0 {
			t.Errorf("%+v: the refused start left %s in its output directory, want nothing", tc.f, ents[0].Name())
		}
	}
}
