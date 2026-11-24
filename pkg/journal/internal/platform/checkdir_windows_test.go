//go:build windows

package platform

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"

	"golang.org/x/sys/windows"
)

// TestCheckDirOSRealVolume fails a checkDirOS that cannot run on a real
// volume: this is the whole real path, GetVolumePathName to the answer the
// property query brings back, with no recorded answers. The directory is
// probed several times, as Init and open of more than one journal probe it,
// and once at the root of the volume the directory's own call answers, whose
// path is another shape of the mount point and GUID answers.
//
// decision: no test arranges a real volume's write-cache policy — nothing
// but the machine's own disk policy decides the refusal here — so this test
// asserts what a real volume must answer whatever its policy: no panic, and
// a check that passes or refuses the directory by the cache-policy rule. The
// refusal itself is pinned by the recorded rows of
// TestCheckWriteCachePolicyVolume. Withdraw this ceiling when something can
// set the policy of a test's own volume.
func TestCheckDirOSRealVolume(t *testing.T) {
	prof.Watch(t, prof.Options{})

	check := func(t *testing.T, dir string) {
		t.Helper()

		err := checkDirOS(dir)
		if err == nil {
			return
		}

		// A volume with the policy genuinely on refuses, and is not a test
		// bug: only a refusal by the cache-policy rule names the machine's
		// own policy, and anything a real volume could not answer — a cut
		// answer, a failed call — fails here.
		if !errors.Is(err, core.ErrUnsuitableStorage) {
			t.Fatalf("directory %s: got %v, want the directory accepted or refused as unsuitable", dir, err)
		}

		var r *refusal
		if !errors.As(err, &r) || r.rule != ruleCachePolicy {
			t.Fatalf("directory %s: got %v, want a cache-policy refusal", dir, err)
		}
	}

	dir := t.TempDir()
	for range 4 {
		check(t, dir)
	}

	// The root of the volume the directory sits on, as the probe's own
	// first call reads it.
	var mount [volumePathLen]uint16
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := windows.GetVolumePathName(dirPtr, &mount[0], volumePathLen); err != nil {
		t.Fatalf("GetVolumePathName of %s: got %v, want the volume's root", dir, err)
	}

	check(t, windows.UTF16ToString(mount[:]))
}
