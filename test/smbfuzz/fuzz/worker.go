//go:build linux

package fuzz

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/cubbit/dittofs/test/smbfuzz/sd"
)

// WorkerConfig is how a worker signs in (DESIGN.md §4.2).
type WorkerConfig struct {
	User     string // the plan's name for the user
	Account  string // the domain account, "alice"
	Domain   string // "DITTOFS"
	Password string // NTLM only
	Auth     string // "ntlm" or "krb5"
	Keytab   string // krb5: the user's keytab
	Realm    string // krb5
}

// RunWorker serves requests on in/out until in closes. It runs as the user's
// uid, which the orchestrator set when it started this process.
func RunWorker(cfg WorkerConfig, in io.Reader, out io.Writer) error {
	if err := signIn(cfg); err != nil {
		return fmt.Errorf("worker %s: sign in: %w", cfg.User, err)
	}
	w := &worker{slots: map[string]int{}}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req Request
		var rep Reply
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			rep.Fatal = err.Error()
		} else {
			rep = w.do(&req)
		}
		if err := enc.Encode(&rep); err != nil {
			return err
		}
	}
	return sc.Err()
}

// signIn gives this process its own session keyring holding the user's
// credential, which is where the kernel client looks when this uid first
// touches a multiuser mount.
func signIn(cfg WorkerConfig) error {
	// A new anonymous session keyring per worker: joining a named one would
	// find the keyring of a previous worker of the same user.
	if _, _, e := unix.Syscall(unix.SYS_KEYCTL, unix.KEYCTL_JOIN_SESSION_KEYRING, 0, 0); e != 0 {
		return fmt.Errorf("join session keyring: %w", e)
	}
	switch cfg.Auth {
	case "", "ntlm":
		payload := cfg.Account + ":" + cfg.Password
		if _, err := unix.AddKey("logon", "cifs:d:"+cfg.Domain, []byte(payload), unix.KEY_SPEC_SESSION_KEYRING); err != nil {
			return fmt.Errorf("add_key: %w", err)
		}
	case "krb5":
		// The credential cache is filled by kinit before the first request
		// (see the orchestrator), at the path cifs.upcall looks for.
	case "local":
		// No SMB: tests drive a worker against a local directory.
		return nil
	default:
		return fmt.Errorf("unknown auth %q", cfg.Auth)
	}
	return nil
}

type worker struct {
	slots map[string]int
}

func errnoOf(err error) int {
	if err == nil {
		return 0
	}
	var e unix.Errno
	if errors.As(err, &e) {
		return int(e)
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return errnoOf(pe.Err)
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return errnoOf(se.Err)
	}
	return int(unix.EIO)
}

func (w *worker) fd(slot string) (int, bool) {
	fd, ok := w.slots[slot]
	return fd, ok
}

func (w *worker) do(r *Request) Reply {
	out, err := w.exec(r)
	if err != nil {
		// An ACE edit returns what it read even when its write fails.
		return Reply{Errno: errnoOf(err), Out: out}
	}
	return Reply{Out: out}
}

// Pattern fills buf with a write's bytes (DESIGN.md §6.6). Byte i of a write is
// a function of i and the step, so a read can be checked without remembering
// the write.
func Pattern(name string, seed uint64, step int, buf []byte) {
	switch name {
	case "zero":
		clear(buf)
	case "ones":
		for i := range buf {
			buf[i] = 0xFF
		}
	case "seq":
		for i := range buf {
			buf[i] = byte((i + step) % 251)
		}
	case "rand":
		r := newRNG2(seed, uint64(step))
		for i := 0; i < len(buf); i += 8 {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], r.Uint64())
			copy(buf[i:], b[:])
		}
	default:
		clear(buf)
	}
}

const hashBlock = 64 << 10

func hashOut(buf []byte) *Output {
	sum := sha256.Sum256(buf)
	o := &Output{N: int64(len(buf)), Hash: hex.EncodeToString(sum[:8])}
	for i := 0; i < len(buf); i += hashBlock {
		end := min(i+hashBlock, len(buf))
		s := sha256.Sum256(buf[i:end])
		o.Blocks = append(o.Blocks, hex.EncodeToString(s[:6]))
	}
	return o
}

// The client's passthrough ioctl (CIFS_QUERY_INFO with PASSTHRU_FSCTL).
const (
	cifsQueryInfo  = 0xC018CF07 // _IOWR(0xCF, 7, struct smb_query_info), 24 bytes
	passthruFSCTL  = 1
	fsctlSetSparse = 0x000900C4
)

