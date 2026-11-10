package storage

// Probe is what a platform's probe gathers about a journal directory: the
// inputs of the platform's verdict, recorded so the verdict itself makes no
// system call. A Gather returns one for a directory; the platform's check
// turns it into the error a caller reads — a refusal, for a directory one
// of its rules rejects, that answers the caller's sentinel.
type Probe struct {
	// Dir is the directory, absolute, cleaned and with its symbolic links
	// resolved.
	Dir string
	// Mountinfo is the text of /proc/self/mountinfo.
	Mountinfo string
	// FSType is statfs's filesystem type for dir, read as 32 bits. The
	// kernel's f_type is a __statfs_word, 32 bits on a 32-bit kernel
	// (include/uapi/asm-generic/statfs.h), and golang.org/x/sys declares
	// Statfs_t.Type as an int32 on 386, arm and mips, where Btrfs's magic
	// arrives negative. Every magic fits in 32 bits, so the probe converts
	// with uint32(st.Type) and the verdict sees the same value on every
	// architecture.
	FSType uint32
	// DirFlags is FS_IOC_GETFLAGS's answer for dir. The verdict reads it and
	// files only on Btrfs, once DirIno is a subvolume root's, so the probe
	// asks for them only there: the ioctl fails on the empty directory a
	// snapshot leaves for a nested subvolume, and on some filesystems outside
	// the list, which the verdict passes.
	DirFlags uint32
	// DirIno is dir's inode number.
	DirIno uint64
	// MountID is the ID of the mount holding dir, as statx reports it, or 0
	// where the kernel does not report it. The probe must ask for
	// STATX_MNT_ID, never STATX_MNT_ID_UNIQUE: statx fills the same field
	// with mnt_id for the first and mnt_id_unique for the second
	// (vfs_statx_path in fs/stat.c), mountinfo's first field is mnt_id
	// (show_mountinfo in fs/proc_namespace.c), and unique IDs start above
	// MNT_UNIQUE_ID_OFFSET, 1 << 31 (fs/namespace.c). With the second flag
	// no ID would match the table, and every directory would be refused.
	MountID uint64
	// Files is every file already in dir, with FS_IOC_GETFLAGS's answer for
	// each.
	Files []ProbedFile
}

// ProbedFile is one file already in the journal directory.
type ProbedFile struct {
	Name  string
	Flags uint32
}

// Gather gathers the verdict's inputs for a directory: statfs's answer, the
// mount table's text, the directory's attribute and its files' — every read
// that platform's verdict takes as its inputs. An error it returns is the
// check's, returned as itself and never as a refusal: a gather that could not
// read says nothing about the directory's suitability.
type Gather func(dir string) (Probe, error)
