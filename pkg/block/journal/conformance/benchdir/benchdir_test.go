package benchdir

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRAMBacked(t *testing.T) {
	for _, tc := range []struct {
		fs   string
		want bool
	}{
		{"tmpfs", true},
		{"ramfs", true},
		{"TMPFS", true},
		{"ext4", false},
		{"xfs", false},
		{"btrfs", false},
		{"apfs", false},
		{"hfs", false},
		{"HFS+", false},
		{"", false},
	} {
		if got := RAMBacked(tc.fs); got != tc.want {
			t.Errorf("RAMBacked(%q) = %v, want %v", tc.fs, got, tc.want)
		}
	}
}

func TestRefusalFor(t *testing.T) {
	for _, tc := range []struct {
		fs      string
		refused bool
	}{
		{"tmpfs", true}, {"ramfs", true}, {"ext4", false}, {"apfs", false}, {"0x1234", false},
	} {
		err := refusalFor("/bench", tc.fs)
		if (err != nil) != tc.refused {
			t.Errorf("refusalFor(/bench, %q) = %v, want refused %v", tc.fs, err, tc.refused)
		}
		if err != nil && (!strings.Contains(err.Error(), "/bench") || !strings.Contains(err.Error(), tc.fs)) {
			t.Errorf("refusalFor(/bench, %q) = %q, want it to name the directory and the filesystem", tc.fs, err)
		}
	}
}

// A directory the platform cannot read is an error, never an acceptance.
func TestRefusalOfAMissingDirectory(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("this platform reads no filesystem")
	}
	if err := Refusal(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("Refusal of a missing directory = nil, want an error")
	}
}