func setSparse(path string, on bool) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }() // opened read-only: a failed close loses nothing
	buf := make([]byte, 24+1)
	binary.LittleEndian.PutUint32(buf[0:], fsctlSetSparse) // info_type: the FSCTL
	binary.LittleEndian.PutUint32(buf[12:], passthruFSCTL) // flags
	binary.LittleEndian.PutUint32(buf[16:], 0)             // input_buffer_length: bytes to copy back
	binary.LittleEndian.PutUint32(buf[20:], 1)             // output_buffer_length: the request's input
	if on {
		buf[24] = 1
	}
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cifsQueryInfo, uintptr(unsafe.Pointer(&buf[0])))
	if e != 0 {
		return e
	}
	return nil
}

func statOut(st *unix.Statx_t) *StatOut {
	ts := func(t unix.StatxTimestamp) int64 { return t.Sec*1e9 + int64(t.Nsec) }
	typ := "other"
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		typ = "file"
	case unix.S_IFDIR:
		typ = "dir"
	case unix.S_IFLNK:
		typ = "symlink"
	}
	return &StatOut{
		Type: typ, Size: int64(st.Size), Nlink: st.Nlink, Ino: st.Ino,
		UID: st.Uid, GID: st.Gid, Perm: uint32(st.Mode) & 0o7777,
		Mtime: ts(st.Mtime), Ctime: ts(st.Ctime), Btime: ts(st.Btime),
		HasBtime: st.Mask&unix.STATX_BTIME != 0, Blocks: int64(st.Blocks),
	}
}

func timespec(t TimeArg) unix.Timespec {
	switch {
	case t.Omit:
		return unix.Timespec{Nsec: unix.UTIME_OMIT}
	case t.Now:
		return unix.Timespec{Nsec: unix.UTIME_NOW}
	}
	return unix.Timespec{Sec: t.Sec, Nsec: t.Nsec}
}

