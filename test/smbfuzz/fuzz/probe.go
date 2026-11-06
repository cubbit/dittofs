//go:build linux

package fuzz

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const lockRefused = -100

// Probe runs RFC 31 §3.6's checks. A failed check means the fixture cannot
// tell the two servers apart for the reason it was written for, so nothing
// may run.
func (r *Runner) Probe() error {
	const dir = "probe"
	for _, side := range sides {
		r.removeTree(side, filepath.Join(r.root(side), dir))
	}
	defer func() {
		r.Close()
		for _, side := range sides {
			r.removeTree(side, filepath.Join(r.root(side), dir))
		}
	}()
	sdOf := func(text string) []byte {
		d, err := r.resolver.Parse(text)
		if err != nil {
			panic(err)
		}
		return d.Marshal()
	}
	expect := func(what, user string, req Request, want int) error {
		for _, side := range sides {
			q := req
			q.Side = side
			q.Path = filepath.Join(r.root(side), dir, req.Path)
			if q.Op == "open" {
				q.Slot2 = side + ":" + user + ".probe"
			}
			if q.Slot != "" {
				q.Slot = side + ":" + req.Slot
			}
			rep := r.call(user, &q)
			ok := rep.Errno == want
			if want == lockRefused {
				// fcntl may answer a lock conflict either way; the client
				// answers EACCES.
				ok = rep.Errno == int(unix.EACCES) || rep.Errno == int(unix.EAGAIN)
			}
			if rep.Fatal != "" || !ok {
				return fmt.Errorf("probe %q on %s: got %s%s, want %s", what, side, errName(rep.Errno), rep.Fatal, errName(want))
			}
		}
		return nil
	}
	steps := []struct {
		what, user string
		req        Request
		want       int
	}{
		{"admin creates the probe directory", "admin", Request{Op: "mkdir", Path: "", Mode: 0o777}, 0},
		{"admin opens the probe directory to everyone", "admin", Request{Op: "setacl", Path: "", Part: "dacl", SD: sdOf("D:(A;OICI;FA;;;WD)")}, 0},
	}
	for _, u := range []string{"alice", "bob", "carol", "admin"} {
		steps = append(steps,
			struct {
				what, user string
				req        Request
				want       int
			}{u + " has a session", u, Request{Op: "mkdir", Path: u, Mode: 0o777}, 0})
	}
	steps = append(steps, []struct {
		what, user string
		req        Request
		want       int
	}{
		{"a file", "admin", Request{Op: "open", Path: "denied", Flags: unix.O_CREAT | unix.O_RDWR, Mode: 0o644}, 0},
		{"the file is closed", "admin", Request{Op: "close", Slot: "admin.probe"}, 0},
		{"deny alice read", "admin", Request{Op: "setacl", Path: "denied", Part: "dacl", SD: sdOf("D:(D;;FR;;;alice)(A;;FA;;;WD)")}, 0},
		{"the server refuses alice a read the mode would allow", "alice", Request{Op: "open", Path: "denied", Flags: unix.O_RDONLY}, int(unix.EACCES)},
		{"allow bob read only", "admin", Request{Op: "setacl", Path: "denied", Part: "dacl", SD: sdOf("D:(A;;FR;;;bob)(A;;FA;;;Administrator)")}, 0},
		{"the server lets bob read", "bob", Request{Op: "open", Path: "denied", Flags: unix.O_RDONLY}, 0},
		{"the descriptor reads back", "admin", Request{Op: "getacl", Path: "denied", Part: "ntsd"}, 0},
		{"a lock file", "alice", Request{Op: "open", Path: "alice/lockf", Flags: unix.O_CREAT | unix.O_RDWR, Mode: 0o644}, 0},
		{"alice locks it", "alice", Request{Op: "lock", Slot: "alice.probe", LockType: unix.F_WRLCK, Off: 0, Len: 10}, 0},
		{"bob opens it", "bob", Request{Op: "open", Path: "alice/lockf", Flags: unix.O_RDWR}, 0},
		{"bob's conflicting lock reaches the server and fails", "bob", Request{Op: "lock", Slot: "bob.probe", LockType: unix.F_WRLCK, Off: 5, Len: 10}, lockRefused},
		{"alice closes", "alice", Request{Op: "close", Slot: "alice.probe"}, 0},
		{"bob closes", "bob", Request{Op: "close", Slot: "bob.probe"}, 0},
		{"DOS attributes can be set", "alice", Request{Op: "setdosattr", Path: "alice/lockf", Attrs: 0x2}, 0},
		{"the admin reads a SACL on the reference", "admin", Request{Op: "noop"}, 0},
	}...)
	for _, s := range steps {
		if err := expect(s.what, s.user, s.req, s.want); err != nil {
			return err
		}
	}
	// The reference only: the security privilege took effect (§7.6). DittoFS
	// is not probed for it: there a refusal is a finding.
	rep := r.call("admin", &Request{Side: SideRef, Op: "getacl", Path: filepath.Join(r.root(SideRef), dir, "denied"), Part: "ntsd_full"})
	if rep.Errno != 0 {
		return fmt.Errorf("probe: the admin cannot read a SACL on the reference (%s): the security privilege is not granted", errName(rep.Errno))
	}
	for _, side := range sides {
		rep := r.call("admin", &Request{Side: side, Op: "getdosattr", Path: filepath.Join(r.root(side), dir, "alice/lockf")})
		if rep.Errno != 0 || le32(rep.Out.Bytes)&0x2 == 0 {
			return fmt.Errorf("probe: HIDDEN did not read back on %s", side)
		}
	}
	return nil
}
