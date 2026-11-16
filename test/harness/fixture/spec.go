package fixture

import (
	"errors"
	"fmt"
	"os"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// ErrNoServer refuses a fixture whose server is DittoFS: dt starts none
// until its fixture is rebuilt on the server's new control plane, with dfs
// init and dfs start.
//
// decision: the refusal is the fixture's start, not Unsupported, so a cell
// that asks for a DittoFS server is not run with this reason while dt's own
// tests of a fixture's lifecycle, which start a fake server, still run. Move
// it into Unsupported if a listing must show such cells as not runnable.
var ErrNoServer = errors.New("dt starts no DittoFS server until its fixture is rebuilt on the new control plane")

// Unsupported says what of fixture f dt doesn't set up yet on platform goos,
// as runtime.GOOS names it, or returns "". A field that holds a matrix
// placeholder is checked once a cell fills it.
func Unsupported(f *registry.Fixture, goos string) string {
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
// {server}, {share}, {user}, {user2} and {password}, {nfs-port} when it
// serves NFS, {smb-port} when it serves SMB, {mount} when it mounts the
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