func getxattr(path, name string) ([]byte, error) {
	buf := make([]byte, 128<<10)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func aclXattr(part string) string {
	switch part {
	case "dacl":
		return "system.cifs_acl"
	case "ntsd":
		return "system.cifs_ntsd"
	case "ntsd_full":
		return "system.cifs_ntsd_full"
	case "owner":
		return "system.smb3_ntsd_owner"
	}
	return "system.cifs_ntsd"
}

func (w *worker) exec(r *Request) (*Output, error) {
	switch r.Op {
	case "open":
		if old, ok := w.slots[r.Slot2]; ok {
			_ = unix.Close(old)
			delete(w.slots, r.Slot2)
		}
		fd, err := unix.Open(r.Path, r.Flags|unix.O_CLOEXEC, r.Mode)
		if err != nil {
			return nil, err
		}
		w.slots[r.Slot2] = fd
		return nil, nil
	case "close":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		delete(w.slots, r.Slot)
		return nil, unix.Close(fd)
	case "pread":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		buf := make([]byte, r.Len)
		got := 0
		for got < len(buf) {
			n, err := unix.Pread(fd, buf[got:], r.Off+int64(got))
			if err != nil {
				return nil, err
			}
			if n == 0 {
				break
			}
			got += n
		}
		return hashOut(buf[:got]), nil
	case "pwrite", "write":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		buf := make([]byte, r.Len)
		Pattern(r.Data, r.Seed, r.Step, buf)
		done := 0
		for done < len(buf) || len(buf) == 0 {
			var n int
			var err error
			if r.Op == "pwrite" {
				n, err = unix.Pwrite(fd, buf[done:], r.Off+int64(done))
			} else {
				n, err = unix.Write(fd, buf[done:])
			}
			if err != nil {
				return nil, err
			}
			done += n
			if n == 0 {
				break
			}
		}
		return &Output{N: int64(done)}, nil
	case "lseek":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		n, err := unix.Seek(fd, r.Off, r.Whence)
		if err != nil {
			return nil, err
		}
		return &Output{N: n}, nil
	case "ftruncate":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		return nil, unix.Ftruncate(fd, r.Len)
	case "truncate":
		return nil, unix.Truncate(r.Path, r.Len)
	case "fallocate":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		return nil, unix.Fallocate(fd, r.Mode, r.Off, r.Len)
	case "setsparse":
		return nil, setSparse(r.Path, r.On)
	case "fsync", "fdatasync":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		if r.Op == "fsync" {
			return nil, unix.Fsync(fd)
		}
		return nil, unix.Fdatasync(fd)
	case "copy_file_range", "ficlone_range":
		in, ok1 := w.fd(r.Slot)
		out, ok2 := w.fd(r.Slot2)
		if !ok1 || !ok2 {
			return nil, unix.EBADF
		}
		if r.Op == "ficlone_range" {
			return nil, unix.IoctlFileCloneRange(out, &unix.FileCloneRange{
				Src_fd: int64(in), Src_offset: uint64(r.Off), Src_length: uint64(r.Len), Dest_offset: uint64(r.Off2),
			})
		}
		offIn, offOut := r.Off, r.Off2
		n, err := unix.CopyFileRange(in, &offIn, out, &offOut, int(r.Len), 0)
		if err != nil {
			return nil, err
		}
		return &Output{N: int64(n)}, nil
	case "mkdir":
		return nil, unix.Mkdir(r.Path, r.Mode)
	case "rmdir":
		return nil, unix.Rmdir(r.Path)
	case "unlink":
		return nil, unix.Unlink(r.Path)
	case "rename":
		return nil, unix.Renameat2(unix.AT_FDCWD, r.Path, unix.AT_FDCWD, r.Path2, uint(r.Flags))
	case "link":
		return nil, unix.Link(r.Path, r.Path2)
	case "statfs":
		var st unix.Statfs_t
		return nil, unix.Statfs(r.Path, &st)
	case "mkfiles":
		for i := r.Off; i < r.Off+r.Len; i++ {
			fd, err := unix.Open(r.Path+"/"+FillName(int(i)), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o644)
			if err != nil {
				return nil, err
			}
			if err := unix.Close(fd); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "readdir":
		f, err := os.Open(r.Path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }() // a directory read: a failed close loses nothing
		ents, err := f.ReadDir(-1)
		if err != nil {
			return nil, err
		}
		o := &Output{}
		for _, e := range ents {
			t := "other"
			switch {
			case e.Type().IsDir():
				t = "dir"
			case e.Type().IsRegular():
				t = "file"
			case e.Type()&os.ModeSymlink != 0:
				t = "symlink"
			}
			o.Dir = append(o.Dir, DirEnt{Name: e.Name(), Type: t})
		}
		sort.Slice(o.Dir, func(i, j int) bool { return o.Dir[i].Name < o.Dir[j].Name })
		return o, nil
	case "stat", "lstat", "fstat":
		var st unix.Statx_t
		var err error
		mask := unix.STATX_BASIC_STATS | unix.STATX_BTIME
		switch r.Op {
		case "stat":
			err = unix.Statx(unix.AT_FDCWD, r.Path, 0, mask, &st)
		case "lstat":
			err = unix.Statx(unix.AT_FDCWD, r.Path, unix.AT_SYMLINK_NOFOLLOW, mask, &st)
		default:
			fd, ok := w.fd(r.Slot)
			if !ok {
				return nil, unix.EBADF
			}
			err = unix.Statx(fd, "", unix.AT_EMPTY_PATH, mask, &st)
		}
		if err != nil {
			return nil, err
		}
		return &Output{Stat: statOut(&st)}, nil
	case "utimensat":
		return nil, unix.UtimesNanoAt(unix.AT_FDCWD, r.Path, []unix.Timespec{timespec(r.ATime), timespec(r.MTime)}, 0)
	case "getdosattr", "getbtime":
		name := "user.cifs.dosattrib"
		if r.Op == "getbtime" {
			name = "user.cifs.creationtime"
		}
		b, err := getxattr(r.Path, name)
		if err != nil {
			return nil, err
		}
		return &Output{Bytes: b}, nil
	case "setdosattr":
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], r.Attrs)
		return nil, unix.Setxattr(r.Path, "user.cifs.dosattrib", b[:], 0)
	case "setbtime":
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], r.BTime)
		return nil, unix.Setxattr(r.Path, "user.cifs.creationtime", b[:], 0)
	case "getxattr":
		b, err := getxattr(r.Path, r.XName)
		if err != nil {
			return nil, err
		}
		return &Output{Bytes: b}, nil
	case "setxattr":
		return nil, unix.Setxattr(r.Path, r.XName, r.XValue, r.Flags)
	case "removexattr":
		return nil, unix.Removexattr(r.Path, r.XName)
	case "listxattr":
		buf := make([]byte, 128<<10)
		n, err := unix.Listxattr(r.Path, buf)
		if err != nil {
			return nil, err
		}
		o := &Output{Names: []string{}}
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name == "" || strings.HasPrefix(name, "user.cifs.") || strings.HasPrefix(name, "user.smb3.") {
				continue
			}
			o.Names = append(o.Names, name)
		}
		sort.Strings(o.Names)
		return o, nil
	case "chmod":
		return nil, unix.Chmod(r.Path, r.Mode)
	case "chown":
		return nil, unix.Chown(r.Path, r.UID, r.GID)
	case "lock", "unlock", "testlock":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		lk := unix.Flock_t{Type: r.LockType, Whence: unix.SEEK_SET, Start: r.Off, Len: r.Len}
		if r.Op == "unlock" {
			lk.Type = unix.F_UNLCK
		}
		cmd := unix.F_OFD_SETLK
		if r.Op == "testlock" {
			cmd = unix.F_OFD_GETLK
		}
		if err := unix.FcntlFlock(uintptr(fd), cmd, &lk); err != nil {
			return nil, err
		}
		if r.Op == "testlock" {
			return &Output{Lock: &LockOut{Type: lk.Type, Start: lk.Start, Len: lk.Len}}, nil
		}
		return nil, nil
	case "flock":
		fd, ok := w.fd(r.Slot)
		if !ok {
			return nil, unix.EBADF
		}
		return nil, unix.Flock(fd, r.Flags)
	case "getacl":
		b, err := getxattr(r.Path, aclXattr(r.Part))
		if err != nil {
			return nil, err
		}
		return &Output{Bytes: b}, nil
	case "setacl", "setowner":
		return nil, unix.Setxattr(r.Path, aclXattr(r.Part), r.SD, 0)
	case "aceadd", "acedel", "aceedit":
		return editACL(r)
	case "noop":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown op %q: %w", r.Op, unix.ENOSYS)
}

