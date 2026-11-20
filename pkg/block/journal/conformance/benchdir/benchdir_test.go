package benchdir

import "testing"

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

// An ordinary directory of the host, an APFS volume on a developer's Mac
// among them, is refused only if the platform reads its filesystem as RAM.
func TestRefusalFollowsTheHostsFilesystem(t *testing.T) {
	dir := t.TempDir()
	name, err := FSType(dir)
	if err != nil {
		t.Fatalf("FSType of a temporary directory: %v", err)
	}
	if got, want := Refusal(dir) != nil, RAMBacked(name); got != want {
		t.Fatalf("Refusal(%s) refused = %v on %q, want %v", dir, got, name, want)
	}
}
