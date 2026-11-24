package platform

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// The statfs filesystem types the probe tells apart. Each is pinned to its
// golang.org/x/sys constant by a Linux test; they are declared here so the
// verdict builds and is tested on every platform.
const (
	// magicExt is the type of ext2, ext3 and ext4, which share one magic.
	magicExt uint32 = 0xef53
	magicXFS uint32 = 0x58465342
	// magicBtrfs is Btrfs, the copy-on-write filesystem of RFC 28 Appendix A.
	magicBtrfs uint32 = 0x9123683e
)

// fsNoCOW is the inode flag FS_IOC_GETFLAGS reports for a file or directory
// with copy-on-write disabled (FS_NOCOW_FL in the kernel's linux/fs.h).
const fsNoCOW uint32 = 0x00800000

// fsJournalData is the inode flag FS_IOC_GETFLAGS reports for a file or
// directory whose writes run through the filesystem's own journal
// (FS_JOURNAL_DATA_FL in the kernel's linux/fs.h).
const fsJournalData uint32 = 0x00004000

// btrfsSubvolumeRootIno is the inode number of every Btrfs subvolume's root
// directory (BTRFS_FIRST_FREE_OBJECTID in the kernel's linux/btrfs_tree.h).
const btrfsSubvolumeRootIno uint64 = 256

// refusalRule names the rule of RFC 28 Appendix A that refused a directory.
type refusalRule uint8

const (
	// ruleBarrier: the directory's mount turns cache flushes off.
	ruleBarrier refusalRule = iota + 1
	// ruleSubvolume: on Btrfs, the directory is not a subvolume's root.
	ruleSubvolume
	// ruleCopyOnWrite: on Btrfs, the directory or a file in it does not have
	// copy-on-write disabled.
	ruleCopyOnWrite
	// ruleCachePolicy: on Windows, the directory's volume has write-cache
	// buffer flushing turned off by policy.
	ruleCachePolicy
	// ruleDataJournal: on ext, the mount runs data journaling — the
	// data=journal mode — or the directory or a file in it has the
	// journal-data attribute set.
	ruleDataJournal
	// ruleKernel: the kernel is older than 4.13, where ext4, XFS and Btrfs
	// each began recording a writeback error per file.
	ruleKernel
)

// refusal is a directory refused by a rule of RFC 28 Appendix A.
type refusal struct {
	rule refusalRule
	// dir is the journal directory.
	dir string
	// mount is the mount point holding dir, for ruleBarrier, or the root of
	// dir's volume, for ruleCachePolicy.
	mount string
	// option is the refused mount option, for ruleBarrier and ruleDataJournal's
	// mode, or the kernel release, for ruleKernel.
	option string
	// path is what the rule refused: the directory for ruleSubvolume, the
	// directory or the file holding the attribute for ruleCopyOnWrite and
	// ruleDataJournal, the volume GUID path for ruleCachePolicy.
	path string
}

// Is reports whether target is core.ErrUnsuitableStorage: a refusal is the
// directory the caller may fix, and every rule's refusal answers the one
// sentinel. An error met while checking matches nothing here, so a caller
// can tell a fault from a refusal.
func (r *refusal) Is(target error) bool {
	return target == core.ErrUnsuitableStorage
}

