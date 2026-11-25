//go:build e2e

package helpers

import (
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/test/e2e/framework"
)

// MountNFS mounts share from the server with the kernel's NFS client at
// version vers, 3, 4.0, 4.1 or 4.2, under the security flavour sec, as
// mount's sec= names it (sys, krb5, krb5i, krb5p), and returns the mount, or
// the client's error without failing t, since a refused mount is what an
// admission row asserts. The test's end unmounts it. NFSv3 reaches MOUNT on
// the same port, and nolock answers its locks in the client.
func MountNFS(t *testing.T, share Share, vers, sec string) (*framework.Mount, error) {
	t.Helper()
	s := Start(t)
	if s.NFSPort == 0 {
		t.Fatal("the server names no NFS port to mount from")
	}
	opts := fmt.Sprintf("vers=%s,port=%d,sec=%s,actimeo=0", vers, s.NFSPort, sec)
	if vers == "3" {
		opts += fmt.Sprintf(",tcp,mountport=%d,nolock", s.NFSPort)
	}
	m, err := framework.MountAt(t, "nfs", s.Addr+":/"+share.Name, opts)
	if err == nil {
		t.Cleanup(m.Unmount)
	}
	return m, err
}
