package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

func TestDefaultDirIsRemovedNamedDirIsKept(t *testing.T) {
	// The default run leaves no 1.5 GiB of preallocated segments behind;
	// a directory the presenter named is theirs to keep.
	mainOptions = []journal.Option{journaltest.SuitableMount()}
	defer func() { mainOptions = nil }()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var out, errw strings.Builder
	if code := runMain(nil, strings.NewReader(""), &out, &errw); code != 0 {
		t.Fatalf("runMain on the default dir: exit %d, stderr %q", code, errw.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "jdemo-*")); len(left) != 0 {
		t.Errorf("the default run left %d jdemo-* directories behind (want none): %v", len(left), left)
	}

	base := t.TempDir()
	named := filepath.Join(base, "talk-journal")
	args := []string{"--dir", named}
	if code := runMain(args, strings.NewReader(""), &out, &errw); code != 0 {
		t.Fatalf("runMain on a named dir: exit %d, stderr %q", code, errw.String())
	}
	if _, err := os.Stat(named); os.IsNotExist(err) {
		t.Errorf("runMain removed the presenter's named directory %s", named)
	}
}