func (r *refusal) Error() string {
	switch r.rule {
	case ruleBarrier:
		return fmt.Sprintf("journal: directory %s refused: mount %s has %s, so no sync reaches the device; remount with barriers",
			r.dir, r.mount, r.option)
	case ruleSubvolume:
		return fmt.Sprintf("journal: directory %s refused: on Btrfs it must be its own subvolume, which snapshots of its parent do not cover; make the directory a subvolume root",
			r.dir)
	case ruleCopyOnWrite:
		if r.path == r.dir {
			return fmt.Sprintf("journal: directory %s refused: copy-on-write is on; set chattr +C on the directory while it is empty",
				r.dir)
		}
		return fmt.Sprintf("journal: directory %s refused: file %s keeps copy-on-write, as it was created before chattr +C was set on the directory; "+
			"copy it into a new file created in the directory, which inherits the attribute, and rename that over it, "+
			"or recreate the journal in a directory that had chattr +C set while empty",
			r.dir, r.path)
	case ruleDataJournal:
		// The mode and the attribute are one rule: both run a sync's data
		// through the filesystem's own journal, where a failure to write it
		// to its place is reported to a later sync, if at all.
		if r.option != "" {
			return fmt.Sprintf("journal: directory %s refused: mount %s runs ext4 data journaling (%s), so a sync returns once the data is in the filesystem's own journal and a write that never reached its place fails a later one; remount with data=ordered or data=writeback",
				r.dir, r.mount, r.option)
		}
		if r.path == r.dir {
			return fmt.Sprintf("journal: directory %s refused: the directory has the journal-data attribute set, so its writes run through ext4 data journaling; clear it with chattr -j on the directory while it is empty",
				r.dir)
		}
		return fmt.Sprintf("journal: directory %s refused: file %s has the journal-data attribute set, so its writes run through ext4 data journaling; the attribute is inherited at creation only, and it is set on files one at a time (chattr +j), so each file is checked alone; clear it with chattr -j on the file",
			r.dir, r.path)
	case ruleKernel:
		return fmt.Sprintf("journal: directory %s refused: kernel %s is older than 4.13, where ext4, XFS and Btrfs each began recording a writeback error per file and reporting it to every descriptor open when it happened; on an earlier kernel the error reaches the first sync through any descriptor and is cleared, so a write that failed reaches no proof; boot a kernel of 4.13 or newer",
			r.dir, r.option)
	case ruleCachePolicy:
		// The action uses Microsoft's words: Device Manager, the disk's
		// Policies tab (How to manually turn disk write caching on or off,
		// https://learn.microsoft.com/previous-versions/troubleshoot/windows-server/turn-disk-write-caching-on-off),
		// and the check box "Turn off Windows write-cache buffer flushing on
		// the device" (Performance Tuning Remote Desktop Session Hosts,
		// https://learn.microsoft.com/windows-server/administration/performance-tuning/role/remote-desktop/session-hosts).
		return fmt.Sprintf("journal: directory %s refused: volume %s (%s) has Windows write-cache buffer flushing turned off by policy, "+
			"so no sync reaches the device; in Device Manager, open the disk's Policies tab and clear "+
			"\"Turn off Windows write-cache buffer flushing on the device\"",
			r.dir, r.mount, r.path)
	}
	return fmt.Sprintf("journal: directory %s refused", r.dir)
}

// The errors the verdict returns when its inputs do not describe the
// directory. None is a refusal: each fails the check without naming a rule.
var (
	// errDirectoryNotClean: the directory path is not absolute and clean.
	// The probe promises an absolute, cleaned path with its symbolic links
	// resolved, and the walk reads it as text. Cleaning the text here would
	// not help: after a symbolic link the kernel resolves ".." from the
	// link's target, which the text does not show. So such a path does not
	// describe the directory, and the check fails.
	errDirectoryNotClean = errors.New("journal: the directory path is not absolute and clean")
	// errMountTableMalformed: a line of the mount table does not parse.
	errMountTableMalformed = errors.New("journal: mount table does not parse")
	// errMountUnresolved: the table has no single root mount, mounts at one
	// point form a cycle or two of them are tied, or the directory's mount
	// ID names no mount of the table.
	errMountUnresolved = errors.New("journal: no mount of the table holds the directory")
	// errMountIDDisagrees: the directory's mount ID names another mount than
	// the one the table makes visible at its path.
	errMountIDDisagrees = errors.New("journal: the directory's mount is not the one visible at its path")
	// errMountTypeMismatch: statfs and the table name different filesystems.
	errMountTypeMismatch = errors.New("journal: mount table and statfs name different filesystems")
	// errKernelRelease: the kernel release is not empty but its first two
	// numbers do not parse. The probe passes uname's text unread, so a
	// release the verdict cannot read numbers from says the inputs do not
	// describe the kernel: the check fails rather than refusing on a guess.
	errKernelRelease = errors.New("journal: the kernel release does not parse")
)

// CheckDir decides whether dir is suitable for a journal. It returns an
// error matching core.ErrUnsuitableStorage for the first rule of the
// platform profile a directory refuses — a mount without cache flushes, a
// Btrfs directory that is not a subvolume root or keeps copy-on-write —
// with its message naming the rule, the directory and what to change; any
// other error as itself, which says nothing about whether the directory is
// suitable; and nil for a suitable one.
//
// The verdict runs on gathered inputs, so it builds on every platform.
// gather, when not nil, is what reads those inputs for the check; nil takes
// the platform's own check, the real probe on Linux and none where the
// profile names no rule the platform can read, whose answer accepts every
// directory. A gather's error is returned as itself, never as a refusal.
func CheckDir(dir string, gather storage.Gather) error {
	if gather == nil {
		return checkDirOS(dir)
	}
	p, err := gather(dir)
	if err != nil {
		return err
	}
	return CheckProbe(p)
}