// aceIndex resolves an ACE position against a list of n entries: "0" and "1"
// as written, "last" the last entry, "end" one past it.
func aceIndex(index string, n int) int {
	switch index {
	case "last":
		return n - 1
	case "end":
		return n
	case "1":
		return 1
	}
	return 0
}

// editACL reads the descriptor part, inserts, removes or changes one ACE, and
// writes it back, the way a tool that grants or revokes one entry does: the
// entries the server returned, inherited ones included, are written back as
// read. It returns the descriptor as read, so a difference in the starting
// point shows before the write. A position past the list is refused with
// ENOENT by the worker itself, alike on both sides when the lists agree.
func editACL(r *Request) (*Output, error) {
	name := aclXattr(r.Part)
	before, err := getxattr(r.Path, name)
	if err != nil {
		return nil, err
	}
	out := &Output{Bytes: before}
	d, err := sd.Unmarshal(before)
	if err != nil {
		return out, unix.EBADMSG
	}
	if err := applyACEEdit(d, r); err != nil {
		return out, err
	}
	return out, unix.Setxattr(r.Path, name, d.Marshal(), 0)
}

// applyACEEdit inserts, removes or changes the one entry r names in d.
func applyACEEdit(d *sd.SD, r *Request) error {
	switch r.Op {
	case "aceadd":
		add, err := sd.Unmarshal(r.SD)
		if err != nil {
			return unix.EINVAL
		}
		// The entry comes as a one-entry descriptor: in its SACL when it is
		// an audit or label entry, in its DACL otherwise.
		target, from := &d.DACL, add.DACL
		if add.SACL != nil && len(add.SACL.ACEs) > 0 {
			target, from = &d.SACL, add.SACL
		}
		if from == nil || len(from.ACEs) == 0 {
			return unix.EINVAL
		}
		if *target == nil {
			*target = &sd.ACL{} // a null DACL becomes a list with one entry
		}
		l := *target
		i := min(max(aceIndex(r.Index, len(l.ACEs)), 0), len(l.ACEs))
		l.ACEs = append(l.ACEs[:i], append([]sd.ACE{from.ACEs[0]}, l.ACEs[i:]...)...)
	case "acedel", "aceedit":
		if d.DACL == nil {
			return unix.ENOENT
		}
		i := aceIndex(r.Index, len(d.DACL.ACEs))
		if i < 0 || i >= len(d.DACL.ACEs) {
			return unix.ENOENT
		}
		if r.Op == "acedel" {
			d.DACL.ACEs = append(d.DACL.ACEs[:i], d.DACL.ACEs[i+1:]...)
		} else {
			d.DACL.ACEs[i].Mask, d.DACL.ACEs[i].Flags = r.AceMask, r.AceFlags
		}
	}
	return nil
}
