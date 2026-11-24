package share

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// The mount command line on each platform, from host, port, share and
// options.
func TestMountLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    mount
		want string
	}{
		{"nfs v3 on linux", mount{GOOS: "linux", Protocol: "nfs", Host: "dfs.example", Port: 12049, Share: "/archive", Point: "/mnt/a", NFSVersion: "3"},
			"mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,actimeo=0,nolock dfs.example:/archive /mnt/a"},
		{"nfs v4.1 on linux", mount{GOOS: "linux", Protocol: "nfs", Host: "dfs.example", Port: 2049, Share: "/archive", Point: "/mnt/a", NFSVersion: "4.1"},
			"mount -t nfs -o nfsvers=4.1,tcp,port=2049,actimeo=0 dfs.example:/archive /mnt/a"},
		{"nfs v3 on macOS", mount{GOOS: "darwin", Protocol: "nfs", Host: "dfs.example", Port: 12049, Share: "/archive", Point: "/mnt/a", NFSVersion: "3"},
			"mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,actimeo=0,resvport dfs.example:/archive /mnt/a"},
		{"nfs over IPv6", mount{GOOS: "linux", Protocol: "nfs", Host: "::1", Port: 2049, Share: "/archive", Point: "/mnt/a", NFSVersion: "4"},
			"mount -t nfs -o nfsvers=4,tcp,port=2049,actimeo=0 [::1]:/archive /mnt/a"},
		{"nfs on Windows", mount{GOOS: "windows", Protocol: "nfs", Host: "dfs.example", Port: 2049, Share: "/team/archive", Point: "Z:", NFSVersion: "3"},
			`mount -o mtype=hard,nolock \\dfs.example\team\archive Z:`},
		{"smb on linux", mount{GOOS: "linux", Protocol: "smb", Host: "dfs.example", Port: 1445, Share: "archive", Point: "/mnt/a", User: "alice"},
			"mount -t cifs -o vers=2.1,port=1445,username=alice //dfs.example/archive /mnt/a"},
		{"smb on macOS", mount{GOOS: "darwin", Protocol: "smb", Host: "dfs.example", Port: 1445, Share: "archive", Point: "/mnt/a", User: "a b"},
			"mount_smbfs smb://a%20b@dfs.example:1445/archive /mnt/a"},
		{"smb on Windows", mount{GOOS: "windows", Protocol: "smb", Host: "dfs.example", Port: 1445, Share: "archive", Point: "Z:", User: "alice"},
			`net use Z: \\dfs.example\archive /user:alice * /TCPPORT:1445`},
		{"smb on Windows on 445", mount{GOOS: "windows", Protocol: "smb", Host: "dfs.example", Port: 445, Share: "archive", Point: "Z:", User: "alice"},
			`net use Z: \\dfs.example\archive /user:alice *`},
	} {
		got, err := mountLine(tc.m)
		if err != nil || !reflect.DeepEqual(got, strings.Fields(tc.want)) {
			t.Errorf("%s: got %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
}

// A mount the platform cannot make, or whose options would be misread, is
// refused naming why.
func TestMountLineRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    mount
		want string
	}{
		{"nfs on Windows off 2049", mount{GOOS: "windows", Protocol: "nfs", Port: 12049}, "only from port 2049"},
		{"smb with no user", mount{GOOS: "linux", Protocol: "smb", Port: 445}, "--username"},
		{"a cifs user with a comma", mount{GOOS: "linux", Protocol: "smb", Port: 445, User: "a,b"}, "comma"},
		{"an unknown platform", mount{GOOS: "plan9", Protocol: "nfs"}, "plan9"},
	} {
		if _, err := mountLine(tc.m); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

// The port a mount falls back to is the adapter's own default, which dfsctl
// may not link.
func TestDefaultPortsAreTheAdapters(t *testing.T) {
	if got, want := defaultPorts["nfs"], int(settings.DefaultNFS().Port); got != want {
		t.Errorf("NFS default port: got %d, want the adapter's %d", got, want)
	}
	if got, want := defaultPorts["smb"], int(settings.DefaultSMB().Port); got != want {
		t.Errorf("SMB default port: got %d, want the adapter's %d", got, want)
	}
}