// listedFS is a filesystem RFC 28 Appendix A lists as suitable on Linux:
// its statfs magic and the mount table types that report it.
type listedFS struct {
	magic uint32
	types []string
}

// listedFilesystems are ext4, XFS and Btrfs. ext2 and ext3 report the same
// magic as ext4, 0xef53: ext2 has its own driver, which reports
// EXT2_SUPER_MAGIC (fs/ext2/super.c, ext2_statfs), and ext3 is served by the
// ext4 driver (fs/ext4/Kconfig: EXT3_FS selects EXT4_FS).
var listedFilesystems = [...]listedFS{
	{magicExt, []string{"ext2", "ext3", "ext4"}},
	{magicXFS, []string{"xfs"}},
	{magicBtrfs, []string{"btrfs"}},
}

// decision: copy-on-write is detected on Btrfs only, the copy-on-write
// filesystem RFC 28 Appendix A names ("On a copy-on-write filesystem, such as
// Btrfs"). The filesystem is the one statfs names by its magic, not the type
// the mount table writes, so ext2, ext3 and ext4 are one filesystem here. Cache
// flushes are checked against this table, one row for each MUST of RFC 28
// Appendix A: "Open MUST refuse a copy-on-write filesystem mounted without
// cache flushes (nobarrier on Btrfs)" and "Open MUST refuse a filesystem
// mounted without cache flushes where the mount options say so (nobarrier or
// barrier=0 on ext4, nobarrier on older XFS)". A row is added when the RFC
// names another option, and the Btrfs rules extend to another filesystem when
// the RFC names it as copy-on-write.
var barrierRows = [...]struct {
	magic  uint32
	option string
}{
	{magicExt, "nobarrier"},
	{magicExt, "barrier=0"},
	{magicXFS, "nobarrier"},
	{magicBtrfs, "nobarrier"},
}

