package platformtest

import (
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
)

// TestSuitableMountHoldsSuitable fails a suitable mount the real verdict
// does not accept, or an unsuitable one it does: the helpers feed the
// verdict its inputs, so a helper that rots into always-accept — a table
// that stops parsing, or an option that stops refusing — fails here rather
// than in every test that derives its answer from it.
func TestSuitableMountHoldsSuitable(t *testing.T) {
	if err := platform.CheckProbe(SuitableMount("/tmp/journal")); err != nil {
		t.Fatalf("the suitable mount: got %v, want the directory accepted", err)
	}

	err := platform.CheckProbe(NoBarrierMount("/tmp/journal"))
	if !errors.Is(err, core.ErrUnsuitableStorage) {
		t.Fatalf("the mount without cache flushes: got %v, want it refused matching core.ErrUnsuitableStorage", err)
	}
	if !strings.Contains(err.Error(), "/tmp/journal") || !strings.Contains(err.Error(), "remount with barriers") {
		t.Errorf("the refusal %q names neither the directory nor what to change", err)
	}
}

// TestProbeDirWindowsSpelling feeds the real verdict the probe a Windows
// host's directory yields: a drive path must arrive as absolute, clean text.
func TestProbeDirWindowsSpelling(t *testing.T) {
	const win = `C:\Users\RUNNER~1\AppData\Local\Temp\TestX\001`
	const want = "/C:/Users/RUNNER~1/AppData/Local/Temp/TestX/001"
	got := probeDir(win, '\\')
	if got != want {
		t.Fatalf("probeDir(%q) = %q, want %q", win, got, want)
	}
	if err := platform.CheckProbe(mountTableOn(win, "rw,relatime", '\\')); err != nil {
		t.Fatalf("the suitable mount for the Windows spelling: got %v, want accepted", err)
	}
	err := platform.CheckProbe(mountTableOn(win, "rw,nobarrier,relatime", '\\'))
	if !errors.Is(err, core.ErrUnsuitableStorage) {
		t.Fatalf("the no-barrier mount for the Windows spelling: got %v, want core.ErrUnsuitableStorage", err)
	}
}

// TestSuitableMountOfHostDir holds on every host that a real temporary
// directory's probe passes the verdict.
func TestSuitableMountOfHostDir(t *testing.T) {
	if err := platform.CheckProbe(SuitableMount(t.TempDir())); err != nil {
		t.Fatalf("got %v, want the host temporary directory accepted", err)
	}
}
