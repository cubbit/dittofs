//go:build e2e

package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/test/e2e/framework"
)

// MountNFS mounts share from the server with the kernel's NFS client at
// version vers, 3, 4.0, 4.1 or 4.2, under the security flavour sec, as
// mount's sec= names it (sys, krb5, krb5i, krb5p), and returns the mount, or
// the client's error without failing t, since a refused mount is what an
// admission row asserts. The mount runs as root; Reach then presents an
// account. The test's end unmounts it and removes its mount point. NFSv3
// reaches MOUNT on the same port, and nolock answers its locks in the
// client; retry=0 makes a server that does not answer fail the mount at once.
func MountNFS(t *testing.T, share Share, vers, sec string) (*framework.Mount, error) {
	t.Helper()
	s := Start(t)
	if s.NFSPort == 0 {
		t.Fatal("the server names no NFS port to mount from")
	}
	opts := fmt.Sprintf("vers=%s,port=%d,sec=%s,actimeo=0,retry=0", vers, s.NFSPort, sec)
	if vers == "3" {
		opts += fmt.Sprintf(",tcp,mountport=%d,nolock", s.NFSPort)
	}
	m, err := framework.MountAt(t, "nfs", s.Addr+":/"+share.Name, opts)
	if err == nil {
		t.Cleanup(func() {
			m.Unmount()
			_ = os.Remove(m.Path)
		})
	}
	return m, err
}

// Reach is what who reaches of the mounted share m over AUTH_SYS, acting as
// its uid: Refused when it cannot list the share's root, Read when it can
// but cannot create a file there, ReadWrite when it can do both. A file it
// creates is removed again.
//
// ponytail: a create the share root's own mode refuses also reads as Read,
// since both answer EACCES. Tell them apart, by the root's mode or a probe
// as an account granted admin, once the node serves NFS and a new share's
// root mode is settled.
//
// decision: who acts with its uid as its gid too, since a user's record
// holds no gid the API could give it; a refusal then cannot come from a gid
// the account lacks, as no grant in the admission rows names one.
func Reach(t *testing.T, m *framework.Mount, who Account) Access {
	t.Helper()
	if _, err := framework.ListDirAsUID(t, who.UID, who.UID, m.Path); err != nil {
		return Refused
	}
	probe := filepath.Join(m.Path, unique("reach"))
	if err := framework.WriteFileAsUID(t, who.UID, who.UID, probe, []byte("reach")); err != nil {
		return Read
	}
	if err := framework.RemoveAsUID(t, who.UID, who.UID, probe); err != nil {
		t.Errorf("removing %s as uid %d: %v", probe, who.UID, err)
	}
	return ReadWrite
}