// CheckProbe decides whether the directory p describes is suitable for a
// journal under RFC 28 Appendix A. It returns a *refusal for the first rule
// that fails, in the order kernel release, mount options, ext effective
// options, subvolume root, directory attribute, files; errDirectoryNotClean
// or one of the errMount errors when the inputs do not describe the
// directory; and nil otherwise. A filesystem outside the listed ones, on a
// mount line of a type outside them too, is not refused — though the kernel
// rule still runs, as its MUST is unqualified.
//
// It runs on the gathered inputs alone, so it builds and is tested on every
// platform: the Linux probe is the one Gather that reads them, but the
// verdict answers any Probe, checked under another platform's rules the day
// it names ones.
func CheckProbe(p storage.Probe) error {
	mounts, err := parseMountinfo(p.Mountinfo)
	if err != nil {
		return err
	}

	m, err := visibleMount(mounts, p.Dir)
	if err != nil {
		return err
	}

	if p.MountID != 0 {
		if !hasMount(mounts, p.MountID) {
			return fmt.Errorf("%w: mount ID %d", errMountUnresolved, p.MountID)
		}
		if p.MountID != m.id {
			return fmt.Errorf("%w: mount ID %d, visible mount %d at %s", errMountIDDisagrees, p.MountID, m.id, m.point)
		}
	}

	// decision: an empty KernelRelease passes because it means the probe
	// read none — the Linux probe always reads the release; a gather
	// recorded before it did answers without one — as a mount ID of 0 means
	// a kernel that reports none: a missing answer refuses nothing, while
	// refusing on it would fail every gather the kernel rule cannot see.
	// Withdraw the pass when every Gather must report the kernel.
	if p.KernelRelease != "" {
		major, minor, ok := kernelVersion(p.KernelRelease)
		if !ok {
			return fmt.Errorf("%w: %q", errKernelRelease, p.KernelRelease)
		}
		if major < 4 || (major == 4 && minor < 13) {
			return &refusal{rule: ruleKernel, dir: p.Dir, option: p.KernelRelease}
		}
	}

	// statfs and the mount table read one superblock: vfs_statfs
	// (fs/statfs.c) asks the superblock of the directory's dentry, and
	// show_type (fs/proc_namespace.c) writes the type of the mount's
	// superblock. They disagree only when the selected line is not the
	// directory's mount, so a disagreement either way fails: a listed magic
	// on a line of another type, and a magic outside the list on a line of a
	// listed type, whose options would then be read for the wrong mount.
	fs, listed := listedByMagic(p.FSType)
	if !listed {
		if listedType(m.fsType) {
			return fmt.Errorf("%w: statfs type %#x, mount %s has type %s", errMountTypeMismatch, p.FSType, m.point, m.fsType)
		}

		// decision: a filesystem outside the list passes: the RFC names the
		// suitable ones but gives no MUST to refuse the rest, and test
		// temporary directories are often tmpfs. Overlay hides a nobarrier or
		// copy-on-write filesystem below it. Revisit if the RFC names an
		// unlisted copy-on-write filesystem such as ZFS.
		return nil
	}
	if !fs.reports(m.fsType) {
		return fmt.Errorf("%w: statfs type %#x, mount %s has type %s", errMountTypeMismatch, p.FSType, m.point, m.fsType)
	}

	if option, ok := refusedOption(p.FSType, m.superOptions); ok {
		return &refusal{rule: ruleBarrier, dir: p.Dir, mount: m.point, option: option}
	}

	// The ext4 driver reports the options the mount runs with in its own
	// file, which an option equal to the superblock's default never reaches
	// in the mount table (_ext4_show_options in fs/ext4/super.c), so the
	// barrier and the data-journal options are read from it there. ext3 is
	// served by the ext4 driver (fs/ext4/Kconfig: EXT3_FS selects EXT4_FS)
	// and is checked the same.
	if p.FSType == magicExt {
		switch m.fsType {
		case "ext2":
			// decision: ext2 has its own driver (fs/ext2), which registers
			// nothing under /proc/fs/ext4, so the probe reads no options
			// file for it: the barrier rows stay read from the mount table
			// above, and the ext-options rules are skipped, since the
			// absent file's error names no effective options. Withdraw it if
			// the ext2 driver grows an options file.
		case "ext3", "ext4":
			// An unreadable options file is an error of the check, never a
			// refusal and never a pass: the effective options were never
			// seen, so the mount is not known clean.
			if p.ExtOptionsErr != nil {
				return p.ExtOptionsErr
			}
			words := extOptionWords(p.ExtOptions)
			if option, ok := refusedExtOption(words); ok {
				return &refusal{rule: ruleBarrier, dir: p.Dir, mount: m.point, option: option}
			}
			if slices.Contains(words, "data=journal") {
				return &refusal{rule: ruleDataJournal, dir: p.Dir, mount: m.point, option: "data=journal"}
			}
		}
	}

	// The journal-data attribute is inherited at creation only, so it is set
	// on one file alone (chattr +j) and a file created before it was set on
	// the directory keeps what it was born with: the directory and every
	// file are asked on their own, as the copy-on-write attribute below is.
	if p.FSType == magicExt {
		if p.DirFlags&fsJournalData != 0 {
			return &refusal{rule: ruleDataJournal, dir: p.Dir, path: p.Dir}
		}
		for _, f := range p.Files {
			if f.Flags&fsJournalData != 0 {
				return &refusal{rule: ruleDataJournal, dir: p.Dir, path: joinPath(p.Dir, f.Name)}
			}
		}
		return nil
	}

	if p.FSType != magicBtrfs {
		return nil
	}
	if p.DirIno != btrfsSubvolumeRootIno {
		return &refusal{rule: ruleSubvolume, dir: p.Dir, path: p.Dir}
	}
	if p.DirFlags&fsNoCOW == 0 {
		return &refusal{rule: ruleCopyOnWrite, dir: p.Dir, path: p.Dir}
	}
	for _, f := range p.Files {
		if f.Flags&fsNoCOW == 0 {
			return &refusal{rule: ruleCopyOnWrite, dir: p.Dir, path: joinPath(p.Dir, f.Name)}
		}
	}
	return nil
}

func listedByMagic(magic uint32) (listedFS, bool) {
	for _, fs := range listedFilesystems {
		if fs.magic == magic {
			return fs, true
		}
	}
	return listedFS{}, false
}

