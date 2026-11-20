package bootstrap

import (
	"io/fs"
	"slices"
	"testing"
	"time"
)

// fakeInfo is a file's facts with the platform's own part set by the test.
type fakeInfo struct {
	sys any
}

func (fakeInfo) Name() string {
	return "state"
}

func (fakeInfo) Size() int64 {
	return 0
}

func (fakeInfo) Mode() fs.FileMode {
	return fs.ModeDir | 0o755
}

func (fakeInfo) ModTime() time.Time {
	return time.Time{}
}

func (fakeInfo) IsDir() bool {
	return true
}

func (f fakeInfo) Sys() any {
	return f.sys
}

// The state directory is refused for what lets another user replace what is
// in it: a link, a world write bit, an owner that is neither the node's
// user nor root. A missing one, and an owner the platform does not record,
// are not refused.
func TestRefuseDir(t *testing.T) {
	t.Parallel()

	const node = 1001

	for _, tc := range []struct {
		name string
		dir  dirState
		want []string
	}{
		{
			"another user's",
			dirState{exists: true, mode: 0o755, owner: 1000, ownerSet: true},
			[]string{"is owned by another user"},
		},
		{"root's", dirState{exists: true, mode: 0o755, owner: 0, ownerSet: true}, nil},
		{"the node user's", dirState{exists: true, mode: 0o700, owner: node, ownerSet: true}, nil},
		{
			"an owner the platform does not record",
			dirState{exists: true, mode: 0o755, owner: 1000},
			nil,
		},
		{
			"a link",
			dirState{exists: true, symlink: true, mode: 0o777, owner: node, ownerSet: true},
			[]string{"is a symlink"},
		},
		{
			"writable by others",
			dirState{exists: true, mode: 0o757, owner: node, ownerSet: true},
			[]string{"is writable by others"},
		},
		{
			"writable by the group",
			dirState{exists: true, mode: 0o775, owner: node, ownerSet: true},
			nil,
		},
		{
			"another user's and writable by others",
			dirState{exists: true, mode: 0o777, owner: 1000, ownerSet: true},
			[]string{"is writable by others", "is owned by another user"},
		},
		{
			"mode bits of a platform that records no owner",
			dirState{exists: true, mode: 0o777},
			nil,
		},
		{"missing", dirState{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := refuseDir(tc.dir, node); !slices.Equal(got, tc.want) {
				t.Errorf("refuseDir(%+v): got %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}

// gidOnly and uidPlaced are platform facts that carry no owner id the check
// may read: a group alone, and an owner in a field of another type.
type gidOnly struct {
	Gid uint32
}

type uidText struct {
	Uid string
}

// Facts that carry no owner id give no owner.
func TestDirOwnerNoOwner(t *testing.T) {
	t.Parallel()

	for _, sys := range []any{
		nil, "none", gidOnly{Gid: 7}, &uidText{Uid: "1000"}, (*gidOnly)(nil),
	} {
		if owner, ok := ownerOf(t, sys); ok {
			t.Errorf("dirOwner of %#v: got owner %d, want none", sys, owner)
		}
	}
}

// ownerOf calls dirOwner and reports a panic as a failure of its own, so a
// row that crashes the reader names itself and the rows after it still run.
func ownerOf(t *testing.T, sys any) (owner uint32, ok bool) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("dirOwner of %#v panicked: %v", sys, r)
		}
	}()

	return dirOwner(fakeInfo{sys: sys})
}

// A path the host refuses to report on for lack of permission reads as not
// readable, any other failure as cannot be read; neither prints the path.
func TestReadRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			"permission", &fs.PathError{Op: "lstat", Path: "/s", Err: fs.ErrPermission},
			"is not readable",
		},
		{
			"anything else", &fs.PathError{Op: "lstat", Path: "/s", Err: fs.ErrInvalid},
			"cannot be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := readRule(tc.err); got != tc.want {
				t.Errorf("readRule(%v): got %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
