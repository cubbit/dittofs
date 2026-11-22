package fixture

import (
	"fmt"
	"os"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// Unsupported says what of fixture f dt doesn't set up yet on platform goos,
// as runtime.GOOS names it, or returns "": what dt itself doesn't do
// (NotSetUp), then what the DittoFS server it starts doesn't serve yet
// (Unserved). A field that holds a matrix placeholder is checked once a cell
// fills it.
func Unsupported(f *registry.Fixture, goos string) string {
	if why := NotSetUp(f, goos); why != "" {
		return why
	}
	return Unserved(f)
}

// Unserved says what of fixture f the DittoFS server dt starts can't serve
// yet, or returns "".
//
// decision: a DittoFS fixture that serves NFS or SMB, and so one with
// namespaced clients, is refused, since the node serves no adapter and its
// adapter status names no port a cell could reach; one on the S3 profile,
// since dt seeds no remote store a share's blocks could reach; one naming a
// shares file, since a share's settings are written only through the
// settings API, which refuses them until the settings table is wired; and
// one naming users, since without an adapter nothing they could be created
// for is served. Each such cell would otherwise run against a server
// answering none of its tests. Withdraw each refusal once its part serves.
func Unserved(f *registry.Fixture) string {
	switch {
	case f.Server == "knfsd":
		return ""
	case f.NFS.Serves() || f.SMB.Enabled || f.Client != "":
		return "dt serves no NFS or SMB through the node yet: the node serves no adapter, and its adapter status names no port"
	case f.Profile == "badger-s3":
		return "dt seeds no remote store yet, so it sets up no S3 profile"
	case f.Shares != "":
		return "dt sets no share settings yet, so it reads no shares file"
	case len(f.Users) > 0:
		return "dt creates no fixture users yet, as the node serves no adapter they could reach"
	}
	return ""
}

// NotSetUp says what of fixture f dt itself doesn't set up yet on platform
// goos, whatever the server it starts serves, or returns "".
func NotSetUp(f *registry.Fixture, goos string) string {
	fixed := func(s string) bool { return len(registry.Placeholders(s)) == 0 }
	if f.Server == "knfsd" {
		return "" // the registry lets it set only what dt sets
	}
	switch {
	case f.Server != "" && f.Server != "dfs" && fixed(f.Server):
		return fmt.Sprintf("dt doesn't set up a %s fixture yet", f.Server)
	case f.Profile != "memory" && f.Profile != "badger" && f.Profile != "badger-s3" && fixed(f.Profile):
		return fmt.Sprintf("dt doesn't set up the %q profile yet", f.Profile)
	case f.SMB.Mount && f.NFS.Mounts():
		return "dt doesn't mount a share over both NFS and SMB yet"
	case f.NFS.Mounts() && !f.NFS.FromOptions && fixed(f.NFS.Version) && mountVers[f.NFS.Version] == "":
		return fmt.Sprintf("dt doesn't mount NFS %s yet", f.NFS.Version)
	case f.NFS.Mounts() && fixed(f.NFS.Version) && goos != "linux":
		return "dt mounts NFS only on Linux yet"
	case f.Client != "" && goos != "linux":
		return "dt sets up namespaced clients only on Linux yet"
	case f.SMB.Encryption != "" && f.SMB.Encryption != "preferred" && fixed(f.SMB.Encryption):
		return fmt.Sprintf("dt doesn't set SMB encryption %q", f.SMB.Encryption)
	case f.Kerberos && (goos != "linux" || !f.SMB.Enabled):
		return "dt sets up Kerberos only for SMB on Linux yet"
	case f.Reset != "" && f.Reset != "each" && fixed(f.Reset):
		return fmt.Sprintf("dt doesn't reset shares %q", f.Reset)
	}
	return ""
}

// Placeholders names what fixture f gives a cell's commands, besides {bin}:
// {server}, {share}, {user}, {user2} and {password}, which a DittoFS
// fixture leaves empty until a route sets a user's password, {nfs-port}
// when it serves NFS, {smb-port} when it serves SMB, {mount} when it mounts the
// share, {bucket} on an S3 profile, {client2} with a second client, and
// {realm} and {krb5-conf} with Kerberos.
func Placeholders(f registry.Fixture) []string {
	if f.Server == "knfsd" {
		return []string{"server", "nfs-port"} // the kernel's server has no users, and runs no {bin}
	}
	names := []string{"server", "share", "user", "user2", "password"}
	if f.NFS.Serves() {
		names = append(names, "nfs-port")
	}
	if f.SMB.Enabled {
		names = append(names, "smb-port")
	}
	if f.NFS.Mounts() || f.SMB.Mount {
		names = append(names, "mount")
	}
	if f.Profile == "badger-s3" {
		names = append(names, "bucket")
	}
	if f.Clients == 2 {
		names = append(names, "client2")
	}
	if f.Kerberos {
		names = append(names, "realm", "krb5-conf")
	}
	return names
}

// Network is the network the fixture's server is on, which a container joins
// to reach it: the host's, unless dt runs in a dtc container on a bridge
// network, whose network the container then shares.
func Network() string {
	if os.Getenv("DT_IN_CONTAINER") == "1" && os.Getenv("DT_HOSTNET") != "1" {
		return "container:" + os.Getenv("DT_CONTAINER_NAME")
	}
	return "host"
}