// listedType says whether a mount table type is one a listed filesystem
// reports.
func listedType(fsType string) bool {
	for _, fs := range listedFilesystems {
		if fs.reports(fsType) {
			return true
		}
	}
	return false
}

// reports says whether a mount table type is one this filesystem reports.
func (fs listedFS) reports(fsType string) bool {
	return slices.Contains(fs.types, fsType)
}

// kernelVersion returns the first two numbers of a kernel release, the ones
// the kernel rule decides on. uname(2) fills utsname.release with
// "X.Y[.Z...][suffix]", where X and Y alone stand for the kernel's release,
// so the rest — a patch level, a build suffix such as "-76-generic" — is not
// read. The numbers are compared as a tuple, never as one: 4.13 follows 4.9,
// as 13 does not follow 9. A component's leading digits alone are its
// number, as a development release writes the minor as "13-rc1".
func kernelVersion(release string) (major, minor int, ok bool) {
	first, rest, found := strings.Cut(release, ".")
	if !found {
		return 0, 0, false
	}
	second := rest
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		second = rest[:i]
	}
	major, okMajor := leadingNumber(first)
	minor, okMinor := leadingNumber(second)
	return major, minor, okMajor && okMinor
}

// leadingNumber returns the digits at the start of s; a component with none
// is no number a release writes.
func leadingNumber(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// extOptionWords splits the effective options the ext4 driver reports — it
// writes them comma-separated on one line (ext4_seq_options_show in
// fs/ext4/super.c) — into the words the rows match whole, as refusedOption
// matches the mount table's.
func extOptionWords(options string) []string {
	return strings.FieldsFunc(options, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// refusedExtOption returns the first word of the effective options a barrier
// row for ext refuses.
func refusedExtOption(words []string) (string, bool) {
	for _, w := range words {
		for _, row := range barrierRows {
			if row.magic == magicExt && row.option == w {
				return w, true
			}
		}
	}
	return "", false
}

// refusedOption returns the first option of superOptions a barrier row of
// magic refuses. Options are compared as whole comma-separated words.
//
// Btrfs writes subvol= last in every release from Linux 4.5 to 6.17
// (fs/btrfs/super.c, btrfs_show_options). Up to Linux 6.14 it escapes the
// value with " \t\n\\", which leaves a comma as it is; from 6.15 it writes it
// with seq_show_option, which escapes the comma too (include/linux/seq_file.h).
// The options are cut at subvol= first, so on every release a subvolume's
// name is not an option.
func refusedOption(magic uint32, superOptions string) (string, bool) {
	if magic == magicBtrfs {
		superOptions, _, _ = strings.Cut(superOptions, ",subvol=")
	}
	for option := range strings.SplitSeq(superOptions, ",") {
		for _, row := range barrierRows {
			if row.magic == magic && row.option == option {
				return option, true
			}
		}
	}
	return "", false
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// mountEntry is one line of /proc/self/mountinfo, with the fields the verdict
// reads.
type mountEntry struct {
	id, parent   uint64
	point        string
	fsType       string
	superOptions string
}

// parseMountinfo reads every line of a mount table. The layout is proc(5)'s,
// /proc/[pid]/mountinfo: mount ID, parent ID, major:minor, root, mount point,
// per-mount options, zero or more optional fields, a lone "-", then the
// filesystem type, the mount source and the superblock options. The barrier
// options are superblock options: the per-mount field carries only the VFS
// flags (show_vfsmnt_opts in fs/proc_namespace.c). A line whose structure
// does not parse fails the whole table, wherever it is.
//
// The superblock options are the last field and may hold unescaped spaces:
// 9p writes uname= and aname= as given (fs/9p/v9fs.c, v9fs_show_options), so
// everything after the mount source is read as that one field.
func parseMountinfo(text string) ([]mountEntry, error) {
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	mounts := make([]mountEntry, 0, len(lines))
	for n, line := range lines {
		m, ok := parseMountLine(line)
		if !ok {
			return nil, fmt.Errorf("%w: line %d: %q", errMountTableMalformed, n+1, line)
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

func parseMountLine(line string) (mountEntry, bool) {
	fields := strings.Split(line, " ")
	// The separator follows the six fixed fields and any optional ones, then
	// come the filesystem type, the mount source and the superblock options.
	// Every field before the options is escaped by the kernel, so the first
	// lone "-" is the separator.
	sep := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || len(fields) < sep+4 {
		return mountEntry{}, false
	}

	id, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return mountEntry{}, false
	}
	parent, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return mountEntry{}, false
	}
	point, ok := unescapeMountPoint(fields[4])
	if !ok || !strings.HasPrefix(point, "/") {
		return mountEntry{}, false
	}

	return mountEntry{
		id:           id,
		parent:       parent,
		point:        point,
		fsType:       fields[sep+1],
		superOptions: strings.Join(fields[sep+3:], " "),
	}, true
}

// unescapeMountPoint decodes a mount point as show_mountinfo writes it: a
// space, tab, newline or backslash becomes a backslash and three octal digits
// (mangle_path in fs/seq_file.c, called with " \t\n\\"). Any other backslash
// is malformed, since the kernel escapes every backslash it writes.
func unescapeMountPoint(s string) (string, bool) {
	if !strings.Contains(s, `\`) {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", false
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", false
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), true
}

func hasMount(mounts []mountEntry, id uint64) bool {
	for _, m := range mounts {
		if m.id == id {
			return true
		}
	}
	return false
}

// visibleMount returns the mount the directory's files are reached through.
// It walks dir from the root: at each prefix that is a mount point, the mount
// whose parent is the one visible so far covers it, and so does any mount
// stacked on that one in turn. proc(5), /proc/[pid]/mountinfo: "the top-most
// mount is the one that is not the parent of any other mount at the same
// location", and it "will be accessible only if the longest path subprefix of
// P that is a mount point is not itself hidden by a stacked mount". A mount
// whose parent is hidden is never reached, so neither line order nor the size
// of a mount ID decides.
func visibleMount(mounts []mountEntry, dir string) (mountEntry, error) {
	if !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir {
		return mountEntry{}, fmt.Errorf("%w: %s", errDirectoryNotClean, dir)
	}

	at := make(map[string][]mountEntry)
	for _, m := range mounts {
		at[m.point] = append(at[m.point], m)
	}

	// The root is the mount at "/" whose parent is no mount at "/": the
	// process's root, whose parent lies outside it or is itself. A mount
	// stacked on it is not climbed. A lookup of an absolute path starts at
	// the process's root and does not follow a mount on it: nd_jump_root
	// sets the path to the root that set_root took from the process
	// (fs/namei.c), and mounts are followed only after each component, so
	// that mount hides nothing from this process. The one lookup that does
	// follow mounts on the root, LOOKUP_DOWN, is made when a process enters
	// another mount namespace (fs/namespace.c); its root is then the upper
	// mount, and the root below is outside it and not in its table.
	var root []mountEntry
	for _, m := range at["/"] {
		if m.parent == m.id || !hasMount(at["/"], m.parent) {
			root = append(root, m)
		}
	}
	if len(root) != 1 {
		return mountEntry{}, fmt.Errorf("%w: %d root mounts", errMountUnresolved, len(root))
	}
	visible := root[0]

	for i := 1; i < len(dir)+1; i++ {
		if i < len(dir) && dir[i] != '/' {
			continue
		}
		prefix := dir[:i]
		if prefix == "/" {
			continue
		}
		next, ok, err := childOf(at[prefix], visible.id)
		if err != nil {
			return mountEntry{}, err
		}
		if !ok {
			continue
		}
		if visible, err = climb(at[prefix], next); err != nil {
			return mountEntry{}, err
		}
	}
	return visible, nil
}

// climb returns the top of the stack of mounts at one point that starts at
// base: each step is the mount whose parent is the one below it.
func climb(here []mountEntry, base mountEntry) (mountEntry, error) {
	top := base
	for range here {
		next, ok, err := childOf(here, top.id)
		if err != nil {
			return mountEntry{}, err
		}
		if !ok {
			return top, nil
		}
		top = next
	}
	return mountEntry{}, fmt.Errorf("%w: mounts at %s form a cycle", errMountUnresolved, base.point)
}

// childOf returns the mount of here whose parent is parent. Two such mounts
// at one point leave the visible one undecided, and fail.
func childOf(here []mountEntry, parent uint64) (mountEntry, bool, error) {
	var found []mountEntry
	for _, m := range here {
		if m.parent == parent && m.id != parent {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return mountEntry{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return mountEntry{}, false, fmt.Errorf("%w: %d mounts at %s on mount %d", errMountUnresolved, len(found), found[0].point, parent)
}
