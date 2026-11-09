//go:build linux

package fuzz

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// refOnly are the probes that check the client and the reference, not
// DittoFS: whether the server, rather than the client, decides access is a
// property of the mount options, the same for both, and of Samba's
// configuration. On DittoFS the same behaviour is under test, so a failure
// there is a divergence for the rounds to find, not a broken fixture.
var refOnly = map[string]bool{
	"the server refuses alice a read the mode would allow": true,
	"the server lets bob read":                             true,
}

// Probe runs DESIGN.md §3.6's checks. A failed check means the fixture cannot
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
		on := sides
		if refOnly[what] {
			on = []string{SideRef}
		}
		for _, side := range on {
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
			if rep.Fatal != "" || rep.Errno != want {
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
		{"the lock file has content", "alice", Request{Op: "pwrite", Slot: "alice.probe", Off: 0, Len: 20, Data: "ones"}, 0},
		{"alice locks it", "alice", Request{Op: "lock", Slot: "alice.probe", LockType: unix.F_WRLCK, Off: 0, Len: 10}, 0},
		{"bob opens it", "bob", Request{Op: "open", Path: "alice/lockf", Flags: unix.O_RDWR}, 0},
		// decision: the probe writes over the lock rather than taking a
		// conflicting one. The cifs client keeps the locks every open of
		// an inode holds on its mount and refuses a conflicting lock
		// itself, before either server sees it, while a write with
		// cache=none goes to the server unchecked. A write, not a read:
		// Samba's default strict locking skips the check of a read made
		// under a read oplock, which bob may hold. So a lock-against-lock
		// conflict between two slots of a plan is settled by the client on
		// both sides alike (DESIGN.md §7.5); probe it again once each user
		// has a mount, or a client, of its own.
		{"the server refuses bob a write over alice's lock", "bob", Request{Op: "pwrite", Slot: "bob.probe", Off: 5, Len: 10, Data: "zero"}, int(unix.EACCES)},
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
