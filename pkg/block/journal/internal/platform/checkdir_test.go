package platform

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestRefusalMatchesErrUnsuitableStorage pins every rule's answer to the
// caller's sentinel at the verdict's own level, which every platform builds
// and can name without making a system call: a refusal is the one error Init
// and Open answer with ErrUnsuitableStorage, while its message still names
// the rule, the directory and what to change, and no error that says the
// inputs did not describe the directory matches the sentinel, so a caller
// can tell a fault to fix from a refusal.
func TestRefusalMatchesErrUnsuitableStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, rule := range []refusalRule{
		ruleBarrier,
		ruleSubvolume,
		ruleCopyOnWrite,
		ruleCachePolicy,
	} {
		r := &refusal{rule: rule, dir: "/journal", mount: "/", path: "/journal"}
		if !errors.Is(r, core.ErrUnsuitableStorage) {
			t.Errorf("refusal %d does not match core.ErrUnsuitableStorage", rule)
		}

		if !strings.Contains(r.Error(), "/journal") {
			t.Errorf("refusal %d: got %q, want the message naming the directory", rule, r)
		}
	}

	for _, err := range []error{
		errDirectoryNotClean,
		errMountTableMalformed,
		errMountUnresolved,
		errMountIDDisagrees,
		errMountTypeMismatch,
	} {
		if errors.Is(err, core.ErrUnsuitableStorage) {
			t.Errorf("%v matches core.ErrUnsuitableStorage", err)
		}
	}
}

// TestCheckDir_RefusalMatchesErrUnsuitableStorage fails a CheckDir that
// answers a refusal the caller cannot name: the refusal of a directory any
// platform rule applies to matches ErrUnsuitableStorage, while the message
// still names the rule, the directory and what to change. The gather hands
// the check a mount without cache flushes, so no answer depends on the
// host's mounts.
func TestCheckDir_RefusalMatchesErrUnsuitableStorage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	probe := platformtest.NoBarrierMount(dir)
	err := CheckDir(dir, func(string) (storage.Probe, error) { return probe, nil })

	if !errors.Is(err, core.ErrUnsuitableStorage) {
		t.Fatalf("got %v, want it matching core.ErrUnsuitableStorage", err)
	}

	if !strings.Contains(err.Error(), "nobarrier") || !strings.Contains(err.Error(), platformtest.ProbeDir(dir)) {
		t.Errorf("got %q, want the message naming the rule and the directory", err)
	}
}

// TestCheckDir_ProbeErrorIsNotUnsuitable fails a CheckDir that turns an
// error met while gathering into a refusal: the gather's error, such as a
// mount table that cannot be read, is returned as itself and does not match
// ErrUnsuitableStorage, so a caller can tell a fault to fix from a refusal.
func TestCheckDir_ProbeErrorIsNotUnsuitable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	cause := &fs.PathError{Op: "open", Path: "/proc/self/mountinfo", Err: errors.New("an I/O error met while reading the mount table")}
	err := CheckDir(t.TempDir(), func(string) (storage.Probe, error) { return storage.Probe{}, cause })

	if !errors.Is(err, cause) {
		t.Fatalf("got %v, want the gather's error as itself", err)
	}

	if errors.Is(err, core.ErrUnsuitableStorage) {
		t.Fatalf("got %v, want ErrUnsuitableStorage not matching it", err)
	}
}
