//go:build windows

package platform

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"

	"golang.org/x/sys/windows"
)

// TestCheckDirOSRealVolume fails a probe that cannot run on a real volume:
// this is the whole real path, GetVolumePathName to the answer the property
// query brings back, with no recorded answers.
//
// The test reads the gather's own answer, not only the verdict: a check that
// returns nil would stay green on a volume whose query the rule never sees —
// the not-reported answers pass — so the query's answer is asserted on its
// own: it succeeded, and its bytes reach the policy byte at offset 25, which
// every Version of 28 or above holds.
//
// decision: a volume that answers ERROR_INVALID_FUNCTION or
// ERROR_NOT_SUPPORTED reports no write-cache property, and a volume whose
// Version is below the one published layout never reaches the policy byte:
// none of those answers is the machine's policy, so the test skips naming the
// volume and its answer — a skip is a visible report, never counted as a
// pass. Anything else the volume answers is asserted, a Version above the
// published structure's included, since members added past it leave byte 25
// the policy the verdict reads. Withdraw the skips when something can arrange
// a test's own volume.
//
// No test arranges a real volume's write-cache policy — nothing but the
// machine's own disk policy decides the refusal here — so the check below
// asserts what a real volume must answer whatever its policy. The refusal
// itself is pinned by the verdict rows on checkCacheProbe. Withdraw this
// ceiling when something can set the policy of a test's own volume.
func TestCheckDirOSRealVolume(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()

	p, err := probeVolume(realTable, dir)
	if err != nil {
		t.Fatalf("probeVolume of %s: got %v, want the volume's own answers", dir, err)
	}
	if p.mountPoint == "" || p.volume == "" {
		t.Fatalf("probeVolume of %s: got mount point %q and volume %q, want the machine's volume named", dir, p.mountPoint, p.volume)
	}

	// decision: the skips are reports, not passes — the reasons are in the
	// test's doc. A volume that answers anything but the two not-reported
	// codes has failed its query, and the test fails with it.
	if p.queryErr == errorInvalidFunction || p.queryErr == errorNotSupported {
		t.Skipf("the volume of %s reports no write-cache property (Win32 error %d), so the answer the rule reads was never produced; the skip is a report, never counted as a pass", dir, p.queryErr)
	}
	if p.queryErr != 0 {
		t.Fatalf("the write-cache query of %s failed with Win32 error %d, want an answer", p.mountPoint, p.queryErr)
	}
	if len(p.answer) < writeCacheHeaderLen {
		t.Fatalf("the write-cache query of %s returned %d bytes, want at least the %d of the header", p.mountPoint, len(p.answer), writeCacheHeaderLen)
	}

	// A Version below the published structure's ends before the policy byte;
	// a Version of it or above holds the field, and is asserted either way.
	if version := binary.LittleEndian.Uint32(p.answer[writeCacheVersionOff:]); version < writeCacheFirstWhole {
		t.Skipf("the volume of %s answers Version %d, below the published structure's %d that first holds the policy byte, so no answer of it reaches the rule; the skip is a report, never counted as a pass", p.mountPoint, version, writeCacheFirstWhole)
	}
	if len(p.answer) <= writeCachePolicyOff {
		t.Fatalf("the write-cache answer of %s is %d bytes, want at least the %d that reach the policy byte at offset %d", p.mountPoint, len(p.answer), writeCachePolicyOff+1, writeCachePolicyOff)
	}

	check := func(t *testing.T, dir string) {
		t.Helper()

		err := checkDirOS(dir)
		if err == nil {
			return
		}

		// A volume with the policy genuinely on refuses, and is not a test
		// bug: only a refusal by the cache-policy rule names the machine's
		// own policy, and anything a real volume could not answer — a cut
		// answer, a failed call — fails here, now that the answer above has
		// pinned the query's own success.
		if !errors.Is(err, core.ErrUnsuitableStorage) {
			t.Fatalf("directory %s: got %v, want the directory accepted or refused as unsuitable", dir, err)
		}

		var r *refusal
		if !errors.As(err, &r) || r.rule != ruleCachePolicy {
			t.Fatalf("directory %s: got %v, want a cache-policy refusal", dir, err)
		}
	}

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
